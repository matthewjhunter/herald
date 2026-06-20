-- name: GetArticlesNeedingLinkExtraction :many
-- Articles whose outbound links haven't been parsed yet (new + backfill). url is
-- the article's own address: the extractor drops same-host links (sidebars,
-- archive widgets) so a feed doesn't flood the backlink index linking to itself.
SELECT id, url, content, summary
FROM articles
WHERE links_extracted = FALSE
ORDER BY fetched_date DESC
LIMIT @lim;

-- name: AddArticleLink :exec
INSERT INTO article_links (article_id, url_norm)
VALUES (@article_id, @url_norm)
ON CONFLICT (article_id, url_norm) DO NOTHING;

-- name: MarkArticleLinksExtracted :exec
UPDATE articles SET links_extracted = TRUE WHERE id = @id;
