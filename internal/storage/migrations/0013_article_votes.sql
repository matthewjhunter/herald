-- +goose Up
--
-- Explicit per-article votes (#252, docs/feedback-events.md).
--
-- This table is CURRENT STATE only -- what the vote buttons should render as
-- right now. The history lives in feedback_events, where an upvote, a later
-- downvote and a retraction are three rows and two changes of mind, all kept.
-- Same split as read_state and its events: collapsing them would destroy the
-- record of the reader changing their mind, which is a stronger signal about a
-- borderline article than either vote alone.
--
-- reason is the axis the reader gave, or NULL for a bare vote. A bare vote is a
-- valid label: forcing a reason gets a random one, so the menu is optional by
-- design and NULL is expected to be the common case.
--
-- Deliberately NOT on read_state. A vote is an opinion the reader volunteered;
-- read state is bookkeeping the app maintains on their behalf. Keeping them
-- apart means "never voted" stays distinguishable from "voted neutral", and a
-- read_state reset (ResetReadStateScores) cannot silently discard opinions.

CREATE TABLE IF NOT EXISTS article_votes (
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    article_id BIGINT NOT NULL REFERENCES articles(id) ON DELETE CASCADE,

    -- -1 or 1. No zero: a retracted vote deletes the row, so absence means
    -- "no opinion" and there is exactly one representation of it.
    vote       SMALLINT NOT NULL CHECK (vote IN (-1, 1)),

    -- The axis of the reason, when one was given. Values are the reason
    -- vocabulary in feedback.go, not filter_rules.axis -- see the note there:
    -- filter_rules only admits author/category/tag and cannot express "not this
    -- source" or "wrong for me right now".
    reason     TEXT,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (user_id, article_id)
);

-- Rendering a list needs every vote for a page of articles in one lookup.
CREATE INDEX IF NOT EXISTS idx_article_votes_user ON article_votes(user_id, article_id);

-- +goose Down
DROP TABLE IF EXISTS article_votes;
