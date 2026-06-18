-- name: SubscribeUserToFeed :exec
INSERT INTO user_feeds (user_id, feed_id) VALUES (@user_id, @feed_id)
ON CONFLICT DO NOTHING;

-- name: GetUserFeeds :many
SELECT f.id, f.url, COALESCE(uf.user_title, f.title) AS title, f.description,
       f.site_url, f.last_fetched, f.last_error, f.etag, f.last_modified,
       f.enabled, f.created_at, f.consecutive_errors, f.next_fetch_at, f.status
FROM feeds f
JOIN user_feeds uf ON f.id = uf.feed_id
WHERE uf.user_id = @user_id AND f.enabled = TRUE
ORDER BY COALESCE(uf.user_title, f.title);

-- name: GetAllSubscribedFeeds :many
SELECT DISTINCT f.* FROM feeds f
JOIN user_feeds uf ON f.id = uf.feed_id
WHERE f.enabled = TRUE AND f.status = 'active'
  AND (f.next_fetch_at IS NULL OR f.next_fetch_at <= NOW())
ORDER BY f.title;

-- name: GetAllActiveSubscribedFeeds :many
SELECT DISTINCT f.* FROM feeds f
JOIN user_feeds uf ON f.id = uf.feed_id
WHERE f.enabled = TRUE
ORDER BY f.title;

-- name: GetFeedSubscribers :many
SELECT user_id FROM user_feeds WHERE feed_id = @feed_id;

-- name: UnsubscribeUserFromFeed :exec
DELETE FROM user_feeds WHERE user_id = @user_id AND feed_id = @feed_id;

-- name: CountFeedSubscribers :one
SELECT COUNT(*) FROM user_feeds WHERE feed_id = @feed_id;

-- name: DeleteFeedArticlesBatch :execrows
DELETE FROM articles
WHERE id IN (SELECT a.id FROM articles a WHERE a.feed_id = @feed_id LIMIT @lim);

-- name: DeleteOrphanedFeed :execrows
DELETE FROM feeds
WHERE id = @feed_id AND NOT EXISTS (SELECT 1 FROM user_feeds WHERE user_feeds.feed_id = @feed_id);

-- name: GetAllSubscribingUsers :many
SELECT DISTINCT user_id FROM user_feeds ORDER BY user_id;
