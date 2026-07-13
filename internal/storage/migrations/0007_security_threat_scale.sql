-- +goose Up
--
-- Flip the security verdict from a "safety" score to a "threat" score, and stop
-- storing model prose. Plan 012 (which absorbs 011).
--
-- Polarity: the old security_score ran 0-10 with 10 = completely safe, which
-- cannot compose with additive evidence (a high number meaning "fine" has to be
-- subtracted from a ceiling). The new security_threat runs 0-10 with 0 = clean
-- and evidence ADDS -- the polarity airlock uses (detect.Score, screen.Threat).
-- The column is RENAMED, not reinterpreted in place, so any code still selecting
-- security_score fails to compile rather than silently reading a flipped number.
--
-- security_reason held a sentence the model wrote ABOUT attacker-authored text.
-- It was write-only (no readers) and it is exactly the payload that must never
-- sit in an unfenced channel -- a column a future dashboard or LLM-summarizer
-- could render. Dropped. The durable record is airlock/screen.Finding, which
-- carries no attacker bytes: threat, category (closed vocabulary), verified.
--
-- No data is converted (no `10 - security_score`): the prompt AND the scale both
-- change under plan 012, so every existing verdict is incomparable. The pipeline
-- re-screens from scratch. Nulling the values is done separately at deploy time
-- (the rescore), not here -- this migration only reshapes the columns.

ALTER TABLE articles RENAME COLUMN security_score TO security_threat;
ALTER TABLE articles DROP COLUMN security_reason;
ALTER TABLE articles ADD COLUMN security_category TEXT;
ALTER TABLE articles ADD COLUMN security_verified BOOLEAN;

ALTER TABLE read_state RENAME COLUMN security_score TO security_threat;
ALTER TABLE read_state DROP COLUMN security_reason;
ALTER TABLE read_state ADD COLUMN security_category TEXT;
ALTER TABLE read_state ADD COLUMN security_verified BOOLEAN;

-- +goose Down
--
-- Reverses the reshape. The dropped security_reason values are gone for good
-- (they held no recoverable signal by design); the column returns empty.

ALTER TABLE articles ADD COLUMN security_reason TEXT;
ALTER TABLE articles DROP COLUMN security_verified;
ALTER TABLE articles DROP COLUMN security_category;
ALTER TABLE articles RENAME COLUMN security_threat TO security_score;

ALTER TABLE read_state ADD COLUMN security_reason TEXT;
ALTER TABLE read_state DROP COLUMN security_verified;
ALTER TABLE read_state DROP COLUMN security_category;
ALTER TABLE read_state RENAME COLUMN security_threat TO security_score;
