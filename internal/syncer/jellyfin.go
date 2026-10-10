package syncer

import (
	"context"
	"maps"
	"slices"
	"strings"

	"hangarr/internal/clients/jellyfin"
	"hangarr/internal/matching"
	"hangarr/internal/store"
)

// fetchJellyfin reads the movies, series and seasons of the Jellyfin server,
// and stores its id (links of the UI). Like the Plex identity, a failing
// SystemInfo is not blocking.
func (s *Syncer) fetchJellyfin(ctx context.Context, c *jellyfin.Client) (*serverCatalog, error) {
	if info, err := c.SystemInfo(ctx); err == nil && info.ID != "" {
		if err := s.Store.SetSetting(ctx, store.SettingJellyfinServerID, info.ID, s.now()); err != nil {
			return nil, err
		}
	}
	cat := &serverCatalog{server: store.ServerJellyfin}
	movies, err := c.Items(ctx, jellyfin.TypeMovie)
	if err != nil {
		return nil, err
	}
	for _, it := range movies {
		if it.ID != "" {
			cat.items = append(cat.items, jellyfinItem(matching.KindMovie, it))
		}
	}
	groups := jellyfinGroups(movies)
	if len(groups) == 0 {
		cat.versionsOK = true
	} else {
		items, err := c.Versions(ctx, slices.Sorted(maps.Keys(groups)))
		if err != nil {
			s.Log.Warn("duplicate versions not read", "err", err)
		} else {
			cat.versions = jellyfinVersions(items, groups)
			cat.versionsOK = true
		}
	}
	shows, err := c.Items(ctx, jellyfin.TypeSeries)
	if err != nil {
		return nil, err
	}
	for _, it := range shows {
		if it.ID != "" {
			cat.items = append(cat.items, jellyfinItem(matching.KindSeries, it))
		}
	}
	seasons, err := c.Items(ctx, jellyfin.TypeSeason)
	if err != nil {
		return nil, err
	}
	for _, se := range seasons {
		if se.ID != "" {
			cat.seasons = append(cat.seasons, store.ServerSeason{ShowKey: se.SeriesID,
				Number: int(se.IndexNumber), Key: se.ID})
		}
	}
	return cat, nil
}

// jellyfinItem converts a movie or a series. Its path is the movie file or the
// show folder, which the file and folder matches use.
func jellyfinItem(kind string, it jellyfin.Item) matching.ServerItem {
	tvdb, tmdb, imdb := it.ExternalIDs()
	p := matching.ServerItem{Kind: kind, Key: it.ID, Title: it.Name, Year: int(it.ProductionYear),
		TvdbID: tvdb, TmdbID: tmdb, ImdbID: imdb, OriginalTitle: it.OriginalTitle}
	if !it.DateCreated.IsZero() {
		p.AddedAt = it.DateCreated.Unix()
	}
	if it.Path != "" {
		if kind == matching.KindMovie {
			p.Files = []string{it.Path}
		} else {
			p.Folders = []string{it.Path}
		}
	}
	return p
}

// jellyfinViewer is a Jellyfin user with the account id of its viewer.
type jellyfinViewer struct {
	userID  string
	account int64
}

// syncJellyfinHistory reads the history of every Jellyfin user. A user is
// matched to a stored viewer by name; an unknown one becomes a new account
// with the id derived from its Jellyfin id.
func (s *Syncer) syncJellyfinHistory(ctx context.Context, c *jellyfin.Client) (int, error) {
	users, err := c.Users(ctx)
	if err != nil {
		return 0, err
	}
	names, err := s.Store.AccountIDsByName(ctx)
	if err != nil {
		return 0, err
	}
	var accounts []store.PlexAccount
	viewers := make([]jellyfinViewer, 0, len(users))
	for _, u := range users {
		if !jellyfin.IsID(u.ID) {
			continue
		}
		id := names[strings.ToLower(u.Name)]
		if id == 0 {
			// An account already known under this name is not rewritten: the
			// other sources know more about it.
			id = jellyfin.AccountID(u.ID)
			accounts = append(accounts, store.PlexAccount{AccountID: id, Username: u.Name,
				IsActive: u.Policy == nil || !u.Policy.IsDisabled})
		}
		viewers = append(viewers, jellyfinViewer{userID: u.ID, account: id})
	}
	if err := s.Store.UpsertPlexAccounts(ctx, accounts); err != nil {
		return 0, err
	}
	stored := 0
	for _, v := range viewers {
		n, err := s.syncJellyfinUser(ctx, c, v)
		stored += n
		if err != nil {
			return stored, err
		}
	}
	return stored, nil
}

