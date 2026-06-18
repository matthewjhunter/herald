-- name: StoreFeedFavicon :exec
INSERT INTO feed_favicons (feed_id, data, mime_type, fetched_at)
VALUES (@feed_id, @data, @mime_type, NOW())
ON CONFLICT (feed_id) DO UPDATE SET
  data = excluded.data, mime_type = excluded.mime_type, fetched_at = NOW();

-- name: GetFeedFavicon :one
SELECT * FROM feed_favicons WHERE feed_id = @feed_id;

-- name: GetAllFeedFavicons :many
SELECT * FROM feed_favicons;

-- name: GetSubscribedFeedsWithoutFavicons :many
-- Feeds a user subscribes to that have no cached favicon AND are not inside a
-- failure backoff window (#112): permanent failures retry at most every 30 days,
-- transient ones every 6 hours. A feed with a cached favicon (feed_favicons row)
-- is always excluded.
SELECT DISTINCT f.* FROM feeds f
JOIN user_feeds uf ON f.id = uf.feed_id
LEFT JOIN feed_favicons ff ON f.id = ff.feed_id
WHERE ff.feed_id IS NULL
  AND (
    f.favicon_failed_at IS NULL
    OR (f.favicon_fail_kind = 'transient' AND f.favicon_failed_at < NOW() - INTERVAL '6 hours')
    OR (f.favicon_fail_kind = 'permanent' AND f.favicon_failed_at < NOW() - INTERVAL '30 days')
  )
ORDER BY f.id;

-- name: RecordFaviconFailure :exec
UPDATE feeds SET favicon_failed_at = NOW(), favicon_fail_kind = @fail_kind::text
WHERE id = @feed_id;

-- name: ClearFaviconFailure :exec
UPDATE feeds SET favicon_failed_at = NULL, favicon_fail_kind = ''
WHERE id = @feed_id;
