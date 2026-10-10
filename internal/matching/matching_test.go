package matching

import "testing"

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"The Office (US)":                 "office us",
		"Shōgun":                          "shogun",
		"Les Revenants":                   "revenants",
		"L'Amour est dans le pré":         "amour est dans le pre",
		"Marvel's Agents of S.H.I.E.L.D.": "marvel s agents of s h i e l d",
		"Law & Order":                     "law and order",
		"Dune (2021)":                     "dune",
		"The":                             "the",
		"  Été   ":                        "ete",
	}
	for in, want := range cases {
		if got := NormalizeTitle(in); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchPriorityAndFuzzy(t *testing.T) {
	arr := []ArrItem{
		{MediaID: 1, Kind: KindSeries, TvdbID: 81189, TmdbID: 1396, Title: "Breaking Bad", Year: 2008},
		{MediaID: 2, Kind: KindSeries, TmdbID: 63639, Title: "The Expanse", Year: 2015},
		{MediaID: 3, Kind: KindSeries, Title: "Shōgun", Year: 2024},
		{MediaID: 4, Kind: KindMovie, TmdbID: 27205, ImdbID: "tt1375666", Title: "Inception", Year: 2010},
		{MediaID: 5, Kind: KindMovie, ImdbID: "tt0816692", Title: "Interstellar", Year: 2014},
		{MediaID: 6, Kind: KindMovie, Title: "Remake", Year: 2020},
		{MediaID: 7, Kind: KindMovie, TmdbID: 999, Title: "Only In Radarr", Year: 2001},
	}
	plex := []ServerItem{
		{Kind: KindSeries, Key: "a", TvdbID: 81189, Title: "Breaking Bad"},
		{Kind: KindSeries, Key: "b", TmdbID: 63639, Title: "Expanse"},
		{Kind: KindSeries, Key: "c", Title: "Shogun", Year: 2024},
		{Kind: KindMovie, Key: "d", TmdbID: 27205, Title: "Inception"},
		{Kind: KindMovie, Key: "e", ImdbID: "tt0816692", Title: "Interstellar"},
		{Kind: KindMovie, Key: "f", Title: "Remake", Year: 1985}, // too far
		{Kind: KindMovie, Key: "g", Title: "Remake", Year: 2021}, // ±1
		{Kind: KindMovie, Key: "h", Title: "Home Videos"},
		// Same tvdb id as a movie must not cross kinds.
		{Kind: KindMovie, Key: "i", TmdbID: 1396, Title: "Something"},
	}
	links, plexOnly := Match(arr, plex)
	got := map[int64]Link{}
	for _, l := range links {
		got[l.MediaID] = l
	}
	want := map[int64]Link{
		1: {1, "a", MethodTVDB},
		2: {2, "b", MethodTMDB},
		3: {3, "c", MethodFuzzy},
		4: {4, "d", MethodTMDB},
		5: {5, "e", MethodIMDB},
		6: {6, "g", MethodFuzzy},
	}
	if len(got) != len(want) {
		t.Fatalf("links: %+v", links)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("media %d: got %+v want %+v", id, got[id], w)
		}
	}
	keys := map[string]bool{}
	for _, p := range plexOnly {
		keys[p.Key] = true
	}
	if len(plexOnly) != 3 || !keys["f"] || !keys["h"] || !keys["i"] {
		t.Errorf("plex only: %+v", plexOnly)
	}
}

func TestIDMatchBeatsFuzzy(t *testing.T) {
	// Arr item 1 would fuzzy-match "x", but "x" is the id match of item 2.
	arr := []ArrItem{
		{MediaID: 1, Kind: KindMovie, Title: "Twin", Year: 2000},
		{MediaID: 2, Kind: KindMovie, TmdbID: 5, Title: "Twin", Year: 2000},
	}
	plex := []ServerItem{{Kind: KindMovie, Key: "x", TmdbID: 5, Title: "Twin", Year: 2000}}
	links, _ := Match(arr, plex)
	if len(links) != 1 || links[0].MediaID != 2 || links[0].Method != MethodTMDB {
		t.Fatalf("links: %+v", links)
	}
}

