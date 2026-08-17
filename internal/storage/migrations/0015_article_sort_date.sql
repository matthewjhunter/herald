-- +goose Up
--
-- An ordering key that considers both dates (#282).
--
-- Every list view ordered on published_date, which trusts a timestamp the
-- publisher writes and nobody validates. Ace of Spades emits
--
--     <pubDate>August 5, 2026 11:00 AM</pubDate>
--
-- with no timezone, so it parses as UTC while the site posts on Eastern. The
-- article was fetched six minutes after it really went out and filed four hours
-- down the list, below entries the reader had already been through. It never
-- appeared at the top of anything; one such article surfaced eleven days late.
--
-- sort_date keeps both dates in view through a single rule, with no per-feed
-- state to maintain -- per-feed timezones are unknowable at this scale:
--
--   fresh at fetch (within a day)  -> fetched_date, whatever the publisher says
--   older than that               -> published_date
--
-- The threshold does the discrimination. A discrepancy of hours is a broken
-- clock or poll latency, and the article is new to the reader either way. A
-- discrepancy of days is a genuinely old post -- a backfill on subscribe, a
-- digest, a feed republishing its archive -- which must file into history
-- rather than landing on top of today's news. A future-dated pubDate lands in
-- the first branch too, so a feed cannot pin itself above everything by
-- claiming tomorrow.
--
-- Generated rather than computed in Go so it cannot drift from its inputs, and
-- so every row already stored is corrected the moment this migration runs.
ALTER TABLE articles ADD COLUMN IF NOT EXISTS sort_date TIMESTAMPTZ
    GENERATED ALWAYS AS (
        CASE
            WHEN published_date IS NULL THEN fetched_date
            WHEN fetched_date - published_date > INTERVAL '24 hours' THEN published_date
            ELSE fetched_date
        END
    ) STORED;

-- The list key is (sort_date, publication, id): sort_date first, then
-- publication to order a poll batch that shares one fetch time, then id to make
-- the order total -- which is what lets a cursor resume exactly where a page
-- ended. COALESCE keeps the second column non-null so a NULL published_date
-- needs no special case in the comparison.
--
-- Rows stored before this migration carry a per-row fetch stamp rather than one
-- per poll, so their batches keep whatever order they were recorded in; the tie
-- break only reaches rows fetched from here on.
CREATE INDEX IF NOT EXISTS idx_articles_sort
    ON articles (sort_date DESC, COALESCE(published_date, fetched_date) DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_articles_feed_sort
    ON articles (feed_id, sort_date DESC, COALESCE(published_date, fetched_date) DESC, id DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_articles_feed_sort;
DROP INDEX IF EXISTS idx_articles_sort;
ALTER TABLE articles DROP COLUMN IF EXISTS sort_date;
