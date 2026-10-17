-- +goose Up
--
-- Generation-time provenance for the per-article AI summary.
--
-- article_summaries records what the model wrote but not which model wrote it,
-- which prompt it was given, or how long it took. The reader sees a block
-- labelled "AI Summary" and has no way to tell a 3B local model's output from a
-- frontier cloud model's, or to see the instructions that shaped it. The
-- curation score already records model and prompt hash at scoring time
-- (migration 0012) for the same reason: the moment of generation is the only
-- point where the answer is certain, and reading back whatever prompt is
-- current afterwards silently mislabels every summary written before an edit.
--
-- prompt_hash is the content-addressed key into prompt_versions, so the text
-- behind it can be recovered later without copying the template onto every
-- row. duration_ms is wall-clock for the model call alone, not the stage.
--
-- All three stay NULL for summaries written before this migration. That is
-- honest -- their provenance was never recorded and cannot be recovered -- and
-- consumers must treat NULL as "unknown", never as "current".
ALTER TABLE article_summaries ADD COLUMN IF NOT EXISTS model TEXT;
ALTER TABLE article_summaries ADD COLUMN IF NOT EXISTS prompt_hash TEXT;
ALTER TABLE article_summaries ADD COLUMN IF NOT EXISTS duration_ms BIGINT;

-- +goose Down
ALTER TABLE article_summaries DROP COLUMN IF EXISTS duration_ms;
ALTER TABLE article_summaries DROP COLUMN IF EXISTS prompt_hash;
ALTER TABLE article_summaries DROP COLUMN IF EXISTS model;
