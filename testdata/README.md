# Fixtures

Responses served by `mockarr` (`make dev`, e2e) and used by the tests. The layout mirrors the upstream
endpoints:

| File | Endpoint |
|---|---|
| `sonarr/series.json`, `tag.json`, `qualityprofile.json` | `GET /api/v3/series`, `/tag`, `/qualityprofile` |
| `sonarr/diskspace.json`, `radarr/diskspace.json` | `GET /api/v3/diskspace` |
| `sonarr/health.json`, `radarr/health.json` | `GET /api/v3/health` (Sonarr: one indexer warning) |
| `*/indexer.json` | `GET /api/v3/indexer` (Sonarr: NZBgeek named by the `IndexerStatusCheck` of `health.json`, hence failing; Radarr: YTS disabled) |
| `radarr/movie.json`, `tag.json`, `qualityprofile.json` | `GET /api/v3/movie`, `/tag`, `/qualityprofile` |
| `sonarr/calendar.json`, `radarr/calendar.json` | `GET /api/v3/calendar?start=…&end=…` (dates recalées sur aujourd'hui) |
| `plex/sections.json` | `GET /library/sections` |
| `plex/section-<key>-type-<n>.json` | `GET /library/sections/<key>/all?type=<n>&includeGuids=1` |
| `plex/accounts.json`, `plex/history.json` | `GET /accounts`, `GET /status/sessions/history/all` |
| `tautulli/get_history.json`, `get_users.json` | `GET /api/v2?cmd=get_history&grouping=0…`, `cmd=get_users` |

## Synthetic scenario (`testdata/`)

Hand-written from the API documentation, timestamps anchored on **2026-10-01T00:00:00Z** (`1790812800`;
`mockarr` shifts them to "now" when `MOCK_SHIFT_DATES=true`). The tests assert on it, so change it with care:

- users: alice (1001), bob (1002), carol (1003, inactive, no history); Tautulli's "Local" user (0) is ignored;
- Breaking Bad: completed (6/6, resumed session of S1E2 sharing `reference_id` 9), also watched by bob;
- The Expanse: season 1 watched, season 2 started then left 140 days ago → abandoned, never-watched season;
- Severance: never watched, tagged `keep` (protected); Shōgun (Sonarr) ↔ Shogun (Plex, no GUID) → fuzzy match;
- Arcane, Avatar: in Sonarr/Radarr only; Home Videos, Vacances 2019: in Plex only;
- Inception: watched by bob, and by alice under an old rating key (555) → resolved by title + year;
- Dune: two sessions with the same `reference_id` → one play, in progress; Interstellar: abandoned; Tenet: never;
- calendar (always moved so that the anchor is today, filtered by `start`/`end`): Severance S02E01 (+2 days),
  S02E02, S02E10 (season finale, +65) and S03E01 (+120); Shōgun S02E01–E03 the same day (+20); Pluribus (not in
  the catalog, series premiere, +5); Avatar: Fire and Ash in cinemas (+10), digital (+100), physical (+130);
  Project Hail Mary (not in the catalog, +40); one past episode (−3 days) that must not show; plus, for volume, the
  rest of Severance season 2 and about ten series and nine movies not in the catalog (weekly shows and anime, whole
  seasons released at once, an episode airing today, releases up to +130 days);
- Lost: deleted from the library → orphan history row; plus a music track and a live session, both ignored.

## Dev extras (`testdata/dev/`, `posters.json`, `fanarts.json`)

For `make dev` only (`compose.dev.yml`), never used by the tests:

- `MOCK_DEV_EXTRAS=true`: `dev/sonarr/series.json` and `dev/radarr/movie.json` are added to the catalog (the series
  and movies of the calendars, so that they sync and get a detail page);
- `MOCK_REAL_POSTERS=true`: `mediacover` serves the real posters listed in `posters.json` (`"sonarr/3"` → URL;
  TVmaze for series and anime, IMDb for movies) and the real backdrops listed in `fanarts.json` (TMDB, `w1280`),
  downloaded once by the mock and kept in memory. Only URLs are committed, never the images. An item without a
  fanart there gets a 404: the detail page then shows its blurred poster.

## Real captures (`testdata/captured/`)

`make capture-fixtures` writes anonymized responses of your real instances to `testdata/captured/` (same layout).
Tests check that every captured file decodes; the synthetic scenario stays the reference for assertions. Review
the files before committing them. Point the mock at them with `MOCK_FIXTURES=/testdata/captured`.
