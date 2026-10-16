package syncer

import (
	"context"
	"strconv"

	"golang.org/x/sync/errgroup"

	"hangarr/internal/clients/plex"
	"hangarr/internal/clients/radarr"
	"hangarr/internal/clients/sonarr"
	"hangarr/internal/matching"
	"hangarr/internal/store"
	"hangarr/internal/upstream"
)

// --- Sonarr / Radarr ---------------------------------------------------------

func tagLabels(ids []int64, labels map[int64]string) []string {
	var out []string
	for _, id := range ids {
		if l, ok := labels[id]; ok {
			out = append(out, l)
		}
	}
	return out
}

func images(imgs []sonarr.Image) (poster, fanart string) {
	for _, im := range imgs {
		switch im.CoverType {
		case "poster":
			poster = im.RemoteURL
		case "fanart":
			fanart = im.RemoteURL
		}
	}
	return poster, fanart
}

// arrLookups fetches tag labels and quality profile names of an *arr app.
func arrLookups(ctx context.Context, tags func(context.Context) ([]sonarr.Tag, error),
	profiles func(context.Context) ([]sonarr.QualityProfile, error)) (labels, names map[int64]string, err error) {
	var ts []sonarr.Tag
	var ps []sonarr.QualityProfile
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { ts, err = tags(gctx); return })
	g.Go(func() (err error) { ps, err = profiles(gctx); return })
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	labels, names = map[int64]string{}, map[int64]string{}
	for _, t := range ts {
		labels[t.ID] = t.Label
	}
	for _, p := range ps {
		names[p.ID] = p.Name
	}
	return labels, names, nil
}

func seriesItem(se sonarr.Series, labels, names map[int64]string) store.CatalogItem {
	poster, fanart := images(se.Images)
	it := store.CatalogItem{
		Kind: "series", ArrID: se.ID, Title: se.Title, SortTitle: se.SortTitle, TitleSlug: se.TitleSlug,
		Year: int(se.Year), Overview: se.Overview, TvdbID: int64(se.TvdbID), TmdbID: int64(se.TmdbID),
		ImdbID: se.ImdbID, Status: se.Status, Genres: se.Genres, NetworkOrStudio: se.Network,
		Rating: float64(se.Ratings.Value), AddedAt: se.Added.UnixPtr(), Tags: tagLabels(se.Tags, labels),
		QualityProfile: names[se.QualityProfileID], Monitored: se.Monitored, Path: se.Path,
		RuntimeMinutes: int(se.Runtime), PosterURL: poster, FanartURL: fanart,
		AltTitles: altTitles("", se.AlternateTitles),
	}
	if se.Statistics != nil {
		it.SizeBytes = int64(se.Statistics.SizeOnDisk)
	}
	// Episode counts exclude specials, consistently with watch_state.
	total, downloaded := 0, 0
	for _, sn := range se.Seasons {
		rec := store.SeasonRecord{Number: sn.SeasonNumber, Monitored: sn.Monitored}
		if st := sn.Statistics; st != nil {
			rec.EpisodeFileCount, rec.TotalEpisodeCount = int(st.EpisodeFileCount), int(st.TotalEpisodeCount)
			rec.SizeBytes = int64(st.SizeOnDisk)
		}
		if sn.SeasonNumber > 0 {
			total += rec.TotalEpisodeCount
			downloaded += rec.EpisodeFileCount
		}
		it.Seasons = append(it.Seasons, rec)
	}
	it.TotalEpisodes, it.DownloadedEpisodes = &total, &downloaded
	it.HasFile = downloaded > 0
	return it
}

