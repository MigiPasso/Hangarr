-- Accent/case-insensitive search: title normalized by matching.NormalizeTitle,
-- filled on every catalog upsert (the startup sync backfills existing rows).
ALTER TABLE media ADD COLUMN title_norm TEXT NOT NULL DEFAULT '';
