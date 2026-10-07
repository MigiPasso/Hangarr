package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// Library benchmarks: the "Who?" perspective computes states on the fly
// from one user's plays (correlated subqueries per series). Run with
//
//	docker compose -f compose.dev.yml --profile tools run --rm gotools \
//	  go test -run '^$' -bench LibraryView -benchmem ./internal/store
//
// make test does not run them.

// benchLibrary is the size of a synthetic library.
type benchLibrary struct {
	name           string
	series, movies int
	users, events  int
}

var benchSizes = []benchLibrary{
	{"small", 300, 1_000, 5, 30_000},
	{"medium", 1_000, 3_000, 10, 150_000},
	{"large", 3_000, 10_000, 30, 600_000},
}

// benchNow is the fixed "now" of the benchmarks.
var benchNow = time.Unix(1_800_000_000, 0)

// seedLibrary fills s with a deterministic library: series of 1 to 10
// seasons, users watching their series episode after episode (finished,
// in progress or dropped series), movies watched in full or partially, over
// the last two years. It returns the most active user.
func seedLibrary(tb testing.TB, s *Store, size benchLibrary) int64 {
	tb.Helper()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(1, 2))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	prep := func(q string) *sql.Stmt {
		st, err := tx.PrepareContext(ctx, q)
		if err != nil {
			tb.Fatal(err)
		}
		return st
	}
	insMedia := prep(`INSERT INTO media(id, kind, arr_id, plex_rating_key, title, title_norm, year, status, genres,
		size_bytes, added_at, tags, total_episodes, downloaded_episodes, match_state, play_count, watch_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '["Drama"]', ?, ?, '[]', ?, ?, 'matched', ?, ?)`)
	defer func() { _ = insMedia.Close() }()
	insSeason := prep(`INSERT INTO seasons(media_id, season_number, episode_file_count, total_episode_count, size_bytes)
		VALUES (?, ?, ?, ?, ?)`)
	defer func() { _ = insSeason.Close() }()
	insEvent := prep(`INSERT INTO watch_events(source, source_id, media_id, media_type, rating_key, season_number,
		episode_number, user_id, started_at, stopped_at, play_seconds, percent_complete, watched)
		VALUES ('tautulli', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	defer func() { _ = insEvent.Close() }()
	insUser := prep(`INSERT INTO plex_accounts(account_id, username, is_active) VALUES (?, ?, 1)`)
	defer func() { _ = insUser.Close() }()

	states := []string{"never", "in_progress", "completed", "abandoned"}
	type show struct {
		id       int64
		episodes [][2]int // season, episode with a file, in order
	}
	shows := make([]show, size.series)
	for i := range size.series {
		id := int64(i + 1)
		sh := show{id: id}
		total := 0
		var seasons [][3]int                    // number, files, episodes
		for sn := 0; sn <= 1+r.IntN(10); sn++ { // season 0 = specials
			eps := 6 + r.IntN(8)
			files := eps
			if r.IntN(10) == 0 {
				files = r.IntN(eps) // season partly downloaded
			}
			seasons = append(seasons, [3]int{sn, files, eps})
			if sn > 0 {
				total += files
				for e := 1; e <= files; e++ {
					sh.episodes = append(sh.episodes, [2]int{sn, e})
				}
			}
		}
		title := fmt.Sprintf("Series %d", i)
		if _, err := insMedia.Exec(id, "series", id, fmt.Sprint(100_000+id), title, strings.ToLower(title), 1990+r.IntN(35),
			"ended", int64(total)*1_500_000_000, benchNow.Unix()-r.Int64N(3*365*86400), total, total,
			0, states[r.IntN(len(states))]); err != nil {
			tb.Fatal(err)
		}
		for _, se := range seasons {
			if _, err := insSeason.Exec(id, se[0], se[1], se[2], int64(se[1])*1_500_000_000); err != nil {
				tb.Fatal(err)
			}
		}
		shows[i] = sh
	}
	for i := range size.movies {
		id := int64(size.series + i + 1)
		title := fmt.Sprintf("Movie %d", i)
		if _, err := insMedia.Exec(id, "movie", int64(i+1), fmt.Sprint(100_000+id), title, strings.ToLower(title), 1960+r.IntN(65),
			"released", 8_000_000_000+r.Int64N(30_000_000_000), benchNow.Unix()-r.Int64N(3*365*86400), nil, nil,
			0, states[r.IntN(len(states))]); err != nil {
			tb.Fatal(err)
		}
	}

	// Users: user 1001 is the most active, then fewer plays for each one.
	weights := make([]float64, size.users)
	sum := 0.0
	for u := range size.users {
		weights[u] = 1 / float64(u+1)
		sum += weights[u]
		if _, err := insUser.Exec(1001+u, fmt.Sprintf("user%d", u)); err != nil {
			tb.Fatal(err)
		}
	}
	pickUser := func() int {
		x := r.Float64() * sum
		for u, w := range weights {
			if x -= w; x <= 0 {
				return u
			}
		}
		return size.users - 1
	}
	// Each user follows a few series at a time, episode after episode, and
	// sometimes drops one.
	type progress struct{ show, next int }
	following := make([][]progress, size.users)
	for n := range size.events {
		u := pickUser()
		at := benchNow.Unix() - r.Int64N(2*365*86400)
		if r.IntN(100) < 40 { // a movie
			m := size.series + 1 + r.IntN(size.movies)
			watched := r.IntN(100) < 85
			pct := 100
			if !watched {
				pct = 10 + r.IntN(70)
			}
			if _, err := insEvent.Exec(n+1, m, "movie", fmt.Sprint(m), nil, nil, 1001+u, at-7200, at, 72*pct, pct, watched); err != nil {
				tb.Fatal(err)
			}
			continue
		}
		f := following[u]
		if len(f) < 3 {
			f = append(f, progress{show: r.IntN(size.series)})
		}
		k := r.IntN(len(f))
		p := &f[k]
		sh := shows[p.show]
		if p.next >= len(sh.episodes) || r.IntN(200) == 0 { // finished or dropped
			f = append(f[:k], f[k+1:]...)
			following[u] = f
			continue
		}
		ep := sh.episodes[p.next]
		p.next++
		following[u] = f
		if _, err := insEvent.Exec(n+1, sh.id, "episode", fmt.Sprint(n+1), ep[0], ep[1], 1001+u, at-2700, at, 2700, 100, true); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `ANALYZE`); err != nil {
		tb.Fatal(err)
	}
	return 1001
}

func BenchmarkLibraryView(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			s, err := Open(context.Background(), b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = s.Close() })
			user := seedLibrary(b, s, size)
			var events int
			_ = s.db.QueryRow(`SELECT COUNT(*) FROM watch_events WHERE user_id = ?`, user).Scan(&events)
			b.Logf("%s: %d series, %d movies, %d users, %d events (%d for the user)",
				size.name, size.series, size.movies, size.users, size.events, events)
			cases := []struct {
				name string
				f    LibraryFilter
			}{
				{"everyone", LibraryFilter{Kind: "series", Sort: "title"}},
				{"user", LibraryFilter{Kind: "series", Sort: "title", User: user}},
				{"user_state", LibraryFilter{Kind: "series", Sort: "title", User: user, States: []string{"in_progress"}}},
				{"user_sort_last", LibraryFilter{Kind: "series", Sort: "last", Desc: true, User: user}},
				{"user_movies", LibraryFilter{Kind: "movie", Sort: "title", User: user}},
			}
			for _, c := range cases {
				c.f.Limit = 49 // one page (DefaultPageSize + 1)
				b.Run(c.name, func(b *testing.B) {
					ctx := context.Background()
					for b.Loop() {
						// What a page load runs: the page and its totals.
						if _, err := s.LibraryPage(ctx, c.f, benchNow, 90); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
			if size.name == "medium" {
				q, args := LibraryQuery(cases[1].f, benchNow, 90)
				rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+q, args...)
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = rows.Close() }()
				var plan []string
				for rows.Next() {
					var id, parent, unused int
					var detail string
					if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
						b.Fatal(err)
					}
					plan = append(plan, detail)
				}
				if err := rows.Err(); err != nil {
					b.Fatal(err)
				}
				b.Logf("query plan (user view):\n  %s", strings.Join(plan, "\n  "))
			}
		})
	}
}
