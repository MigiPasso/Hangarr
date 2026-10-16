package store

import (
	"testing"
)

func (e *serverEnv) protectedLocal(title string) bool {
	e.t.Helper()
	var p bool
	if err := e.s.db.QueryRow(`SELECT protected_local FROM media WHERE title = ?`, title).Scan(&p); err != nil {
		e.t.Fatalf("%s: %v", title, err)
	}
	return p
}

func (e *serverEnv) journaled(action string) []Action {
	e.t.Helper()
	all, _, err := e.s.Actions(e.ctx, 0, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []Action
	for _, a := range all {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

// BUG-01: a movie protected in Hangarr, then imported in Radarr and matched
// by TMDB id: the managed row inherits the protection, the item is not a
// cleanup candidate, and the transfer is journaled.
func TestLocalProtectionFollowsItemIntoRadarr(t *testing.T) {
	for _, tc := range []struct {
		name   string
		only   ServerOnlyItem
		arr    CatalogItem
		method string
	}{
		{"by id", ServerOnlyItem{Kind: "movie", Key: "300", Title: "Arrival", Year: 2016, TmdbID: 329865},
			CatalogItem{Kind: "movie", ArrID: 9, Title: "Arrival", Year: 2016, TmdbID: 329865, SizeBytes: 5e9, HasFile: true}, "tmdb"},
		{"by title", ServerOnlyItem{Kind: "movie", Key: "300", Title: "Arrival", Year: 2016},
			CatalogItem{Kind: "movie", ArrID: 9, Title: "Arrival", Year: 2016, SizeBytes: 5e9, HasFile: true}, "fuzzy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newServerEnv(t)
			e.apply(ServerPlex, []ServerLink{{MediaID: e.movie, Key: "101", Method: "tmdb"}, {MediaID: e.show, Key: "201", Method: "tvdb"}},
				[]ServerOnlyItem{tc.only}, nil)
			if err := e.s.SetLocalProtection(e.ctx, e.id("Arrival"), true, "admin", e.now); err != nil {
				t.Fatal(err)
			}
			// Radarr now manages it.
			if err := e.s.ReplaceArrCatalog(e.ctx, "movie", []CatalogItem{
				{Kind: "movie", ArrID: 1, Title: "Inception", Year: 2010}, tc.arr}, e.now); err != nil {
				t.Fatal(err)
			}
			var managed int64
			if err := e.s.db.QueryRow(`SELECT id FROM media WHERE kind = 'movie' AND arr_id = 9`).Scan(&managed); err != nil {
				t.Fatal(err)
			}
			e.apply(ServerPlex, []ServerLink{{MediaID: e.movie, Key: "101", Method: "tmdb"}, {MediaID: e.show, Key: "201", Method: "tvdb"},
				{MediaID: managed, Key: "300", Method: tc.method}}, nil, nil)
			var rows, protected int
			_ = e.s.db.QueryRow(`SELECT COUNT(*), SUM(protected_local) FROM media WHERE title = 'Arrival'`).Scan(&rows, &protected)
			if rows != 1 || protected != 1 {
				t.Fatalf("rows=%d protected_local rows=%d", rows, protected)
			}
			items, _, err := e.s.CleanupCandidates(e.ctx, CleanupScope{Cutoff: e.now.Unix() + 1}, CleanupFilter{}, "", true, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, it := range items {
				if it.Title == "Arrival" {
					t.Error("protected item proposed for cleanup")
				}
			}
			if got := e.journaled(ProtectCarried); len(got) != 1 || got[0].Title != "Arrival" || got[0].ArrID != 9 {
				t.Errorf("carried over not journaled: %+v", got)
			}
			// Removing it from the managed row works.
			if err := e.s.SetLocalProtection(e.ctx, managed, false, "admin", e.now); err != nil {
				t.Fatal(err)
			}
			if e.protectedLocal("Arrival") {
				t.Error("unprotect left the protection")
			}
			items, _, _ = e.s.CleanupCandidates(e.ctx, CleanupScope{Cutoff: e.now.Unix() + 1}, CleanupFilter{}, "", true, 100)
			found := false
			for _, it := range items {
				found = found || it.Title == "Arrival"
			}
			if !found {
				t.Error("unprotected item must be a candidate again (the check above would prove nothing)")
			}
		})
	}
}

// BUG-01: Plex gives the item a new rating key: the new server_only row is
// still protected (by id, or by title and year without ids).
func TestLocalProtectionSurvivesNewServerKey(t *testing.T) {
	e := newServerEnv(t)
	links := []ServerLink{{MediaID: e.movie, Key: "101", Method: "tmdb"}, {MediaID: e.show, Key: "201", Method: "tvdb"}}
	e.apply(ServerPlex, links, []ServerOnlyItem{
		{Kind: "movie", Key: "105", Title: "Vacances 2019", Year: 2019},
		{Kind: "movie", Key: "300", Title: "Arrival", Year: 2016, TmdbID: 329865},
	}, nil)
	for _, title := range []string{"Vacances 2019", "Arrival"} {
		if err := e.s.SetLocalProtection(e.ctx, e.id(title), true, "admin", e.now); err != nil {
			t.Fatal(err)
		}
	}
	e.apply(ServerPlex, links, []ServerOnlyItem{
		{Kind: "movie", Key: "905", Title: "Vacances 2019", Year: 2019},
		{Kind: "movie", Key: "999", Title: "Arrival (2016)", Year: 2016, TmdbID: 329865},
		{Kind: "movie", Key: "906", Title: "Vacances 2020", Year: 2020},
	}, nil)
	if _, pk, _ := e.row("Vacances 2019"); pk != "905" || !e.protectedLocal("Vacances 2019") {
		t.Errorf("Vacances 2019 (new key %s): protection lost", pk)
	}
	if !e.protectedLocal("Arrival (2016)") {
		t.Error("Arrival (new key, same TMDB id): protection lost")
	}
	if e.protectedLocal("Vacances 2020") {
		t.Error("another item got protected")
	}
	if got := e.journaled(ProtectUnmatched); len(got) != 0 {
		t.Errorf("nothing was lost: %+v", got)
	}
}

// An item gone from the server keeps its protection, journaled once, and
// finds it again when it comes back.
func TestLocalProtectionUnmatchedThenBack(t *testing.T) {
	e := newServerEnv(t)
	links := []ServerLink{{MediaID: e.movie, Key: "101", Method: "tmdb"}, {MediaID: e.show, Key: "201", Method: "tvdb"}}
	if err := e.s.SetLocalProtection(e.ctx, e.id("Vacances 2019"), true, "admin", e.now); err != nil {
		t.Fatal(err)
	}
	e.apply(ServerPlex, links, nil, nil)
	e.apply(ServerPlex, links, nil, nil)
	if got := e.journaled(ProtectUnmatched); len(got) != 1 || got[0].Title != "Vacances 2019" {
		t.Errorf("unmatched: %+v", got)
	}
	e.apply(ServerPlex, links, []ServerOnlyItem{{Kind: "movie", Key: "105", Title: "Vacances 2019", Year: 2019}}, nil)
	if !e.protectedLocal("Vacances 2019") {
		t.Error("protection not found again")
	}
}

// Upgrading a database at version 8: rows protected in Hangarr get their
// local_protections entry.
func TestMigration0009KeepsLocalProtection(t *testing.T) {
	e := newServerEnv(t)
	for _, q := range []string{
		`UPDATE media SET protected_local = 1 WHERE title = 'Vacances 2019'`,
		`DROP TABLE local_protections`,
		`DELETE FROM schema_migrations WHERE version = 9`,
	} {
		if _, err := e.s.db.ExecContext(e.ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := e.s.migrate(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.apply(ServerPlex, []ServerLink{{MediaID: e.movie, Key: "101", Method: "tmdb"}, {MediaID: e.show, Key: "201", Method: "tvdb"}},
		[]ServerOnlyItem{{Kind: "movie", Key: "777", Title: "Vacances 2019", Year: 2019}}, nil)
	if !e.protectedLocal("Vacances 2019") {
		t.Error("protection lost by the migration")
	}
}
