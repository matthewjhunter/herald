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
-- The current pointer. History lives in prompt_versions (#258); this row is
-- what prompt resolution reads on the hot path, so it keeps its own copy of the
-- hash rather than joining to find it.
INSERT INTO user_prompts (user_id, prompt_type, prompt_template, template_hash, temperature, model, updated_at)
VALUES (@user_id, @prompt_type, @prompt_template, @template_hash, @temperature, @model, NOW())
ON CONFLICT (user_id, prompt_type) DO UPDATE SET
  prompt_template = excluded.prompt_template,
  template_hash = excluded.template_hash,
  temperature = excluded.temperature,
  model = COALESCE(excluded.model, user_prompts.model),
  updated_at = NOW();

-- name: GetUserPromptResolved :one
-- One lookup for everything resolution needs, replacing the three separate
-- GetUserPrompt* round trips per article per user.
SELECT prompt_template, template_hash, temperature, model FROM user_prompts
WHERE user_id = @user_id AND prompt_type = @prompt_type;

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

-- name: InsertPromptVersion :one
-- Append-only (#258): every call adds a row. A revert appends the older text as
-- a new version rather than rewinding, so ordering stays a truthful record.
INSERT INTO prompt_versions
  (user_id, prompt_type, prompt_template, template_hash, temperature, model, source)
VALUES (@user_id, @prompt_type, @prompt_template, @template_hash, @temperature, @model, @source)
RETURNING id;

-- name: RegisterPromptVersion :exec
-- Idempotent registration for the tiers with no row of their own (embedded
-- default, config file). Called once per process per distinct hash, never on
-- the hot path -- prompt resolution is cached and must not write.
INSERT INTO prompt_versions
  (user_id, prompt_type, prompt_template, template_hash, temperature, model, source)
SELECT @user_id, @prompt_type, @prompt_template, @template_hash, @temperature, @model, @source
WHERE NOT EXISTS (
  SELECT 1 FROM prompt_versions
  WHERE user_id = @user_id AND prompt_type = @prompt_type AND template_hash = @template_hash
);

-- name: ListPromptVersions :many
SELECT id, user_id, prompt_type, prompt_template, template_hash, temperature, model, source, created_at
FROM prompt_versions
WHERE user_id = @user_id AND prompt_type = @prompt_type
ORDER BY id DESC
LIMIT @lim;

-- name: GetPromptVersion :one
SELECT id, user_id, prompt_type, prompt_template, template_hash, temperature, model, source, created_at
FROM prompt_versions
WHERE id = @id;

-- name: GetPromptTemplateByHash :one
-- Recover the text behind a hash recorded elsewhere (feedback events, scores).
-- Any scope will do: the hash identifies the text, not who used it.
SELECT prompt_template FROM prompt_versions
WHERE template_hash = @template_hash
ORDER BY id ASC
LIMIT 1;
