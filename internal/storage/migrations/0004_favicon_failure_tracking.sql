-- +goose Up
--
-- Negative-cache favicon fetch failures so a permanently-broken favicon
-- (404/403/empty) is not re-fetched and re-logged every poll cycle (#112). The
-- favicon success cache stays in feed_favicons; this records *failure* state on
-- the feed itself so GetSubscribedFeedsWithoutFavicons can apply a backoff:
--
--   favicon_failed_at  -- when the last favicon fetch failed (NULL = no failure
--                         on record, e.g. never attempted or last attempt cached)
--   favicon_fail_kind  -- 'permanent' (404/403/410/451/empty) or 'transient'
--                         (5xx/timeout/network); '' when no failure on record.
--
-- A successful fetch clears both (and inserts the feed_favicons row, which is
-- what actually excludes the feed from the retry query).
ALTER TABLE feeds ADD COLUMN favicon_failed_at TIMESTAMPTZ;
ALTER TABLE feeds ADD COLUMN favicon_fail_kind TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE feeds DROP COLUMN favicon_fail_kind;
ALTER TABLE feeds DROP COLUMN favicon_failed_at;
