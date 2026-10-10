// Package clients_test checks that the API structs decode the fixtures:
// the synthetic ones and, when present, real captures (testdata/captured).
package clients_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hangarr/internal/clients/jellyfin"
	"hangarr/internal/clients/plex"
	"hangarr/internal/clients/radarr"
	"hangarr/internal/clients/sonarr"
	"hangarr/internal/clients/tautulli"
)

func fixtureDirs(t *testing.T) []string {
	dirs := []string{"../../testdata", "../../testdata/dev"}
	if _, err := os.Stat("../../testdata/captured"); err == nil {
		dirs = append(dirs, "../../testdata/captured")
	}
	return dirs
}

func decode(t *testing.T, path string, v any) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return true
}

func TestSonarrRadarrFixtures(t *testing.T) {
	for _, dir := range fixtureDirs(t) {
		var series []sonarr.Series
		if decode(t, filepath.Join(dir, "sonarr/series.json"), &series) {
			if len(series) == 0 {
				t.Errorf("%s: no series", dir)
			}
			for _, s := range series {
				if s.ID == 0 || s.Title == "" || s.TvdbID == 0 {
					t.Errorf("%s: series missing id/title/tvdbId: %+v", dir, s)
				}
			}
		}
		var movies []radarr.Movie
		if decode(t, filepath.Join(dir, "radarr/movie.json"), &movies) {
			for _, m := range movies {
				if m.ID == 0 || m.Title == "" || m.TmdbID == 0 {
					t.Errorf("%s: movie missing id/title/tmdbId: %+v", dir, m)
				}
			}
		}
		var episodes []sonarr.CalendarEpisode
		if decode(t, filepath.Join(dir, "sonarr/calendar.json"), &episodes) {
			for _, e := range episodes {
				if e.SeriesID == 0 || e.AirDateUTC.IsZero() || e.Series == nil || e.Series.Title == "" {
					t.Errorf("%s: calendar episode missing seriesId/airDateUtc/series: %+v", dir, e)
				}
			}
		}
		var releases []radarr.CalendarMovie
		if decode(t, filepath.Join(dir, "radarr/calendar.json"), &releases) {
			for _, m := range releases {
				if m.ID == 0 || m.Title == "" || (m.InCinemas.IsZero() && m.DigitalRelease.IsZero() && m.PhysicalRelease.IsZero()) {
					t.Errorf("%s: calendar movie missing id/title/release date: %+v", dir, m)
				}
			}
		}
		for _, f := range []string{"sonarr/tag.json", "radarr/tag.json"} {
			var tags []sonarr.Tag
			decode(t, filepath.Join(dir, f), &tags)
		}
	}
}

func TestSonarrDetails(t *testing.T) {
	var series []sonarr.Series
	decode(t, "../../testdata/sonarr/series.json", &series)
	bb := series[0]
	if bb.Statistics == nil || bb.Statistics.SizeOnDisk != 6e9 || len(bb.Seasons) != 3 || bb.Added.IsZero() {
		t.Fatalf("Breaking Bad: %+v", bb)
	}
	if !series[4].Added.IsZero() {
		t.Error("0001-01-01 must decode as zero time")
	}
	var movies []radarr.Movie
	decode(t, "../../testdata/radarr/movie.json", &movies)
	for i, want := range []float64{8.8, 8.0, 7.2, 8.6, 0} {
		if got := movies[i].Ratings.Best(); got != want {
			t.Errorf("%s rating = %v, want %v", movies[i].Title, got, want)
		}
	}
}

func TestPlexFixtures(t *testing.T) {
	for _, dir := range fixtureDirs(t) {
		var sections struct {
			MediaContainer struct{ Directory []plex.Section }
		}
		if !decode(t, filepath.Join(dir, "plex/sections.json"), &sections) {
			continue
		}
		for _, s := range sections.MediaContainer.Directory {
			if s.Key == "" || s.Type == "" {
				t.Errorf("%s: bad section %+v", dir, s)
			}
		}
		files, _ := filepath.Glob(filepath.Join(dir, "plex/section-*-type-*.json"))
		for _, f := range files {
			var items struct {
				MediaContainer struct{ Metadata []plex.Item }
			}
			decode(t, f, &items)
			for _, it := range items.MediaContainer.Metadata {
				if it.RatingKey == "" || it.Title == "" {
					t.Errorf("%s: item without ratingKey/title: %+v", f, it)
				}
			}
		}
	}
}

func TestPlexExternalIDs(t *testing.T) {
	cases := []struct {
		item       plex.Item
		tvdb, tmdb int64
		imdb       string
	}{
		{plex.Item{GUIDs: []plex.GUID{{ID: "imdb://tt0903747"}, {ID: "tmdb://1396"}, {ID: "tvdb://81189"}}}, 81189, 1396, "tt0903747"},
		{plex.Item{LegacyGUID: "com.plexapp.agents.imdb://tt0816692?lang=en"}, 0, 0, "tt0816692"},
		{plex.Item{LegacyGUID: "com.plexapp.agents.thetvdb://81189?lang=en"}, 81189, 0, ""},
		{plex.Item{LegacyGUID: "com.plexapp.agents.themoviedb://27205?lang=fr"}, 0, 27205, ""},
		{plex.Item{LegacyGUID: "plex://movie/abc", GUIDs: []plex.GUID{{ID: "imdb://nope"}}}, 0, 0, ""},
		{plex.Item{LegacyGUID: "local://105"}, 0, 0, ""},
	}
	for _, c := range cases {
		tvdb, tmdb, imdb := c.item.ExternalIDs()
		if tvdb != c.tvdb || tmdb != c.tmdb || imdb != c.imdb {
			t.Errorf("%+v: got %d %d %q", c.item, tvdb, tmdb, imdb)
		}
	}
	if plex.KeyID("/library/metadata/201") != "201" || plex.KeyID("") != "" {
		t.Error("KeyID")
	}
	h := plex.HistoryEntry{HistoryKey: "/status/sessions/history/904"}
	if h.ID() != 904 {
		t.Errorf("history id %d", h.ID())
	}
}

