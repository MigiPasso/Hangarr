package syncer

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"hangarr/internal/clients/httpx"
	"hangarr/internal/clients/plex"
	"hangarr/internal/clients/tautulli"
	"hangarr/internal/matching"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// --- History -----------------------------------------------------------------

func intPtr(n int) *int { return &n }

// TautulliEvent converts a history row; ok is false for rows that are not
// stored (no id yet, or not a movie/episode).
func TautulliEvent(r tautulli.HistoryRow) (store.WatchEvent, bool) {
	if r.ID <= 0 || (r.MediaType != "movie" && r.MediaType != "episode") {
		return store.WatchEvent{}, false
	}
	e := store.WatchEvent{
		Source: SourceTautulli, SourceID: int64(r.ID), MediaType: r.MediaType,
		RatingKey: string(r.RatingKey), ParentRatingKey: string(r.ParentRatingKey),
		GrandparentRatingKey: string(r.GrandparentRatingKey), UserID: int64(r.UserID),
		StartedAt: int64(r.Started), StoppedAt: int64(r.Stopped), PlaySeconds: r.PlaySeconds(),
		PercentComplete: int(r.PercentComplete), Watched: r.Watched(), Platform: r.Platform, Player: r.Player,
		TranscodeDecision: r.TranscodeDecision, Title: r.Title, GrandparentTitle: r.GrandparentTitle,
		Year: int(r.Year),
	}
	if e.StartedAt == 0 {
		e.StartedAt = int64(r.Date)
	}
	if e.StoppedAt == 0 {
		e.StoppedAt = e.StartedAt
	}
	if r.ReferenceID > 0 {
		ref := int64(r.ReferenceID)
		e.ReferenceID = &ref
	}
	if r.MediaType == "episode" {
		e.SeasonNumber, e.EpisodeNumber = intPtr(int(r.ParentMediaIndex)), intPtr(int(r.MediaIndex))
	}
	return e, true
}

// syncTautulliHistory pages from newest to oldest and stops once a whole
// page is older than the cursor minus 24 h; the overlap catches long
// sessions that finished late, and the upsert keeps it idempotent.
func (s *Syncer) syncTautulliHistory(ctx context.Context, c *tautulli.Client, users []tautulli.User, v *viewers) (int, error) {
	var accounts []store.PlexAccount
	if v != nil {
		accounts = v.accounts
	} else {
		for _, u := range users {
			if u.UserID == 0 {
				continue
			}
			accounts = append(accounts, store.PlexAccount{AccountID: int64(u.UserID), Username: u.Username,
				FriendlyName: u.FriendlyName, IsActive: u.IsActive == 1, ThumbURL: u.Thumb})
		}
	}
	if err := s.Store.UpsertPlexAccounts(ctx, accounts); err != nil {
		return 0, err
	}
	if v != nil {
		n, err := s.Store.KeepViewers(ctx, v.keep())
		if err != nil {
			return 0, err
		}
		if n > 0 {
			s.Log.Info("plays of users unknown to Tautulli dropped", "deleted_events", n)
		}
	}
	pageSize := tautulli.HistoryPageSize
	if s.HistoryPageSize > 0 {
		pageSize = s.HistoryPageSize
	}
	return s.pageHistory(ctx, cursorTautulli, func(start int) (historyPage, error) {
		p, err := c.History(ctx, start, pageSize)
		if err != nil {
			return historyPage{}, err
		}
		out := historyPage{dates: make([]int64, 0, len(p.Rows)), rows: len(p.Rows), total: int(p.RecordsFiltered)}
		for i, r := range p.Rows {
			if i == 0 {
				out.first = fmt.Sprintf("%d/%d/%d", int64(r.ID), int64(r.ReferenceID), int64(r.Date))
			}
			out.dates = append(out.dates, int64(r.Date))
			if e, ok := TautulliEvent(r); ok && v.allows(e.UserID) {
				out.events = append(out.events, e)
			}
		}
		return out, nil
	}, pageSize)
}

