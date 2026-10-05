# Fixtures

Responses served by `mockarr` (`make dev`, e2e) and used by the tests. The layout mirrors the upstream
endpoints:

| File | Endpoint |
|---|---|
| `sonarr/series.json`, `tag.json`, `qualityprofile.json` | `GET /api/v3/series`, `/tag`, `/qualityprofile` |
| `sonarr/diskspace.json`, `radarr/diskspace.json` | `GET /api/v3/diskspace` |
| `radarr/movie.json`, `tag.json`, `qualityprofile.json` | `GET /api/v3/movie`, `/tag`, `/qualityprofile` |
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
- Lost: deleted from the library → orphan history row; plus a music track and a live session, both ignored.

## Real captures (`testdata/captured/`)

`make capture-fixtures` writes anonymized responses of your real instances to `testdata/captured/` (same layout).
Tests check that every captured file decodes; the synthetic scenario stays the reference for assertions. Review
the files before committing them. Point the mock at them with `MOCK_FIXTURES=/testdata/captured`.
