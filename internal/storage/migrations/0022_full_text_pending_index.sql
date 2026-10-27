-- +goose Up
--
-- The full-text pass polls every cycle for articles still waiting on a fetch,
-- newest first (GetArticlesNeedingFullText). full_text_fetched is set once per
-- article and never cleared, so the pending rows are a handful against a table
-- that only grows, and without an index the planner scans the table to find
-- them -- logged in prod as a slow query on every cycle (issue #342). A partial
-- index holds only the pending rows, in the order the query wants them, so it
-- stays tiny and the query never sorts.
CREATE INDEX IF NOT EXISTS idx_articles_full_text_pending
    ON articles (fetched_date DESC)
    WHERE full_text_fetched = FALSE;

-- +goose Down
DROP INDEX IF EXISTS idx_articles_full_text_pending;