// viewers is the viewer list of merged mode: the Tautulli users. The Plex
// server accounts are matched to them by name (the server owner is 1 for
// Plex and its plex.tv id for Tautulli); Plex accounts Tautulli does not
// know are left out, with their plays.
type viewers struct {
	ids      map[int64]int64 // Plex account id → Tautulli user id
	accounts []store.PlexAccount
}

func newViewers(accs []plex.Account, users []tautulli.User) *viewers {
	v := &viewers{ids: map[int64]int64{}}
	byName := map[string]int64{}
	for _, u := range users {
		if u.UserID == 0 {
			continue
		}
		v.accounts = append(v.accounts, store.PlexAccount{AccountID: int64(u.UserID), Username: u.Username,
			FriendlyName: u.FriendlyName, IsActive: u.IsActive == 1, ThumbURL: u.Thumb})
		if u.FriendlyName != "" {
			byName[strings.ToLower(u.FriendlyName)] = int64(u.UserID)
		}
	}
	for _, u := range users { // the user name wins over a display name
		if u.UserID != 0 && u.Username != "" {
			byName[strings.ToLower(u.Username)] = int64(u.UserID)
		}
	}
	for _, a := range accs {
		if a.ID <= 0 || a.Name == "" {
			continue
		}
		if tid, ok := byName[strings.ToLower(a.Name)]; ok {
			v.ids[int64(a.ID)] = tid
		}
	}
	return v
}

func (v *viewers) keep() []int64 {
	out := make([]int64, 0, len(v.accounts))
	for _, a := range v.accounts {
		out = append(out, a.AccountID)
	}
	return out
}

// allows reports whether the plays of a stored user id are kept (always
// without a viewer list).
func (v *viewers) allows(id int64) bool {
	if v == nil {
		return true
	}
	for _, a := range v.accounts {
		if a.AccountID == id {
			return true
		}
	}
	return false
}

// syncHistories reads the history sources of up, one after the other:
// Tautulli, then the Plex history (merged mode: both, Plex ids translated to
// Tautulli's), or the Jellyfin history. In a single-source mode the other
// source is dropped first.
// Each source is recorded in sync_runs.
func (s *Syncer) syncHistories(ctx context.Context, up *upstream.Clients) []SourceResult {
	var out []SourceResult
	step := func(source string, fn func() (int, error)) {
		start := s.now()
		id, err := s.Store.StartSyncRun(ctx, source, start)
		if err != nil {
			out = append(out, SourceResult{Source: source, Err: err})
			return
		}
		t := time.Now()
		n, err := fn()
		if ferr := s.Store.FinishSyncRun(context.WithoutCancel(ctx), id, s.now(), n, err); ferr != nil {
			s.Log.Error("sync run not recorded", "source", source, "err", ferr)
		}
		out = append(out, SourceResult{Source: source, Items: n, Duration: time.Since(t), Err: err})
	}
	// Merged mode: the viewer list is Tautulli's, Plex ids are translated.
	var v *viewers
	if up.Merged() && up.HistoryClient() != nil && up.Plex != nil {
		users, err := up.HistoryClient().Users(ctx)
		var accs []plex.Account
		if err == nil {
			accs, err = up.Plex.Accounts(ctx)
		}
		if err != nil {
			return []SourceResult{{Source: SourceTautulli, Err: fmt.Errorf("viewers: %w", err)}}
		}
		v = newViewers(accs, users)
	}
	if c := up.HistoryClient(); c != nil {
		step(SourceTautulli, func() (int, error) {
			if !up.Merged() {
				if err := s.keepHistory(ctx, SourceTautulli, cursorPlexHistory); err != nil {
					return 0, err
				}
				users, err := c.Users(ctx)
				if err != nil {
					return 0, err
				}
				return s.syncTautulliHistory(ctx, c, users, nil)
			}
			return s.syncTautulliHistory(ctx, c, nil, v)
		})
	}
	if up.PlexHistory() {
		step(SourcePlexHistory, func() (int, error) {
			if !up.Merged() {
				if err := s.keepHistory(ctx, SourcePlexHistory, cursorTautulli); err != nil {
					return 0, err
				}
			}
			return s.syncPlexHistory(ctx, up.Plex, v)
		})
	}
	if up.JellyfinHistory() {
		step(SourceJellyfinHistory, func() (int, error) { return s.syncJellyfinHistory(ctx, up.Jellyfin) })
	}
	return out
}

