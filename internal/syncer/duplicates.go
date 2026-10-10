package syncer

import (
	"strconv"
)

// serverMovie is a movie of the media server and its number of versions.
type serverMovie struct {
	Key    string
	TmdbID int64
	ImdbID string
	Count  int // versions of that item (at least 1)
}

// duplicateGroups returns the group key of every movie the server has
// several times: one item with several versions, or several items sharing a
// TMDB or IMDb id, transitively (A and B share a TMDB id, B and C an IMDb id:
// one group). The key comes from the ids, not from the order of the listing,
// so that it stays the same from one sync to the next (acknowledgements are
// stored under it): the smallest TMDB id, else the smallest IMDb id, else the
// item key for an item without ids.
func duplicateGroups(movies []serverMovie) map[string]string {
	parent := make([]int, len(movies))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		if ra, rb := find(a), find(b); ra != rb {
			parent[rb] = ra
		}
	}
	firstBy := map[string]int{}
	link := func(id string, i int) {
		if j, ok := firstBy[id]; ok {
			union(j, i)
		} else {
			firstBy[id] = i
		}
	}
	for i, m := range movies {
		link("key:"+m.Key, i) // the same item listed twice is one item
		if m.TmdbID > 0 {
			link("tmdb:"+strconv.FormatInt(m.TmdbID, 10), i)
		}
		if m.ImdbID != "" {
			link("imdb:"+m.ImdbID, i)
		}
	}

	type group struct {
		versions int
		items    map[string]bool
		tmdb     int64
		imdb     string
		minKey   string
	}
	groups := map[int]*group{}
	for i, m := range movies {
		r := find(i)
		g := groups[r]
		if g == nil {
			g = &group{items: map[string]bool{}, minKey: m.Key}
			groups[r] = g
		}
		if g.items[m.Key] {
			continue
		}
		g.items[m.Key] = true
		g.versions += max(m.Count, 1)
		if m.TmdbID > 0 && (g.tmdb == 0 || m.TmdbID < g.tmdb) {
			g.tmdb = m.TmdbID
		}
		if m.ImdbID != "" && (g.imdb == "" || m.ImdbID < g.imdb) {
			g.imdb = m.ImdbID
		}
		if m.Key < g.minKey {
			g.minKey = m.Key
		}
	}
	out := map[string]string{}
	for _, g := range groups {
		if g.versions < 2 {
			continue
		}
		key := "item:" + g.minKey
		switch {
		case g.tmdb > 0:
			key = "tmdb:" + strconv.FormatInt(g.tmdb, 10)
		case g.imdb != "":
			key = "imdb:" + g.imdb
		}
		for item := range g.items {
			out[item] = key
		}
	}
	return out
}
