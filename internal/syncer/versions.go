package syncer

import (
	"strconv"

	"hangarr/internal/clients/jellyfin"
	"hangarr/internal/clients/plex"
	"hangarr/internal/store"
)

// plexVersions lists the versions of the movies Plex has several times: one
// per media of an item of a group.
func plexVersions(movies []plex.Item) []store.MediaVersion {
	serverMovies := make([]serverMovie, 0, len(movies))
	for _, it := range movies {
		// Without a rating key, unrelated movies would share the empty key.
		if it.RatingKey == "" {
			continue
		}
		_, tmdb, imdb := it.ExternalIDs()
		serverMovies = append(serverMovies, serverMovie{Key: string(it.RatingKey), TmdbID: tmdb, ImdbID: imdb, Count: len(it.Media)})
	}
	groups := duplicateGroups(serverMovies)
	var out []store.MediaVersion
	// The key is unique in the table: an item listed twice is read once.
	seen := map[string]bool{}
	for _, it := range movies {
		group, ok := groups[string(it.RatingKey)]
		if !ok {
			continue
		}
		for _, m := range it.Media {
			if m.ID <= 0 {
				continue
			}
			id := strconv.FormatInt(int64(m.ID), 10)
			if seen[id] {
				continue
			}
			seen[id] = true
			v := store.MediaVersion{Server: store.ServerPlex, VersionKey: id, ItemKey: string(it.RatingKey),
				GroupKey: group, Parts: len(m.Part), Resolution: string(m.VideoResolution), VideoCodec: m.VideoCodec,
				AudioCodec: m.AudioCodec, Container: m.Container, Bitrate: int64(m.Bitrate)}
			for i, p := range m.Part {
				if i == 0 {
					v.File = p.File
				}
				v.SizeBytes += int64(p.Size)
			}
			out = append(out, v)
		}
	}
	return out
}

// jellyfinGroups returns the duplicate groups of the Jellyfin movies, each item
// counting its versions as MediaSourceCount says.
func jellyfinGroups(movies []jellyfin.Item) map[string]string {
	serverMovies := make([]serverMovie, 0, len(movies))
	for _, it := range movies {
		if it.ID == "" {
			continue
		}
		_, tmdb, imdb := it.ExternalIDs()
		serverMovies = append(serverMovies, serverMovie{Key: it.ID, TmdbID: tmdb, ImdbID: imdb, Count: int(it.MediaSourceCount)})
	}
	return duplicateGroups(serverMovies)
}

// jellyfinVersions lists the versions (media sources) of the items of a group.
func jellyfinVersions(items []jellyfin.Item, groups map[string]string) []store.MediaVersion {
	var out []store.MediaVersion
	// The key is unique in the table: a source listed twice is read once.
	seen := map[string]bool{}
	for _, it := range items {
		group, ok := groups[it.ID]
		if !ok {
			continue
		}
		for _, src := range it.MediaSources {
			if src.ID == "" || seen[src.ID] {
				continue
			}
			seen[src.ID] = true
			v := store.MediaVersion{Server: store.ServerJellyfin, VersionKey: src.ID, ItemKey: it.ID,
				GroupKey: group, File: src.Path, Parts: 1, SizeBytes: int64(src.Size), Container: src.Container,
				Bitrate: int64(src.Bitrate) / 1000}
			if video := firstStream(src.MediaStreams, "Video"); video != nil {
				v.VideoCodec = video.Codec
				v.Resolution = resolutionLabel(int(video.Width), int(video.Height))
			}
			if audio := firstStream(src.MediaStreams, "Audio"); audio != nil {
				v.AudioCodec = audio.Codec
			}
			out = append(out, v)
		}
	}
	return out
}

// firstStream returns the first stream of a type, nil if there is none.
func firstStream(streams []jellyfin.MediaStream, typ string) *jellyfin.MediaStream {
	for i := range streams {
		if streams[i].Type == typ {
			return &streams[i]
		}
	}
	return nil
}

// resolutionLabel names a picture size with the vocabulary of Plex. The width
// decides first: a 2.39:1 scope film has a small height.
func resolutionLabel(width, height int) string {
	switch {
	case width > 0:
		switch {
		case width >= 3200:
			return "4k"
		case width >= 1800:
			return "1080"
		case width >= 1200:
			return "720"
		case width >= 700:
			return "480"
		}
		return "sd"
	case height > 0:
		switch {
		case height >= 1600:
			return "4k"
		case height >= 900:
			return "1080"
		case height >= 600:
			return "720"
		case height >= 400:
			return "480"
		}
		return "sd"
	}
	return ""
}
