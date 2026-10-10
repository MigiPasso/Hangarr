package syncer

import (
	"maps"
	"slices"
	"testing"

	"hangarr/internal/clients/jellyfin"
	"hangarr/internal/clients/plex"
	"hangarr/internal/store"
)

func TestPlexVersions(t *testing.T) {
	movies := []plex.Item{
		{RatingKey: "6894", Title: "Dune", GUIDs: []plex.GUID{{ID: "tmdb://438631"}}, Media: []plex.Media{
			{ID: 11195, VideoResolution: "4k", VideoCodec: "hevc", AudioCodec: "truehd", Container: "mkv", Bitrate: 60000,
				Part: []plex.Part{{File: "/movies/Dune (2021)/Dune.mkv", Size: 1000}}},
			{ID: 11205, VideoResolution: "1080", VideoCodec: "h264", AudioCodec: "aac", Container: "mp4", Bitrate: 8000,
				Part: []plex.Part{{File: "/movies/Dune (2021)/Dune.1080.mp4", Size: 2000}}},
		}},
		{RatingKey: "10", Title: "Tenet", GUIDs: []plex.GUID{{ID: "tmdb://577922"}}, Media: []plex.Media{
			{ID: 301, VideoResolution: "1080", Part: []plex.Part{{File: "/movies/Tenet/a.mkv", Size: 1}, {File: "/movies/Tenet/b.mkv", Size: 2}}},
		}},
		{RatingKey: "11", Title: "Tenet", GUIDs: []plex.GUID{{ID: "tmdb://577922"}}, Media: []plex.Media{
			{ID: 302, VideoResolution: "1080", Part: []plex.Part{{File: "/movies/Tenet 2/Tenet.mkv", Size: 5}}},
		}},
		{RatingKey: "20", Title: "Inception", GUIDs: []plex.GUID{{ID: "tmdb://27205"}}, Media: []plex.Media{
			{ID: 401, Part: []plex.Part{{File: "/movies/Inception.mkv", Size: 9}}},
		}},
	}
	want := []store.MediaVersion{
		{Server: store.ServerPlex, VersionKey: "11195", ItemKey: "6894", GroupKey: "tmdb:438631",
			File: "/movies/Dune (2021)/Dune.mkv", Parts: 1, SizeBytes: 1000, Resolution: "4k", VideoCodec: "hevc",
			AudioCodec: "truehd", Container: "mkv", Bitrate: 60000},
		{Server: store.ServerPlex, VersionKey: "11205", ItemKey: "6894", GroupKey: "tmdb:438631",
			File: "/movies/Dune (2021)/Dune.1080.mp4", Parts: 1, SizeBytes: 2000, Resolution: "1080", VideoCodec: "h264",
			AudioCodec: "aac", Container: "mp4", Bitrate: 8000},
		{Server: store.ServerPlex, VersionKey: "301", ItemKey: "10", GroupKey: "tmdb:577922",
			File: "/movies/Tenet/a.mkv", Parts: 2, SizeBytes: 3, Resolution: "1080"},
		{Server: store.ServerPlex, VersionKey: "302", ItemKey: "11", GroupKey: "tmdb:577922",
			File: "/movies/Tenet 2/Tenet.mkv", Parts: 1, SizeBytes: 5, Resolution: "1080"},
	}
	if got := plexVersions(movies); !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestPlexVersionsSkipsMediaWithoutID(t *testing.T) {
	movies := []plex.Item{
		{RatingKey: "1", GUIDs: []plex.GUID{{ID: "tmdb://10"}}, Media: []plex.Media{
			{ID: 0, Part: []plex.Part{{File: "/a.mkv", Size: 1}}},
			{ID: 7, Part: []plex.Part{{File: "/b.mkv", Size: 2}}},
		}},
	}
	got := plexVersions(movies)
	if len(got) != 1 || got[0].VersionKey != "7" || got[0].File != "/b.mkv" {
		t.Errorf("got %+v, want only the version 7", got)
	}
}

func TestPlexVersionsSkipsEmptyKey(t *testing.T) {
	movies := []plex.Item{
		{GUIDs: []plex.GUID{{ID: "tmdb://10"}}, Media: []plex.Media{
			{ID: 1, Part: []plex.Part{{File: "/a.mkv", Size: 1}}},
			{ID: 2, Part: []plex.Part{{File: "/b.mkv", Size: 2}}},
		}},
		{GUIDs: []plex.GUID{{ID: "tmdb://10"}}, Media: []plex.Media{
			{ID: 3, Part: []plex.Part{{File: "/c.mkv", Size: 3}}},
		}},
	}
	if got := plexVersions(movies); len(got) != 0 {
		t.Errorf("movies without a rating key: got %+v, want none", got)
	}
}

// A listing can repeat an item (the library changed during the pages); its
// versions are stored once, since the key of a version is unique.
func TestVersionsListedTwice(t *testing.T) {
	dune := plex.Item{RatingKey: "6894", GUIDs: []plex.GUID{{ID: "tmdb://438631"}}, Media: []plex.Media{
		{ID: 11195, Part: []plex.Part{{File: "/a.mkv"}}},
		{ID: 11205, Part: []plex.Part{{File: "/b.mkv"}}},
	}}
	if got := plexVersions([]plex.Item{dune, dune}); len(got) != 2 {
		t.Errorf("plex: got %d versions, want 2", len(got))
	}
	const duneID = "d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6"
	items := []jellyfin.Item{{ID: duneID, MediaSourceCount: 2, ProviderIDs: map[string]string{"Tmdb": "438631"}, MediaSources: []jellyfin.MediaSource{
		{ID: "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d", Path: "/a.mkv"},
		{ID: "2a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d", Path: "/b.mkv"},
	}}}
	groups := map[string]string{duneID: "tmdb:438631"}
	if got := jellyfinVersions(append(items, items...), groups); len(got) != 2 {
		t.Errorf("jellyfin: got %d versions, want 2", len(got))
	}
}

func TestJellyfinVersions(t *testing.T) {
	const (
		dune      = "d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6"
		tenetA    = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d"
		tenetB    = "2a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d"
		inception = "3a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d"
		duneUHD   = "4a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d"
		duneHD    = "5a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d"
		tenetSrc  = "6a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d"
	)
	movies := []jellyfin.Item{
		{ID: dune, Name: "Dune", MediaSourceCount: 2, ProviderIDs: map[string]string{"Tmdb": "438631"}},
		{ID: tenetA, Name: "Tenet", ProviderIDs: map[string]string{"Tmdb": "577922"}},
		{ID: tenetB, Name: "Tenet", ProviderIDs: map[string]string{"Tmdb": "577922"}},
		{ID: inception, Name: "Inception", ProviderIDs: map[string]string{"Tmdb": "27205"}},
	}
	wantGroups := map[string]string{dune: "tmdb:438631", tenetA: "tmdb:577922", tenetB: "tmdb:577922"}
	groups := jellyfinGroups(movies)
	if !maps.Equal(groups, wantGroups) {
		t.Fatalf("groups: got %v, want %v", groups, wantGroups)
	}

	items := []jellyfin.Item{
		{ID: dune, MediaSources: []jellyfin.MediaSource{
			{ID: duneUHD, Path: "/movies/Dune/Dune.2160p.mkv", Container: "mkv", Size: 40, Bitrate: 60000000,
				MediaStreams: []jellyfin.MediaStream{
					{Type: "Video", Codec: "hevc", Width: 3840, Height: 1600},
					{Type: "Audio", Codec: "truehd"},
				}},
			{ID: duneHD, Path: "/movies/Dune/Dune.1080p.mkv", Container: "mkv", Size: 20, Bitrate: 60000000,
				MediaStreams: []jellyfin.MediaStream{
					{Type: "Video", Codec: "h264", Width: 1920, Height: 800},
					{Type: "Audio", Codec: "truehd"},
				}},
		}},
		{ID: inception, MediaSources: []jellyfin.MediaSource{{ID: "7a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d", Path: "/movies/Inception.mkv"}}},
		{ID: tenetA, MediaSources: []jellyfin.MediaSource{
			{ID: "", Path: "/movies/Tenet/ignored.mkv"},
			{ID: tenetSrc, Path: "/movies/Tenet/Tenet.mkv", Container: "mp4", Size: 9, Bitrate: 8000000,
				MediaStreams: []jellyfin.MediaStream{{Type: "Video", Codec: "h264", Width: 720, Height: 576}}},
		}},
	}
	want := []store.MediaVersion{
		{Server: store.ServerJellyfin, VersionKey: duneUHD, ItemKey: dune, GroupKey: "tmdb:438631",
			File: "/movies/Dune/Dune.2160p.mkv", Parts: 1, SizeBytes: 40, Resolution: "4k", VideoCodec: "hevc",
			AudioCodec: "truehd", Container: "mkv", Bitrate: 60000},
		{Server: store.ServerJellyfin, VersionKey: duneHD, ItemKey: dune, GroupKey: "tmdb:438631",
			File: "/movies/Dune/Dune.1080p.mkv", Parts: 1, SizeBytes: 20, Resolution: "1080", VideoCodec: "h264",
			AudioCodec: "truehd", Container: "mkv", Bitrate: 60000},
		{Server: store.ServerJellyfin, VersionKey: tenetSrc, ItemKey: tenetA, GroupKey: "tmdb:577922",
			File: "/movies/Tenet/Tenet.mkv", Parts: 1, SizeBytes: 9, Resolution: "480", VideoCodec: "h264",
			Container: "mp4", Bitrate: 8000},
	}
	if got := jellyfinVersions(items, groups); !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolutionLabel(t *testing.T) {
	cases := []struct {
		width, height int
		want          string
	}{
		{3840, 2160, "4k"},
		{3840, 1600, "4k"},
		{1920, 800, "1080"},
		{1280, 720, "720"},
		{720, 576, "480"},
		{640, 360, "sd"},
		{0, 2160, "4k"},
		{0, 1080, "1080"},
		{0, 0, ""},
	}
	for _, c := range cases {
		if got := resolutionLabel(c.width, c.height); got != c.want {
			t.Errorf("resolutionLabel(%d, %d) = %q, want %q", c.width, c.height, got, c.want)
		}
	}
}