func movieItem(m radarr.Movie, labels, names map[int64]string) store.CatalogItem {
	poster, fanart := images(m.Images)
	return store.CatalogItem{
		Kind: "movie", ArrID: m.ID, Title: m.Title, SortTitle: m.SortTitle, TitleSlug: m.TitleSlug,
		Year: int(m.Year), Overview: m.Overview, TmdbID: int64(m.TmdbID), ImdbID: m.ImdbID, Status: m.Status,
		Genres: m.Genres, NetworkOrStudio: m.Studio, Rating: m.Ratings.Best(), SizeBytes: int64(m.SizeOnDisk),
		AddedAt: m.Added.UnixPtr(), Tags: tagLabels(m.Tags, labels), QualityProfile: names[m.QualityProfileID],
		Monitored: m.Monitored, Path: m.Path, HasFile: m.HasFile, RuntimeMinutes: int(m.Runtime),
		PosterURL: poster, FanartURL: fanart, AltTitles: altTitles(m.OriginalTitle, m.AlternateTitles),
		FileName: fileName(m),
	}
}

func fileName(m radarr.Movie) string {
	if m.MovieFile == nil {
		return ""
	}
	return m.MovieFile.RelativePath
}

// altTitles lists the original and alternate titles, without duplicates.
func altTitles(original string, alt []sonarr.AlternateTitle) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	add(original)
	for _, a := range alt {
		add(a.Title)
	}
	return out
}

