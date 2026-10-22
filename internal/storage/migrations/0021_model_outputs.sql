-- +goose Up
--
-- The model output corpus (docs/model-outputs.md).
--
-- Every scoring call already records its verdict (a score, a finding, a
-- summary) and, since #258, which model and prompt produced it. What it did not
-- keep is the pair a training set needs: the model's full completion and the
-- exact input it answered. The live rows cannot hold that -- read_state and
-- article_summaries are overwritten on rescoring, and article content is
-- rewritten by the full-text fetch and the boilerplate trim -- so this is an
-- append-only log beside them, one row per model call, written at the moment
-- the call returns.
--
-- Trust: `response` is raw model output over attacker-controlled article text
-- and is never rendered by the web UI or fed back into a prompt. Treat the
-- table as untrusted bytes, exactly like articles.content.
CREATE TABLE model_outputs (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- 'screen', 'curate', 'summarize': the prompt type that produced the row.
    kind          TEXT NOT NULL CHECK (kind IN ('screen', 'curate', 'summarize')),
    article_id    BIGINT NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    -- Set for per-user kinds (curate); NULL for the article-global ones. Not a
    -- foreign key, for the same reason prompt_versions.user_id is not: a sample
    -- outlives the account that caused it.
    user_id       BIGINT,
    -- The screen splits long articles into overlapping windows and sends one
    -- call per window; a row is one call, so one article may have several.
    chunk_index   INTEGER NOT NULL DEFAULT 0,
    chunk_count   INTEGER NOT NULL DEFAULT 1,

    model         TEXT NOT NULL,
    prompt_hash   TEXT NOT NULL,
    -- sha256 of the sanitized article content the prompt was rendered from
    -- (the string the pipeline's articleContent returns), lowercase hex. Lets
    -- an exporter tell whether the live article still matches what the model
    -- saw, and skip the ones the repair passes have rewritten since.
    content_hash  TEXT NOT NULL,
    -- The interests list the curation prompt was rendered with, NULL for the
    -- other kinds. Part of the input, so it has to travel with the output.
    keywords      TEXT,

    -- The model's completion, verbatim, capped at the writer's limit.
    response      TEXT NOT NULL,
    -- What herald extracted from it: the score and reasoning, the finding, or
    -- the summary's accepted length. Shape per kind, documented in the Go type.
    parsed        JSONB,

    -- 'sync' for a call the pipeline made itself, 'batch:<provider>' for one
    -- that went through a provider batch API (docs/batch-scoring.md).
    via           TEXT NOT NULL,
    -- Whether this row may be used to train a local model. Defaults true: a
    -- herald instance normally runs local models, whose output is its own. The
    -- batch stage writes false, because batch output comes from a cloud
    -- provider whose terms may restrict training on it. Model name is stored
    -- too, so a synchronous cloud call can still be filtered at export time.
    trainable     BOOLEAN NOT NULL DEFAULT TRUE,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_model_outputs_article ON model_outputs(article_id);
-- The exporter's scan: trainable rows of one kind, in arrival order.
CREATE INDEX idx_model_outputs_export ON model_outputs(kind, created_at) WHERE trainable;

-- +goose Down
DROP TABLE IF EXISTS model_outputs;
