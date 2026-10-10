package web

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"hangarr/internal/actions"
	"hangarr/internal/store"
)

// batchTimeout bounds a batch request (items are processed one by one).
const batchTimeout = 10 * time.Minute

// errTooManyItems is returned for a selection above actions.MaxBatch.
var errTooManyItems = errors.New("too many items")

// batchEntry is a selected item in the confirmation.
type batchEntry struct {
	ID        int64
	Kind      string
	Title     string
	SizeBytes int64
	Reason    string // why it is excluded (English key)
	Args      []any  // arguments of the Reason key
}

type batchData struct {
	Action          string // delete | protect
	Items           []batchEntry
	Excluded        []batchEntry
	TotalBytes      int64
	HasSeries       bool
	ConfirmByTyping bool // type the number of items (whole series in the batch)
	DryRun          bool
	DeleteFiles     bool
	ImportExclusion bool
	Error           string // English key, translated by the template

	Report    []actions.BatchItem
	Freed     int64
	OKCount   int
	FailCount int
}

// selectedIDs reads the "ids" form values: valid, distinct, at most MaxBatch.
func selectedIDs(r *http.Request) ([]int64, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	var ids []int64
	for _, v := range r.PostForm["ids"] {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("invalid id")
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) > actions.MaxBatch {
		return nil, errTooManyItems
	}
	return ids, nil
}

// classify splits the selection into eligible and excluded items.
func (s *Server) classify(r *http.Request, ids []int64, protectAction bool) (*batchData, error) {
	d := &batchData{DryRun: s.Config.DryRun}
	up := s.Upstreams.Get()
	for _, id := range ids {
		m, err := s.Store.MediaByID(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			d.Excluded = append(d.Excluded, batchEntry{ID: id, Title: fmt.Sprintf("#%d", id), Reason: "not found"})
			continue
		}
		if err != nil {
			return nil, err
		}
		e := batchEntry{ID: m.ID, Kind: m.Kind, Title: m.Title, SizeBytes: m.SizeBytes}
		switch {
		case m.ArrID == 0:
			e.Reason, e.Args = "in %s only", []any{up.ServerLabel()}
		case store.HasProtectedTag(m.Tags, s.Config.ProtectedTags):
			e.Reason = "already protected"
			if !protectAction {
				e.Reason = "protected by a tag"
			}
		}
		if e.Reason != "" {
			d.Excluded = append(d.Excluded, e)
			continue
		}
		d.Items = append(d.Items, e)
		d.TotalBytes += m.SizeBytes
		d.HasSeries = d.HasSeries || m.Kind == "series"
	}
	return d, nil
}

func (s *Server) renderBatch(w http.ResponseWriter, r *http.Request, status int, name string, d *batchData) {
	title := "Delete selection"
	if d.Action == "protect" {
		title = "Protect selection"
	}
	pd := s.page(r, title, "", d)
	if isHTMX(r) {
		s.render(w, r, http.StatusOK, "batch", name, pd)
		return
	}
	s.render(w, r, status, "batch", "", pd)
}

// extendDeadline lets a batch run longer than the server WriteTimeout.
func extendDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(batchTimeout))
}