func TestTitleIndex(t *testing.T) {
	idx := NewTitleIndex([]TitleCandidate{
		{MediaID: 1, Kind: KindMovie, Title: "Inception", Year: 2010},
		{MediaID: 2, Kind: KindMovie, Title: "Dune", Year: 1984},
		{MediaID: 3, Kind: KindMovie, Title: "Dune", Year: 2021},
		{MediaID: 4, Kind: KindSeries, Title: "Lost", Year: 2004},
	})
	cases := []struct {
		kind, title string
		year        int
		want        int64
	}{
		{KindMovie, "inception", 2010, 1},
		{KindMovie, "Inception", 0, 1},
		{KindMovie, "Dune", 2021, 3},
		{KindMovie, "Dune", 2022, 3},
		{KindMovie, "Dune", 0, 0}, // ambiguous
		{KindMovie, "Dune", 2000, 0},
		{KindMovie, "Lost", 2004, 0}, // wrong kind
		{KindSeries, "Lost", 0, 4},
	}
	for _, c := range cases {
		if got := idx.Find(c.kind, c.title, c.year); got != c.want {
			t.Errorf("Find(%s, %q, %d) = %d, want %d", c.kind, c.title, c.year, got, c.want)
		}
	}
}

// French films with English titles in Radarr and no ids on the Plex side
// (unmatched agent, wrong year): paired by file or folder on disk.
func TestMatchByFileAndFolder(t *testing.T) {
	arr := []ArrItem{
		{MediaID: 1, Kind: KindMovie, Title: "Anatomy of a Fall", Year: 2023,
			Folder: "/data/movies/Anatomy of a Fall (2023)", File: "Anatomy of a Fall (2023) Bluray-1080p.mkv"},
		{MediaID: 2, Kind: KindMovie, Title: "Love Is Overrated", Year: 2025,
			Folder: "/data/movies/Love Is Overrated (2025)", File: "Love.Is.Overrated.2025.mkv"},
		{MediaID: 3, Kind: KindSeries, Title: "Call My Agent!", Year: 2015, Folder: "/tv/Call My Agent!"},
		// Two movies in one flat folder: the folder alone proves nothing.
		{MediaID: 4, Kind: KindMovie, Title: "Flat One", Year: 2001, Folder: "/movies"},
		{MediaID: 5, Kind: KindMovie, Title: "Flat Two", Year: 2002, Folder: "/movies"},
	}
	plex := []ServerItem{
		// Different mount, file renamed: the folder still matches.
		{Kind: KindMovie, Key: "a", Title: "Anatomie d'une chute", Year: 1970,
			Files: []string{`D:\Films\Anatomy of a Fall (2023)\anatomie.mkv`}},
		// Same file name, different folder.
		{Kind: KindMovie, Key: "b", Title: "L'Amour c'est surcôté", Year: 2025,
			Files: []string{"/mnt/media/films/Love/Love.Is.Overrated.2025.mkv"}},
		{Kind: KindSeries, Key: "c", Title: "Dix pour cent", Year: 2015, Folders: []string{"/media/tv/Call My Agent!/"}},
		{Kind: KindMovie, Key: "d", Title: "Autre", Year: 2001, Files: []string{"/movies/autre.mkv"}},
	}
	links, only := Match(arr, plex)
	got := map[int64]string{}
	for _, l := range links {
		if l.Method != MethodFile {
			t.Errorf("link %+v: want method file", l)
		}
		got[l.MediaID] = l.Key
	}
	if got[1] != "a" || got[2] != "b" || got[3] != "c" || len(got) != 3 {
		t.Errorf("links: %v", got)
	}
	if len(only) != 1 || only[0].Key != "d" {
		t.Errorf("plex only: %+v", only)
	}
}

func TestMatchByAlternateTitle(t *testing.T) {
	arr := []ArrItem{{MediaID: 1, Kind: KindMovie, Title: "Anatomy of a Fall", Year: 2023,
		AltTitles: []string{"Anatomie d'une chute"}}}
	plex := []ServerItem{{Kind: KindMovie, Key: "a", Title: "Anatomie d’une chute", Year: 2023}}
	links, _ := Match(arr, plex)
	if len(links) != 1 || links[0].Method != MethodFuzzy {
		t.Fatalf("links: %+v", links)
	}
	// Plex original title against the main title.
	plex = []ServerItem{{Kind: KindMovie, Key: "a", Title: "Anatomie d'une chute", OriginalTitle: "Anatomy of a Fall", Year: 2024}}
	if links, _ := Match([]ArrItem{{MediaID: 1, Kind: KindMovie, Title: "Anatomy of a Fall", Year: 2023}}, plex); len(links) != 1 {
		t.Errorf("original title: %+v", links)
	}
}
