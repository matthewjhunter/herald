-- +goose Up
--
-- Re-extract outbound links so same-host self-links are dropped (#206 follow-up).
-- The first pass (0005) stored every absolute <a href> in an article, including
-- a link-blog's own sidebar / "recent posts" / archive widgets, which point back
-- into the same site on every page. Those swamped the "linked by" lookup with
-- one feed linking to itself. The extractor now skips links whose host matches
-- the article's own host, so wipe the index and reset the gate to rebuild it
-- cleanly over the daemon's extraction cycles.
DELETE FROM article_links;
UPDATE articles SET links_extracted = FALSE;

-- +goose Down
-- No-op: the data is regenerated from article content, not lost. Reverting the
-- extractor logic alone (without re-running extraction) leaves the index as the
-- new code built it, which is harmless to the old query.
SELECT 1;
