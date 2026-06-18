-- name: AddFeed :one
INSERT INTO feeds (url, title, description)
VALUES (@url, @title, @description::text)
RETURNING id;

-- name: GetFeed :one
SELECT * FROM feeds WHERE id = @id;

-- name: GetActiveFeedsToFetch :many
SELECT * FROM feeds
WHERE enabled = TRUE AND status = 'active'
  AND (next_fetch_at IS NULL OR next_fetch_at <= NOW());

-- name: IncrementFeedError :exec
UPDATE feeds
SET last_error = @last_error::text, consecutive_errors = consecutive_errors + 1
WHERE id = @id;

-- name: GetFeedErrorState :one
SELECT consecutive_errors, last_fetched FROM feeds WHERE id = @id;

-- name: MarkFeedDead :exec
UPDATE feeds SET status = 'dead' WHERE id = @id;

-- name: SetFeedNextFetch :exec
UPDATE feeds SET next_fetch_at = @next_fetch_at WHERE id = @id;

-- name: MarkFeedFetched :exec
UPDATE feeds
SET last_fetched = NOW(), last_error = NULL, consecutive_errors = 0, status = 'active'
WHERE id = @id;

-- name: MarkFeedFetchedWithNext :exec
UPDATE feeds
SET last_fetched = NOW(), last_error = NULL, consecutive_errors = 0,
    status = 'active', next_fetch_at = @next_fetch_at
WHERE id = @id;

-- name: UpdateFeedCacheHeaders :exec
UPDATE feeds SET etag = @etag::text, last_modified = @last_modified::text WHERE id = @id;

-- name: RenameFeed :exec
UPDATE feeds SET title = @title WHERE id = @id;

-- name: RenameUserFeed :exec
UPDATE user_feeds SET user_title = @user_title WHERE user_id = @user_id AND feed_id = @feed_id;

-- name: UpdateFeedSiteURL :exec
UPDATE feeds SET site_url = @site_url WHERE id = @id;

-- name: GetFeedRecentPublishDates :many
SELECT published_date FROM articles
WHERE feed_id = @feed_id AND published_date IS NOT NULL
ORDER BY published_date DESC
LIMIT 11;
