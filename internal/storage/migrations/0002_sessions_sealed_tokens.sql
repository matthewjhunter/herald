-- +goose Up
--
-- Bring the sessions table to the sealed-token shape (#173) and tidy a legacy
-- index name. The 0001 baseline only CREATEs sessions IF NOT EXISTS, so a
-- production database whose sessions table predates the sealed-token change --
-- plaintext TEXT access/refresh tokens, no version counter -- is left untouched
-- by 0001. This migration performs the transform the old in-process startup code
-- did before it was removed.
--
-- Drop and recreate rather than ALTER: the old plaintext tokens cannot be
-- re-sealed (they were never encrypted), so there is nothing worth keeping.
-- Sessions are ephemeral auth state; the only cost is one re-authentication per
-- active user. On a fresh database 0001 already built this exact shape, so this
-- is a recreate of an empty table.
DROP TABLE IF EXISTS sessions;
CREATE TABLE sessions (
    id              TEXT PRIMARY KEY,
    user_sub        TEXT NOT NULL,
    access_token    BYTEA NOT NULL,
    refresh_token   BYTEA NOT NULL,
    version         BIGINT NOT NULL DEFAULT 0,
    access_expiry   TIMESTAMPTZ NOT NULL,
    absolute_expiry TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_sessions_absolute_expiry ON sessions(absolute_expiry);

-- Cosmetic: production's article_summaries primary-key index kept the name
-- article_summaries_new_pkey from the old CREATE-new-table-then-RENAME rebuild.
-- A fresh build names it article_summaries_pkey; align them. The IF EXISTS guard
-- makes this a no-op on a fresh database (where the old name never existed).
ALTER INDEX IF EXISTS article_summaries_new_pkey RENAME TO article_summaries_pkey;

-- +goose Down
DROP TABLE IF EXISTS sessions;
CREATE TABLE sessions (
    id              TEXT PRIMARY KEY,
    user_sub        TEXT NOT NULL,
    access_token    TEXT NOT NULL,
    refresh_token   TEXT NOT NULL,
    access_expiry   TIMESTAMPTZ NOT NULL,
    absolute_expiry TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_sessions_absolute_expiry ON sessions(absolute_expiry);
ALTER INDEX IF EXISTS article_summaries_pkey RENAME TO article_summaries_new_pkey;
