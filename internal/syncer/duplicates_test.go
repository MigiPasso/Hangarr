package syncer

import (
	"maps"
	"testing"
)

func TestDuplicateGroups(t *testing.T) {
	cases := []struct {
		name   string
		movies []serverMovie
		want   map[string]string
	}{
		{"single versions, distinct ids", []serverMovie{
			{Key: "1", TmdbID: 10, Count: 1}, {Key: "2", TmdbID: 20, Count: 1}, {Key: "3", Count: 1},
		}, map[string]string{}},
		{"one item with two versions", []serverMovie{
			{Key: "1", TmdbID: 10, ImdbID: "tt1", Count: 2}, {Key: "2", TmdbID: 20, Count: 1},
		}, map[string]string{"1": "tmdb:10"}},
		{"two versions, no ids", []serverMovie{{Key: "7", Count: 3}}, map[string]string{"7": "item:7"}},
		{"two items, same tmdb (two Plex sections)", []serverMovie{
			{Key: "1", TmdbID: 10, Count: 1}, {Key: "9", TmdbID: 10, Count: 1}, {Key: "3", TmdbID: 30, Count: 1},
		}, map[string]string{"1": "tmdb:10", "9": "tmdb:10"}},
		{"same imdb only", []serverMovie{
			{Key: "1", ImdbID: "tt5", Count: 1}, {Key: "2", ImdbID: "tt5", Count: 1},
		}, map[string]string{"1": "imdb:tt5", "2": "imdb:tt5"}},
		{"transitive: tmdb then imdb, smallest tmdb names the group", []serverMovie{
			{Key: "a", TmdbID: 40, ImdbID: "tt9", Count: 1},
			{Key: "b", TmdbID: 40, Count: 1},
			{Key: "c", TmdbID: 35, ImdbID: "tt9", Count: 1},
		}, map[string]string{"a": "tmdb:35", "b": "tmdb:35", "c": "tmdb:35"}},
		{"the same item listed twice is not a duplicate", []serverMovie{
			{Key: "1", TmdbID: 10, Count: 1}, {Key: "1", TmdbID: 10, Count: 1},
		}, map[string]string{}},
		{"count 0 counts as one version", []serverMovie{
			{Key: "1", TmdbID: 10}, {Key: "2", TmdbID: 10},
		}, map[string]string{"1": "tmdb:10", "2": "tmdb:10"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := duplicateGroups(c.movies); !maps.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// The group key must not depend on the order of the listing: it holds the
// acknowledgements from one sync to the next.
func TestDuplicateGroupsStableKey(t *testing.T) {
	a := []serverMovie{{Key: "x", TmdbID: 50, ImdbID: "tt2", Count: 1}, {Key: "y", ImdbID: "tt2", Count: 1}}
	b := []serverMovie{a[1], a[0]}
	ga, gb := duplicateGroups(a), duplicateGroups(b)
	if !maps.Equal(ga, gb) || ga["y"] != "tmdb:50" {
		t.Errorf("order changed the groups: %v vs %v", ga, gb)
	}
}
