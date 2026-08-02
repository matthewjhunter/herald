-- +goose Up
--
-- Append-only prompt history (#258). user_prompts is keyed (user_id,
-- prompt_type) and edits overwrite prompt_template in place, so the text that
-- produced any already-written score is gone. That makes "did the rewrite
-- help?" unanswerable after the fact and leaves no way back from a bad edit.
--
-- The table is CONTENT-ADDRESSED: rows are identified by template_hash, the
-- sha256 of the template text. That matters because an effective prompt does
-- not always come from a database row. PromptLoader resolves through four
-- tiers -- embedded default, config file, admin override (user_id = 0), user
-- row -- and the bottom two have no row to version. Hashing the resolved text
-- gives every tier a stable identity, so a feedback event recorded on a stock
-- instance is as attributable as one from a user who wrote their own prompt.
-- Before this, both columns were simply NULL on every event.
--
-- source records which tier the text came from. It is descriptive, not a key:
-- the same text promoted from config into a user row keeps its hash and gains
-- a second row. Hash answers "which prompt", source answers "how did it get
-- used", and they are deliberately separate questions.
--
-- Rows are never updated or deleted by the application. A revert appends a new
-- row carrying an older hash rather than rewinding history, so the sequence of
-- ids stays a truthful record of what was in force and when.

CREATE TABLE IF NOT EXISTS prompt_versions (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- 0 for the non-user tiers (embedded default, config file, admin
    -- override), matching PromptLoader's own use of user_id = 0 as the global
    -- scope. Not a foreign key: a version must outlive the account that wrote
    -- it, or deleting a user would erase the provenance of scores that other
    -- tables still reference by hash.
    user_id         BIGINT NOT NULL,
    prompt_type     TEXT NOT NULL,

    prompt_template TEXT NOT NULL,
    -- sha256 of prompt_template, lowercase hex. Computed once, at write time,
    -- so the scoring path, the feedback log (#251) and this table cannot
    -- disagree about which prompt a score came from.
    template_hash   TEXT NOT NULL,

    temperature     DOUBLE PRECISION,
    model           TEXT,

    -- 'builtin' | 'config' | 'admin' | 'user'
    source          TEXT NOT NULL,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- History reads: one scope, newest first.
CREATE INDEX IF NOT EXISTS idx_prompt_versions_scope ON prompt_versions(user_id, prompt_type, id DESC);
-- Hash reads: recover the text behind a hash recorded elsewhere, from any scope.
CREATE INDEX IF NOT EXISTS idx_prompt_versions_hash ON prompt_versions(template_hash);

-- The current pointer keeps its own copy of the hash so prompt resolution stays
-- a single indexed lookup on the hot path -- it runs per article per user and
-- must not gain a join.
ALTER TABLE user_prompts ADD COLUMN IF NOT EXISTS template_hash TEXT;

-- Backfill: existing rows are real prompts in force, they just have no history.
-- Seed each as its own first version so the record starts complete rather than
-- with an unexplained gap before the first post-migration edit.
UPDATE user_prompts
SET template_hash = encode(sha256(prompt_template::bytea), 'hex')
WHERE template_hash IS NULL;

INSERT INTO prompt_versions (user_id, prompt_type, prompt_template, template_hash, temperature, model, source, created_at)
SELECT user_id, prompt_type, prompt_template,
       encode(sha256(prompt_template::bytea), 'hex'),
       temperature, model,
       CASE WHEN user_id = 0 THEN 'admin' ELSE 'user' END,
       COALESCE(updated_at, created_at, NOW())
FROM user_prompts;

-- +goose Down
ALTER TABLE user_prompts DROP COLUMN IF EXISTS template_hash;
DROP TABLE IF EXISTS prompt_versions;