func TestTautulliFixtures(t *testing.T) {
	for _, dir := range fixtureDirs(t) {
		var env struct {
			Response struct {
				Result string
				Data   tautulli.HistoryPage
			}
		}
		if decode(t, filepath.Join(dir, "tautulli/get_history.json"), &env) {
			if env.Response.Result != "success" || len(env.Response.Data.Rows) == 0 {
				t.Errorf("%s: empty history", dir)
			}
			for _, r := range env.Response.Data.Rows {
				if r.ID > 0 && (r.Date == 0 || r.UserID == 0 && !strings.EqualFold(r.User, "local")) {
					t.Errorf("%s: row %d without date/user", dir, r.ID)
				}
			}
		}
		var users struct {
			Response struct{ Data []tautulli.User }
		}
		decode(t, filepath.Join(dir, "tautulli/get_users.json"), &users)
	}
}

func TestTautulliTolerance(t *testing.T) {
	var env struct {
		Response struct{ Data tautulli.HistoryPage }
	}
	decode(t, "../../testdata/tautulli/get_history.json", &env)
	rows := env.Response.Data.Rows
	if len(rows) != 22 || env.Response.Data.RecordsFiltered != 22 {
		t.Fatalf("rows=%d filtered=%d", len(rows), env.Response.Data.RecordsFiltered)
	}
	live := rows[0]
	if live.ID != 0 || live.ReferenceID != 0 {
		t.Errorf("live session must decode with zero ids: %+v", live)
	}
	var partial, resumed tautulli.HistoryRow
	for _, r := range rows {
		switch r.ID {
		case 9:
			partial = r
		case 10:
			resumed = r
		}
	}
	if partial.Watched() || partial.WatchedStatus != 0.5 || !resumed.Watched() || partial.ReferenceID != resumed.ReferenceID {
		t.Errorf("resumed session: %+v / %+v", partial, resumed)
	}
	var users struct {
		Response struct{ Data []tautulli.User }
	}
	decode(t, "../../testdata/tautulli/get_users.json", &users)
	if bob := users.Response.Data[2]; bob.UserID != 1002 || bob.IsActive != 1 {
		t.Errorf("string-typed numbers: %+v", bob)
	}
	noPlayDuration := tautulli.HistoryRow{Duration: 600}
	if noPlayDuration.PlaySeconds() != 600 {
		t.Error("PlaySeconds must fall back to duration")
	}
}

func TestJellyfinFixtures(t *testing.T) {
	for _, dir := range fixtureDirs(t) {
		var info jellyfin.SystemInfo
		if decode(t, filepath.Join(dir, "jellyfin/system-info.json"), &info) && info.Version == "" {
			t.Errorf("%s: system info without version", dir)
		}
		var users []jellyfin.User
		if decode(t, filepath.Join(dir, "jellyfin/users.json"), &users) {
			for _, u := range users {
				if !jellyfin.IsID(u.ID) || u.Name == "" {
					t.Errorf("%s: user without id/name: %+v", dir, u)
				}
			}
		}
		files, _ := filepath.Glob(filepath.Join(dir, "jellyfin/items-*.json"))
		for _, f := range files {
			var page struct{ Items []jellyfin.Item }
			decode(t, f, &page)
			for _, it := range page.Items {
				if !jellyfin.IsID(it.ID) || it.Name == "" || it.Type == "" || (it.SeriesID != "" && !jellyfin.IsID(it.SeriesID)) {
					t.Errorf("%s: item without valid id/name/type/seriesId: %+v", f, it)
				}
			}
		}
		for _, pattern := range []string{"played-*.json", "resumable-*.json"} {
			files, _ := filepath.Glob(filepath.Join(dir, "jellyfin", pattern))
			for _, f := range files {
				var page struct{ Items []jellyfin.Item }
				decode(t, f, &page)
				for _, it := range page.Items {
					if !jellyfin.IsID(it.ID) || it.UserData == nil || (it.SeriesID != "" && !jellyfin.IsID(it.SeriesID)) {
						t.Errorf("%s: item without valid id/seriesId or UserData: %+v", f, it)
					}
				}
			}
		}
	}
	var movies struct{ Items []jellyfin.Item }
	if !decode(t, "../../testdata/jellyfin/items-Movie.json", &movies) || len(movies.Items) == 0 || movies.Items[0].Name != "Inception" {
		t.Fatalf("synthetic Jellyfin movies: %+v", movies.Items)
	}
	if tvdb, tmdb, imdb := movies.Items[0].ExternalIDs(); tvdb != 170 || tmdb != 27205 || imdb != "tt1375666" {
		t.Errorf("Inception external ids: %d %d %q", tvdb, tmdb, imdb)
	}
}
