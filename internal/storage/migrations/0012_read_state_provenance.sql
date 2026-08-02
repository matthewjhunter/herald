-- +goose Up
--
-- Scoring-time provenance for the curation score (#258).
--
-- read_state.interest_score records what the curation model decided, but not
-- which model or prompt decided it. The feedback log (#251) worked around that
-- by joining user_prompts when the READER acted, which answers a different
-- question: it reconstructs the prompt in force at read time, not the one that
-- produced the score. Any edit between scoring and reading silently mislabels
-- every event in the queue -- and a prompt edit is exactly the moment the
-- labels matter most, since evaluating the edit is the whole point.
--
-- Recording at the point of scoring is the only place the answer is known for
-- certain. The feedback log now copies these forward instead of re-deriving
-- them, which also makes the snapshot rule in migration 0010 true rather than
-- aspirational.
--
-- Both stay NULL for scores written before this migration. That is honest --
-- their provenance was never recorded and cannot be recovered -- and consumers
-- must treat NULL as "unknown", never as "current".

ALTER TABLE read_state ADD COLUMN IF NOT EXISTS score_model TEXT;
ALTER TABLE read_state ADD COLUMN IF NOT EXISTS prompt_hash TEXT;

-- +goose Down
ALTER TABLE read_state DROP COLUMN IF EXISTS score_model;
ALTER TABLE read_state DROP COLUMN IF EXISTS prompt_hash;