// mergeHistory merges the Plex history into Tautulli's (merged mode), then
// the Jellyfin history into both (whatever the media server: the history of
// the other one is kept), once the events are linked to media.
func (s *Syncer) mergeHistory(ctx context.Context, up *upstream.Clients) error {
	if up.Merged() {
		st, err := s.Store.MergePlexHistory(ctx)
		if err != nil {
			return err
		}
		s.Log.Info("history merged", "plex_entries_merged", st.Merged, "sessions_completed_by_plex", st.Completed,
			"plex_only_plays_estimated", st.Estimated)
	}
	st, err := s.Store.MergeServerHistories(ctx)
	if err != nil {
		return err
	}
	if st.Merged > 0 || st.Completed > 0 {
		s.Log.Info("jellyfin history merged with plex", "jellyfin_entries_merged", st.Merged,
			"plays_completed_by_jellyfin", st.Completed)
	}
	return nil
}

// keepHistory makes source the only history in the database: the events of
// the other source are deleted (after switching from Plex history, which
// has no durations, to Tautulli, both would otherwise count every play).
func (s *Syncer) keepHistory(ctx context.Context, source, otherCursor string) error {
	n, err := s.Store.DropOtherHistory(ctx, source, otherCursor)
	if err != nil {
		return err
	}
	if n > 0 {
		s.Log.Info("history from the previous source dropped", "kept", source, "deleted_events", n)
	}
	return nil
}

// PlexEvent converts a Plex history entry (Plex only records watched items).
func PlexEvent(h plex.HistoryEntry) (store.WatchEvent, bool) {
	if h.ID() <= 0 || (h.Type != "movie" && h.Type != "episode") {
		return store.WatchEvent{}, false
	}
	e := store.WatchEvent{
		Source: SourcePlexHistory, SourceID: h.ID(), MediaType: h.Type, RatingKey: string(h.RatingKey),
		ParentRatingKey: plex.KeyID(h.ParentKey), GrandparentRatingKey: plex.KeyID(h.GrandparentKey),
		UserID: int64(h.AccountID), StartedAt: int64(h.ViewedAt), StoppedAt: int64(h.ViewedAt),
		PercentComplete: 100, Watched: true, Title: h.Title, GrandparentTitle: h.GrandparentTitle, Year: int(h.Year),
	}
	if h.Type == "episode" {
		e.SeasonNumber, e.EpisodeNumber = intPtr(int(h.ParentIndex)), intPtr(int(h.Index))
	}
	return e, true
}

// syncPlexHistory reads the Plex history. In merged mode (v set) the
// accounts are already stored and the Plex ids are translated to the stored
// ones.
func (s *Syncer) syncPlexHistory(ctx context.Context, c *plex.Client, v *viewers) (int, error) {
	ids := map[int64]int64{}
	if v != nil {
		ids = v.ids
	} else {
		accs, err := c.Accounts(ctx)
		if err != nil {
			return 0, err
		}
		accounts := make([]store.PlexAccount, 0, len(accs))
		for _, a := range accs {
			if a.ID > 0 && a.Name != "" {
				accounts = append(accounts, store.PlexAccount{AccountID: int64(a.ID), Username: a.Name, IsActive: true})
			}
		}
		if err := s.Store.UpsertPlexAccounts(ctx, accounts); err != nil {
			return 0, err
		}
	}
	// Entries stored before the translation existed (or before Tautulli).
	if err := s.Store.RemapHistoryUsers(ctx, SourcePlexHistory, ids); err != nil {
		return 0, err
	}
	return s.pageHistory(ctx, cursorPlexHistory, func(start int) (historyPage, error) {
		entries, total, err := c.History(ctx, start)
		if err != nil {
			return historyPage{}, err
		}
		var events []store.WatchEvent
		dates := make([]int64, 0, len(entries))
		first := ""
		if len(entries) > 0 {
			first = entries[0].HistoryKey
		}
		for _, h := range entries {
			dates = append(dates, int64(h.ViewedAt))
			if e, ok := PlexEvent(h); ok {
				tid, ok := ids[e.UserID]
				if ok {
					e.UserID = tid
				} else if v != nil {
					continue // a Plex account Tautulli does not know
				}
				events = append(events, e)
			}
		}
		return historyPage{events: events, dates: dates, rows: len(entries), total: total, first: first}, nil
	}, plex.HistoryPageSize)
}

