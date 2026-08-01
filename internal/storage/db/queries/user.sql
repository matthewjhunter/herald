-- name: CreateUser :one
INSERT INTO users (name) VALUES (@name) RETURNING id;

-- name: GetUserByName :one
SELECT * FROM users WHERE name = @name;

-- name: GetUserByOIDCSub :one
SELECT * FROM users WHERE oidc_sub = @oidc_sub;

-- name: CreateUserWithOIDC :one
INSERT INTO users (name, oidc_sub, email)
VALUES (@name, @oidc_sub, @email)
RETURNING *;

-- name: UpdateUserOIDCEmail :exec
UPDATE users SET email = @email WHERE id = @id;

-- name: ListUsers :many
SELECT * FROM users ORDER BY name;

-- The following deletes back PostgresStore.DeleteUser, which runs them in one
-- transaction. Tables with an ON DELETE CASCADE to users are removed by
-- DeleteUserRow; the rest are deleted explicitly first.

-- name: DeleteUserReadState :exec
DELETE FROM read_state WHERE user_id = @user_id;

-- name: DeleteUserPreferences :exec
DELETE FROM user_preferences WHERE user_id = @user_id;

-- name: DeleteUserFeeds :exec
DELETE FROM user_feeds WHERE user_id = @user_id;

-- name: DeleteUserFeedTags :exec
DELETE FROM feed_tags WHERE user_id = @user_id;

-- name: DeleteUserPrompts :exec
DELETE FROM user_prompts WHERE user_id = @user_id;

-- name: DeleteUserFilterRules :exec
DELETE FROM filter_rules WHERE user_id = @user_id;

-- name: DeleteUserArticleGroups :exec
DELETE FROM article_groups WHERE user_id = @user_id;

-- name: DeleteUserFeedbackEvents :exec
-- The FK would cascade this anyway; it is explicit because feedback events are
-- behavioral data and "deleting my account leaves no residue" is a promise
-- worth enforcing visibly and testing directly (docs/feedback-events.md).
DELETE FROM feedback_events WHERE user_id = @user_id;

-- name: DeleteUserRow :exec
DELETE FROM users WHERE id = @id;