func (s *Syncer) fetchSeries(ctx context.Context, c *sonarr.Client) ([]store.CatalogItem, error) {
	var series []sonarr.Series
	var labels, names map[int64]string
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { series, err = c.Series(gctx); return })
	g.Go(func() (err error) {
		labels, names, err = arrLookups(gctx, c.Tags, c.QualityProfiles)
		return
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make([]store.CatalogItem, 0, len(series))
	for _, se := range series {
		out = append(out, seriesItem(se, labels, names))
	}
	return out, nil
}

func (s *Syncer) fetchMovies(ctx context.Context, c *radarr.Client) ([]store.CatalogItem, error) {
	var movies []radarr.Movie
	var labels, names map[int64]string
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { movies, err = c.Movies(gctx); return })
	g.Go(func() (err error) {
		labels, names, err = arrLookups(gctx, c.Tags, c.QualityProfiles)
		return
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make([]store.CatalogItem, 0, len(movies))
	for _, m := range movies {
		out = append(out, movieItem(m, labels, names))
	}
	return out, nil
}

// RefreshSeries re-reads one series from Sonarr (targeted resync after a
// season deletion) and recomputes statistics.
func (s *Syncer) RefreshSeries(ctx context.Context, arrID int64) error {
	c := s.Upstreams.Get().Sonarr
	if c == nil {
		return upstream.ErrNotConfigured
	}
	se, _, err := c.SeriesByID(ctx, arrID)
	if err != nil {
		return err
	}
	labels, names, err := arrLookups(ctx, c.Tags, c.QualityProfiles)
	if err != nil {
		return err
	}
	if err := s.Store.UpsertCatalogItem(ctx, "series", seriesItem(*se, labels, names), s.now()); err != nil {
		return err
	}
	return s.Recompute(ctx)
}

// Recompute refreshes play counts and watch_state from the
// stored data (after a local change such as a deletion).
func (s *Syncer) Recompute(ctx context.Context) error {
	return s.recompute(ctx)
}

// --- Media server catalog and matching ---------------------------------------

// serverCatalog is the catalog of the media server in use (Plex or Jellyfin).
type serverCatalog struct {
	server  string // store.ServerPlex or store.ServerJellyfin
	items   []matching.ServerItem
	seasons []store.ServerSeason
	// versions are stored only when versionsOK is set; otherwise the versions
	// of the previous sync are kept.
	versions   []store.MediaVersion
	versionsOK bool
}

func (s *Syncer) fetchPlex(ctx context.Context, c *plex.Client) (*serverCatalog, error) {
	if id, err := c.Identity(ctx); err == nil && id != "" {
		if err := s.Store.SetSetting(ctx, store.SettingPlexMachineID, id, s.now()); err != nil {
			return nil, err
		}
	}
	sections, err := c.Sections(ctx)
	if err != nil {
		return nil, err
	}
	cat := &serverCatalog{server: store.ServerPlex}
	var films []plex.Item
	for _, sec := range sections {
		key := string(sec.Key)
		switch sec.Type {
		case "movie":
			items, err := c.Items(ctx, key, plex.TypeMovie)
			if err != nil {
				return nil, err
			}
			films = append(films, items...)
			for _, it := range items {
				cat.items = append(cat.items, plexItem(matching.KindMovie, it))
			}
		case "show":
			shows, err := c.Items(ctx, key, plex.TypeShow)
			if err != nil {
				return nil, err
			}
			for _, it := range shows {
				cat.items = append(cat.items, plexItem(matching.KindSeries, it))
			}
			seasons, err := c.Items(ctx, key, plex.TypeSeason)
			if err != nil {
				return nil, err
			}
			for _, se := range seasons {
				cat.seasons = append(cat.seasons, store.ServerSeason{
					ShowKey: string(se.ParentRatingKey), Number: int(se.Index), Key: string(se.RatingKey)})
			}
		}
	}
	cat.versions = plexVersions(films)
	cat.versionsOK = true
	return cat, nil
}

func plexItem(kind string, it plex.Item) matching.ServerItem {
	tvdb, tmdb, imdb := it.ExternalIDs()
	p := matching.ServerItem{Kind: kind, Key: string(it.RatingKey), Title: it.Title, Year: int(it.Year),
		TvdbID: tvdb, TmdbID: tmdb, ImdbID: imdb, AddedAt: int64(it.AddedAt), OriginalTitle: it.OriginalTitle,
		Files: it.Files()}
	for _, l := range it.Location {
		p.Folders = append(p.Folders, l.Path)
	}
	return p
}

// arrHints are the matching data fetched from Sonarr/Radarr but not stored.
type arrHints struct {
	titles       []string
	folder, file string
}

func matchHints(series, movies []store.CatalogItem) map[string]arrHints {
	out := map[string]arrHints{}
	for _, list := range [][]store.CatalogItem{series, movies} {
		for _, it := range list {
			out[it.Kind+"|"+strconv.FormatInt(it.ArrID, 10)] = arrHints{titles: it.AltTitles, folder: it.Path, file: it.FileName}
		}
	}
	return out
}

func (s *Syncer) applyMatches(ctx context.Context, cat *serverCatalog, hints map[string]arrHints) error {
	arr, err := s.Store.ListArrMedia(ctx)
	if err != nil {
		return err
	}
	items := make([]matching.ArrItem, 0, len(arr))
	for _, m := range arr {
		h := hints[m.Kind+"|"+strconv.FormatInt(m.ArrID, 10)]
		items = append(items, matching.ArrItem{MediaID: m.ID, Kind: m.Kind, TvdbID: m.TvdbID, TmdbID: m.TmdbID,
			ImdbID: m.ImdbID, Title: m.Title, Year: m.Year, AltTitles: h.titles, Folder: h.folder, File: h.file})
	}
	links, plexOnly := matching.Match(items, cat.items)
	storeLinks := make([]store.ServerLink, 0, len(links))
	for _, l := range links {
		storeLinks = append(storeLinks, store.ServerLink{MediaID: l.MediaID, Key: l.Key, Method: l.Method})
	}
	only := make([]store.ServerOnlyItem, 0, len(plexOnly))
	for _, p := range plexOnly {
		only = append(only, store.ServerOnlyItem{Kind: p.Kind, Key: p.Key, Title: p.Title, Year: p.Year,
			TvdbID: p.TvdbID, TmdbID: p.TmdbID, ImdbID: p.ImdbID, AddedAt: p.AddedAt})
	}
	return s.Store.ApplyServerMatches(ctx, cat.server, storeLinks, only, cat.seasons, s.now())
}
