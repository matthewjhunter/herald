-- +goose Up
--
-- Add a claim/lease column so the security screen can run as N concurrent
-- drainers (issue #233) -- whether N goroutines in one process or N separate
-- containers, they contend on the same queue and must not screen the same
-- article twice. screening_claimed_at is stamped when a worker claims a row and
-- cleared when the verdict is written; a claim older than the lease is
-- reclaimable (crash recovery). The claim query increments security_attempts, so
-- a poison article that repeatedly crashes a worker is eventually excluded by the
-- existing attempts < 3 cap rather than looping forever.

ALTER TABLE articles ADD COLUMN screening_claimed_at TIMESTAMPTZ;

-- Hot predicate for the claim query: unscreened rows still within the retry
-- budget, newest first. Narrower than idx_articles_unscreened (which omits the
-- attempts bound), so the claimant scan stays small as screened rows pile up.
CREATE INDEX IF NOT EXISTS idx_articles_screen_claimable
    ON articles (published_date DESC)
    WHERE security_screened_at IS NULL AND security_attempts < 3;

-- +goose Down

DROP INDEX IF EXISTS idx_articles_screen_claimable;
ALTER TABLE articles DROP COLUMN screening_claimed_at;
