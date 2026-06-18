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
SELECT DISTINCT f.* FROM feeds f
JOIN user_feeds uf ON f.id = uf.feed_id
LEFT JOIN feed_favicons ff ON f.id = ff.feed_id
WHERE ff.feed_id IS NULL
ORDER BY f.id;
