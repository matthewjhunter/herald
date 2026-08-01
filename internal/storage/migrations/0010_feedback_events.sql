-- +goose Up
--
-- The feedback event log (#251, docs/feedback-events.md): an append-only record
-- of what Herald predicted about an article and what the reader then did with
-- it. read_state holds current per-user state; this holds history, with the
-- prediction attached.
--
-- Two design points that are easy to get wrong later:
--
--  1. `kind` records the CODE PATH, not a collapsed outcome. Several paths all
--     set read_state.read = TRUE and they mean opposite things -- opening an
--     article from the list is engagement, mark-all-read is queue bankruptcy
--     and carries no interest signal at all. Anything that merges these kinds
--     back together destroys the only reason this table exists.
--
--  2. Provenance is SNAPSHOTTED, not joined. Interest scores get rewritten,
--     prompts are mutated in place (#258), filter rules get deleted. A later
--     join reconstructs today's prediction rather than the one the reader was
--     reacting to, which silently rewrites history.
--
-- article_id is ON DELETE SET NULL rather than CASCADE: DeleteFeedArticlesBatch
-- removes a feed's articles when its last subscriber unsubscribes, so CASCADE
-- would erase a feed's entire label history at the exact moment the reader
-- produced the strongest signal available. Title, URL, and a copy of the
-- embedding are denormalized onto the event so the label outlives the article.

CREATE TABLE IF NOT EXISTS feedback_events (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Referent. Nulled when the article is pruned; the denormalized copies
    -- below keep the event usable as a training label after that.
    article_id     BIGINT REFERENCES articles(id) ON DELETE SET NULL,
    feed_id        BIGINT,
    article_title  TEXT,
    article_url    TEXT,
    embedding      vector(768),

    kind           TEXT NOT NULL,
    -- axis/axis_value mirror filter_rules so a reasoned signal is a supervised
    -- label on the same dimension the rules engine operates on. Passive events
    -- leave them NULL; the explicit controls (#252) populate them.
    axis           TEXT,
    axis_value     TEXT,

    -- Prediction provenance, as of the moment of the interaction.
    interest_score DOUBLE PRECISION,
    score_model    TEXT,
    prompt_hash    TEXT,
    -- rules_fired stays NULL until the scorer records which filter rules
    -- contributed to a score. It does not today -- filter_rules are CRUD-only
    -- and no scoring path reads them (#259).
    rules_fired    JSONB,
    list_position  INTEGER,
    surface        TEXT NOT NULL,
    exploration    BOOLEAN NOT NULL DEFAULT FALSE,

    -- Feed health at event time, so a broken feed's unsubscribe is not mined as
    -- a content judgment. A feed with errors on record is presumed dead
    -- regardless of what the reader clicked.
    feed_status    TEXT,
    feed_errors    INTEGER,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The corpus is read per user, newest first (training windows, recency
-- weighting), and per kind (weighting by signal strength).
CREATE INDEX IF NOT EXISTS idx_feedback_events_user_created ON feedback_events(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_feedback_events_user_kind ON feedback_events(user_id, kind);
CREATE INDEX IF NOT EXISTS idx_feedback_events_article ON feedback_events(article_id) WHERE article_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS feedback_events;
