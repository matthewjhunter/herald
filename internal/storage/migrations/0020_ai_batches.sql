-- +goose Up
--
-- Asynchronous scoring through a provider batch API (docs/batch-scoring.md).
--
-- The screen and curate stages already work in batches of a hundred claimed
-- articles, but each article is still one synchronous model call. A provider
-- batch API takes the same calls as one submission, answers hours later at half
-- the price, and stacks with prompt caching. These tables are what herald needs
-- to remember between the submission and the results: which provider batch is
-- open, which article (or user and date) each request in it was for, and what
-- came back for the pieces that have to wait for their siblings.
--
-- ai_batches is one row per provider submission. The usage columns are summed
-- from the results and are herald's only cost visibility on this path: a
-- gateway passthrough does no accounting. adopted marks a batch found upstream
-- at startup that the table did not know (a gateway retry, or a crash between
-- the provider accepting it and the row being written); its prompt_hash
-- provenance is the currently resolved prompt rather than the one sent, and
-- the flag keeps that caveat visible.
--
-- ai_batch_requests is one row per request within a batch. subject_key is the
-- kind's own identity for what the request scored (an article id, or a user
-- and date for a digest); article_id is kept denormalized for the
-- single-article kinds because the in-flight queries filter on it, and is NULL
-- for a request that covers many articles. Per-chunk screen findings (threat,
-- category, verified) are stored here until every chunk of the article has
-- answered, since the article's verdict is the worst chunk and a missing one
-- could hide it. Nothing the model wrote is stored, consistent with the
-- payload-free verdict rule (plan 012).
--
-- ai_batch_request_articles records which articles a request covered: one row
-- for screen and curate, many for a digest.

CREATE TABLE IF NOT EXISTS ai_batches (
    -- The provider's batch id, which is the only handle for polling it.
    id                 TEXT PRIMARY KEY,
    provider           TEXT NOT NULL,
    kind               TEXT NOT NULL,
    model              TEXT NOT NULL,
    -- running | ended | failed, the collapsed state both providers map onto.
    status             TEXT NOT NULL DEFAULT 'running',
    request_count      INTEGER NOT NULL,
    submitted_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at         TIMESTAMPTZ,
    last_polled_at     TIMESTAMPTZ,
    -- When the provider reports the batch finished processing. ended_at below
    -- is when herald saw that and applied the results, which lags by up to a
    -- cycle; the difference between the two is detection lag, and
    -- provider_ended_at - submitted_at is the provider's processing time.
    provider_ended_at  TIMESTAMPTZ,
    ended_at           TIMESTAMPTZ,
    error              TEXT,
    adopted            BOOLEAN NOT NULL DEFAULT FALSE,
    input_tokens       BIGINT NOT NULL DEFAULT 0,
    output_tokens      BIGINT NOT NULL DEFAULT 0,
    cache_read_tokens  BIGINT NOT NULL DEFAULT 0,
    cache_write_tokens BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS ai_batches_running ON ai_batches (submitted_at) WHERE status = 'running';

CREATE TABLE IF NOT EXISTS ai_batch_requests (
    batch_id           TEXT NOT NULL REFERENCES ai_batches(id) ON DELETE CASCADE,
    custom_id          TEXT NOT NULL,
    kind               TEXT NOT NULL,
    -- 0 for work that is not per user (the security screen).
    user_id            BIGINT NOT NULL DEFAULT 0,
    subject_key        TEXT NOT NULL,
    article_id         BIGINT REFERENCES articles(id) ON DELETE CASCADE,
    chunk_index        INTEGER NOT NULL DEFAULT 0,
    chunk_count        INTEGER NOT NULL DEFAULT 1,
    model              TEXT NOT NULL,
    prompt_hash        TEXT,
    -- pending | succeeded | errored | expired | canceled
    outcome            TEXT NOT NULL DEFAULT 'pending',
    resolved_at        TIMESTAMPTZ,
    error              TEXT,
    threat             DOUBLE PRECISION,
    category           TEXT,
    verified           BOOLEAN,
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (batch_id, custom_id)
);

-- The in-flight set: which articles already have a pending request of a kind
-- for a user, so the submitter does not send them again next cycle.
CREATE INDEX IF NOT EXISTS ai_batch_requests_pending
    ON ai_batch_requests (kind, user_id, article_id) WHERE outcome = 'pending';

CREATE TABLE IF NOT EXISTS ai_batch_request_articles (
    batch_id   TEXT NOT NULL,
    custom_id  TEXT NOT NULL,
    article_id BIGINT NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    PRIMARY KEY (batch_id, custom_id, article_id),
    FOREIGN KEY (batch_id, custom_id) REFERENCES ai_batch_requests(batch_id, custom_id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE IF EXISTS ai_batch_request_articles;
DROP TABLE IF EXISTS ai_batch_requests;
DROP TABLE IF EXISTS ai_batches;
