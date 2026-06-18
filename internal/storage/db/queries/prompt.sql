-- name: GetUserPrompt :one
SELECT prompt_template FROM user_prompts
WHERE user_id = @user_id AND prompt_type = @prompt_type;

-- name: GetUserPromptTemperature :one
SELECT temperature FROM user_prompts
WHERE user_id = @user_id AND prompt_type = @prompt_type;

-- name: GetUserPromptModel :one
SELECT model FROM user_prompts
WHERE user_id = @user_id AND prompt_type = @prompt_type;

-- name: SetUserPrompt :exec
INSERT INTO user_prompts (user_id, prompt_type, prompt_template, temperature, model, updated_at)
VALUES (@user_id, @prompt_type, @prompt_template, @temperature, @model, NOW())
ON CONFLICT (user_id, prompt_type) DO UPDATE SET
  prompt_template = excluded.prompt_template,
  temperature = excluded.temperature,
  model = COALESCE(excluded.model, user_prompts.model),
  updated_at = NOW();

-- name: DeleteUserPrompt :exec
DELETE FROM user_prompts WHERE user_id = @user_id AND prompt_type = @prompt_type;

-- name: ListUserPrompts :many
SELECT prompt_type, prompt_template, temperature, model, created_at, updated_at
FROM user_prompts
WHERE user_id = @user_id
ORDER BY prompt_type;

-- name: GetUserPreference :one
SELECT value FROM user_preferences WHERE user_id = @user_id AND key = @key;

-- name: SetUserPreference :exec
INSERT INTO user_preferences (user_id, key, value)
VALUES (@user_id, @key, @value)
ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value;

-- name: GetAllUserPreferences :many
SELECT key, value FROM user_preferences WHERE user_id = @user_id;

-- name: DeleteUserPreference :exec
DELETE FROM user_preferences WHERE user_id = @user_id AND key = @key;
