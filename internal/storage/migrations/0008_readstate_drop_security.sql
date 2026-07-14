-- +goose Up
--
-- Remove the security/threat columns from read_state. Plan 012 established that
-- the security verdict is a GLOBAL property of an article, shared by every
-- subscriber (#141) -- it lives on articles.security_threat, the sole authority
-- every threat filter and the stats query read. read_state is per-user state:
-- the read flag, the per-user interest_score, ai_scored/ai_retries.
--
-- These four columns were a per-user copy of that global verdict. They were
-- write-only -- the only query that set them (UpsertReadStateScores) is reached
-- solely through a code path no production caller exercises, and nothing ever
-- read them back. They predate plan 012 (0007 renamed security_score here too);
-- 012 propagated the duplication instead of ending it. Drop it now.

ALTER TABLE read_state DROP COLUMN security_threat;
ALTER TABLE read_state DROP COLUMN security_category;
ALTER TABLE read_state DROP COLUMN security_verified;
ALTER TABLE read_state DROP COLUMN security_flagged;

-- +goose Down
--
-- Re-add the columns, nullable, matching their post-0007 shape. The data they
-- held is not restored (it was a redundant copy of articles.security_threat and
-- carried no independent signal).

ALTER TABLE read_state ADD COLUMN security_threat DOUBLE PRECISION;
ALTER TABLE read_state ADD COLUMN security_category TEXT;
ALTER TABLE read_state ADD COLUMN security_verified BOOLEAN;
ALTER TABLE read_state ADD COLUMN security_flagged BOOLEAN NOT NULL DEFAULT FALSE;
