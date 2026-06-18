-- +goose Up
--
-- Outbound-link index for the "which feed linked to this?" feature (#206). Each
-- row is one normalized external URL found in an article's body/summary; the
-- backlink lookup matches a target URL against url_norm. articles.linked_url is
-- NOT usable for this -- it holds the article's own primary/source link, not the
-- citations in its body -- so links are parsed from content here instead.
--
-- links_extracted gates the extraction stage: existing articles default FALSE
-- and are backfilled over cycles (like full_text_fetched), new articles are
-- processed once.
CREATE TABLE IF NOT EXISTS article_links (
    article_id BIGINT NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    url_norm   TEXT NOT NULL,
    PRIMARY KEY (article_id, url_norm)
);

-- The backlink query looks up by normalized URL across all articles, so url_norm
-- is the hot path.
CREATE INDEX IF NOT EXISTS idx_article_links_url_norm ON article_links (url_norm);

ALTER TABLE articles ADD COLUMN links_extracted BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE articles DROP COLUMN links_extracted;
DROP TABLE IF EXISTS article_links;