// batchDelete shows the confirmation (first POST), then runs the deletions
// (step=run) and shows the per-item report.
func (s *Server) batchDelete(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, s.loc(r).T("Deletion unavailable."), http.StatusServiceUnavailable)
		return
	}
	ids, err := selectedIDs(r)
	if err != nil || len(ids) == 0 {
		msg := "No item selected."
		if err != nil {
			msg = "Invalid selection."
		}
		s.renderBatch(w, r, http.StatusUnprocessableEntity, "batch_confirm", &batchData{Action: "delete", Error: msg})
		return
	}
	d, err := s.classify(r, ids, false)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Action = "delete"
	d.ConfirmByTyping = s.Config.ConfirmByTyping && d.HasSeries
	if r.PostFormValue("step") != "run" {
		d.DeleteFiles = true
		s.renderBatch(w, r, http.StatusOK, "batch_confirm", d)
		return
	}
	d.DeleteFiles = r.PostFormValue("delete_files") == "1"
	d.ImportExclusion = r.PostFormValue("import_exclusion") == "1"
	if len(d.Items) == 0 {
		d.Error = "No deletable item in the selection."
		s.renderBatch(w, r, http.StatusUnprocessableEntity, "batch_confirm", d)
		return
	}
	if d.ConfirmByTyping && strings.TrimSpace(r.PostFormValue("confirm_count")) != strconv.Itoa(len(d.Items)) {
		d.Error = "The number typed does not match the number of items to delete."
		s.renderBatch(w, r, http.StatusUnprocessableEntity, "batch_confirm", d)
		return
	}
	extendDeadline(w)
	eligible := make([]int64, 0, len(d.Items))
	for _, e := range d.Items {
		eligible = append(eligible, e.ID)
	}
	d.Report = s.Actions.DeleteBatch(r.Context(), eligible, actions.BatchOptions{DeleteFiles: d.DeleteFiles,
		ImportExclusion: d.ImportExclusion, Username: sessionFrom(r).Username})
	var deleted []int64
	for _, it := range d.Report {
		if it.OK {
			d.OKCount++
			d.Freed += it.SizeFreed
			if !d.DryRun {
				deleted = append(deleted, it.MediaID)
			}
		} else {
			d.FailCount++
		}
	}
	l := s.loc(r)
	msg := l.N(int64(d.OKCount), "%d item deleted, %s freed.", "%d items deleted, %s freed.", l.Bytes(d.Freed))
	if d.DryRun {
		msg = l.N(int64(d.OKCount), "Simulation: %d item would be deleted (%s). Nothing changed.",
			"Simulation: %d items would be deleted (%s). Nothing changed.", l.Bytes(d.Freed))
	}
	if d.FailCount > 0 {
		msg += " " + l.N(int64(d.FailCount), "%d failure.", "%d failures.")
	}
	s.batchTrigger(w, msg, d, deleted, false)
	s.renderBatch(w, r, http.StatusOK, "batch_report", d)
}

// batchProtect protects the selection right away (no confirmation: nothing
// is destroyed) and shows the per-item report.
func (s *Server) batchProtect(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, s.loc(r).T("Protection unavailable."), http.StatusServiceUnavailable)
		return
	}
	ids, err := selectedIDs(r)
	if err != nil || len(ids) == 0 {
		s.renderBatch(w, r, http.StatusUnprocessableEntity, "batch_report",
			&batchData{Action: "protect", Error: "No item selected."})
		return
	}
	d, err := s.classify(r, ids, true)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Action = "protect"
	extendDeadline(w)
	eligible := make([]int64, 0, len(d.Items))
	for _, e := range d.Items {
		eligible = append(eligible, e.ID)
	}
	d.Report = s.Actions.ProtectBatch(r.Context(), eligible, sessionFrom(r).Username)
	for _, it := range d.Report {
		if it.OK {
			d.OKCount++
		} else {
			d.FailCount++
		}
	}
	l := s.loc(r)
	msg := l.N(int64(d.OKCount), "%d item protected.", "%d items protected.")
	if d.DryRun {
		msg = l.N(int64(d.OKCount), "Simulation: %d item would be protected. Nothing changed.",
			"Simulation: %d items would be protected. Nothing changed.")
	}
	if d.FailCount > 0 {
		msg += " " + l.N(int64(d.FailCount), "%d failure.", "%d failures.")
	}
	s.batchTrigger(w, msg, d, nil, !d.DryRun && d.OKCount > 0)
	s.renderBatch(w, r, http.StatusOK, "batch_report", d)
}

// batchTrigger sends the toast and the list of deleted items (their cards
// are removed); reload asks the page to reload when the report is closed.
func (s *Server) batchTrigger(w http.ResponseWriter, msg string, d *batchData, deleted []int64, reload bool) {
	level := "success"
	switch {
	case d.DryRun:
		level = "info"
	case d.FailCount > 0:
		level = "error"
	}
	if deleted == nil {
		deleted = []int64{}
	}
	w.Header().Set("HX-Trigger", asciiJSON(map[string]any{
		"hangarr:toast":      map[string]any{"message": msg, "level": level},
		"hangarr:batch-done": map[string]any{"deleted": deleted, "reload": reload},
	}))
}