// syncJellyfinUser stores the items in progress of one user, then pages
// through the items played, most recent first, from the cursor of that user.
func (s *Syncer) syncJellyfinUser(ctx context.Context, c *jellyfin.Client, v jellyfinViewer) (int, error) {
	resumable, err := c.Resumable(ctx, v.userID)
	if err != nil {
		return 0, err
	}
	var inProgress []store.WatchEvent
	for _, it := range resumable {
		if e, ok := JellyfinEvent(v.userID, v.account, it); ok {
			inProgress = append(inProgress, e)
		}
	}
	if err := s.Store.UpsertWatchEvents(ctx, inProgress); err != nil {
		return 0, err
	}
	played, err := s.pageHistory(ctx, cursorJellyfinHistory+v.userID, func(start int) ([]store.WatchEvent, []int64, int, int, error) {
		items, total, err := c.Played(ctx, v.userID, start)
		if err != nil {
			return nil, nil, 0, 0, err
		}
		var events []store.WatchEvent
		dates := make([]int64, 0, len(items))
		for _, it := range items {
			dates = append(dates, playedDate(it))
			if e, ok := JellyfinEvent(v.userID, v.account, it); ok {
				events = append(events, e)
			}
		}
		return events, dates, len(items), total, nil
	}, jellyfin.PlayedPageSize)
	return len(inProgress) + played, err
}

// playedDate is the date of the last play of an item by its user, 0 if unknown.
func playedDate(it jellyfin.Item) int64 {
	if it.UserData == nil || it.UserData.LastPlayedDate.IsZero() {
		return 0
	}
	return it.UserData.LastPlayedDate.Unix()
}

// JellyfinEvent converts the last play of an item by a user. Jellyfin keeps
// only that last play, so a replay counts once, and it records no durations.
// ok is false for the items that are not stored: neither a movie nor an
// episode, no id, or no play date.
func JellyfinEvent(userID string, account int64, it jellyfin.Item) (store.WatchEvent, bool) {
	ud := it.UserData
	if (it.Type != jellyfin.TypeMovie && it.Type != jellyfin.TypeEpisode) || it.ID == "" || ud == nil ||
		ud.LastPlayedDate.IsZero() {
		return store.WatchEvent{}, false
	}
	last := ud.LastPlayedDate.Unix()
	e := store.WatchEvent{
		Source: SourceJellyfinHistory, SourceID: jellyfin.EventID(userID, it.ID), MediaType: "movie",
		RatingKey: it.ID, UserID: account, StartedAt: last, StoppedAt: last, PercentComplete: jellyfinPercent(it),
		Watched: ud.Played, Title: it.Name, Year: int(it.ProductionYear),
	}
	if it.Type == jellyfin.TypeEpisode {
		e.MediaType = "episode"
		e.GrandparentRatingKey, e.GrandparentTitle = it.SeriesID, it.SeriesName
		e.SeasonNumber, e.EpisodeNumber = intPtr(int(it.ParentIndexNumber)), intPtr(int(it.IndexNumber))
	}
	return e, true
}

// jellyfinPercent is the progress of a play, from 0 to 100: a played item is
// complete, otherwise the percentage Jellyfin reports, or else the position
// in the runtime of the item.
func jellyfinPercent(it jellyfin.Item) int {
	ud := it.UserData
	var p int64
	switch {
	case ud.Played:
		p = 100
	case ud.PlayedPercentage > 0:
		p = int64(ud.PlayedPercentage)
	case it.RunTimeTicks > 0:
		p = int64(ud.PlaybackPositionTicks) * 100 / int64(it.RunTimeTicks)
	}
	return int(min(max(p, 0), 100))
}
