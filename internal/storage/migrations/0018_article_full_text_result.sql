-- +goose Up
--
-- Record WHY a full-text fetch ended, not just that it was considered (#239
-- follow-up).
--
-- full_text_fetched is set unconditionally, before the fetch runs, so that a
-- failing article can never be retried forever. That is the right call for the
-- queue and the wrong one for diagnosis: "processed" and "rejected" become the
-- same state the moment the row is written, and the only record of which is a
-- log line in a container that rotates.
--
-- It cost a real article. On 2026-08-31 a post was rejected because the
-- extraction carried the site's contact block -- five co-blogger addresses in
-- the page header -- and the contact-page guard counted addresses rather than
-- weighing them against the body. The guard was fixed the same day (#305), but
-- nothing could find the articles the old rule had turned away, because the
-- rows recording that decision are indistinguishable from rows that succeeded.
--
-- A nullable column, so every existing row reads as "outcome not recorded"
-- rather than being assigned one it never had. full_text_fetched keeps its
-- meaning and its queries; this only adds the reason beside it.
ALTER TABLE articles ADD COLUMN IF NOT EXISTS full_text_result TEXT;

COMMENT ON COLUMN articles.full_text_result IS
    'Why the full-text pass ended. NULL for rows processed before this column '
    'existed. Values: replaced, linked, not_truncated, no_url, skipped, '
    'cancelled, fetch_failed, linked_fetch_failed, too_short, contact_page, '
    'no_overlap, store_failed.';

-- Partial, because the interesting rows are the minority: the ones a heuristic
-- turned away. This is what makes "re-run everything the old contact-page rule
-- rejected" a query rather than a full scan.
CREATE INDEX IF NOT EXISTS idx_articles_full_text_result
    ON articles (full_text_result)
    WHERE full_text_result IS NOT NULL AND full_text_result <> 'replaced';

-- +goose Down
DROP INDEX IF EXISTS idx_articles_full_text_result;
ALTER TABLE articles DROP COLUMN IF EXISTS full_text_result;