// historyPage is a page of history: events to store, the date of every row,
// the raw row count, the total reported by the server (0 if unknown) and the
// identity of the first row (to detect a server that ignores the offset).
type historyPage struct {
	events []store.WatchEvent
	dates  []int64
	rows   int
	total  int
	first  string
}

// pageFunc fetches the page starting at start.
type pageFunc func(start int) (historyPage, error)

func (s *Syncer) pageHistory(ctx context.Context, cursorName string, fetch pageFunc, pageSize int) (int, error) {
	raw, err := s.Store.Cursor(ctx, cursorName)
	if err != nil {
		return 0, err
	}
	cursor, _ := strconv.ParseInt(raw, 10, 64)
	threshold := cursor - historyOverlapSecs
	maxDate := cursor
	stored, start := 0, 0
	var guard httpx.PageGuard
	for {
		p, err := fetch(start)
		if err != nil {
			return stored, err
		}
		if err := guard.Next(p.first); err != nil {
			return stored, err
		}
		events, dates, rows, total := p.events, p.dates, p.rows, p.total
		if err := s.Store.UpsertWatchEvents(ctx, events); err != nil {
			return stored, err
		}
		stored += len(events)
		allOld := len(dates) > 0
		for _, d := range dates {
			maxDate = max(maxDate, d)
			if d >= threshold {
				allOld = false
			}
		}
		start += rows
		if cursor == 0 {
			if total > 0 {
				s.setProgress("history: %d / %d", start, total)
			} else {
				s.setProgress("history: %d", start)
			}
		}
		if (cursor > 0 && allOld) || rows < pageSize || (total > 0 && start >= total) {
			break
		}
	}
	if maxDate > cursor {
		if err := s.Store.SetCursor(ctx, cursorName, strconv.FormatInt(maxDate, 10), s.now()); err != nil {
			return stored, err
		}
	}
	return stored, nil
}

// linkEvents resolves media for every event: Plex rating keys first, then
// normalized title (+ year) for keys that no longer exist.
func (s *Syncer) linkEvents(ctx context.Context) error {
	if err := s.Store.LinkWatchEventsByKey(ctx); err != nil {
		return err
	}
	titles, err := s.Store.UnlinkedEventTitles(ctx)
	if err != nil || len(titles) == 0 {
		return err
	}
	media, err := s.Store.ListMediaTitles(ctx)
	if err != nil {
		return err
	}
	cands := make([]matching.TitleCandidate, 0, len(media))
	for _, m := range media {
		cands = append(cands, matching.TitleCandidate{MediaID: m.ID, Kind: m.Kind, Title: m.Title, Year: m.Year})
	}
	idx := matching.NewTitleIndex(cands)
	var links []store.TitleLink
	for _, t := range titles {
		var id int64
		if t.MediaType == "episode" {
			// Tautulli's year on episode rows is not reliably the show's year:
			// try the title alone first, the year only breaks ties.
			if id = idx.Find(matching.KindSeries, t.Title, 0); id == 0 {
				id = idx.Find(matching.KindSeries, t.Title, t.Year)
			}
		} else {
			id = idx.Find(matching.KindMovie, t.Title, t.Year)
		}
		if id != 0 {
			links = append(links, store.TitleLink{UnlinkedTitle: t, MediaID: id})
		}
	}
	return s.Store.LinkWatchEventsByTitle(ctx, links)
}
