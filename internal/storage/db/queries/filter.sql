-- name: AddFilterRule :one
INSERT INTO filter_rules (user_id, feed_id, axis, value, score)
VALUES (@user_id, @feed_id, @axis, @value, @score)
RETURNING id;

-- name: GetFilterRules :many
-- When feed_id is NULL the caller wants every rule for the user; when set, the
-- user's global rules (feed_id IS NULL) plus rules scoped to that feed.
SELECT * FROM filter_rules
WHERE user_id = @user_id
  AND (sqlc.narg('feed_id')::bigint IS NULL OR feed_id IS NULL OR feed_id = sqlc.narg('feed_id'))
ORDER BY axis, value;

-- name: UpdateFilterRuleScore :execrows
UPDATE filter_rules SET score = @score WHERE id = @id AND user_id = @user_id;

-- name: DeleteFilterRule :execrows
DELETE FROM filter_rules WHERE id = @id AND user_id = @user_id;

-- name: HasFilterRules :one
SELECT COUNT(*) FROM filter_rules WHERE user_id = @user_id;
