-- name: AddFeedTag :exec
INSERT INTO feed_tags (user_id, feed_id, tag)
VALUES (@user_id, @feed_id, @tag)
ON CONFLICT DO NOTHING;

-- name: RemoveFeedTag :exec
DELETE FROM feed_tags
WHERE user_id = @user_id AND feed_id = @feed_id AND lower(tag) = lower(@tag);

-- name: GetFeedTags :many
SELECT tag FROM feed_tags
WHERE user_id = @user_id AND feed_id = @feed_id
ORDER BY tag;

-- name: GetAllFeedTags :many
SELECT feed_id, tag FROM feed_tags
WHERE user_id = @user_id
ORDER BY feed_id, tag;

-- name: GetUserTags :many
SELECT DISTINCT tag FROM feed_tags
WHERE user_id = @user_id
ORDER BY tag;

-- name: GetFeedsByTags :many
-- Tags must be lower-cased by the caller; matched case-insensitively.
SELECT DISTINCT feed_id FROM feed_tags
WHERE user_id = @user_id AND lower(tag) = ANY(@tags::text[])
ORDER BY feed_id;
