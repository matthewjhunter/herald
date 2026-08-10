-- name: InsertArticleAuthor :exec
INSERT INTO article_authors (article_id, name, email)
VALUES (@article_id, @name, @email::text)
ON CONFLICT DO NOTHING;

-- name: InsertArticleCategory :exec
INSERT INTO article_categories (article_id, category)
VALUES (@article_id, @category)
ON CONFLICT DO NOTHING;

-- name: GetArticleAuthors :many
SELECT name, email FROM article_authors WHERE article_id = @article_id ORDER BY name;

-- name: GetArticleCategories :many
SELECT category FROM article_categories WHERE article_id = @article_id ORDER BY category;

-- name: GetFeedAuthors :many
SELECT DISTINCT aa.name FROM article_authors aa
JOIN articles a ON a.id = aa.article_id
WHERE a.feed_id = @feed_id
ORDER BY aa.name;

-- name: GetFeedCategories :many
SELECT DISTINCT ac.category FROM article_categories ac
JOIN articles a ON a.id = ac.article_id
WHERE a.feed_id = @feed_id
ORDER BY ac.category;

-- name: GetArticleAuthorsBatch :many
-- Authors for a page of articles in one round trip. The per-article query above
-- is an N+1 when a listing path needs metadata for every row it is about to
-- score (#274).
SELECT article_id, name FROM article_authors
WHERE article_id = ANY(@article_ids::bigint[])
ORDER BY article_id, name;

-- name: GetArticleCategoriesBatch :many
SELECT article_id, category FROM article_categories
WHERE article_id = ANY(@article_ids::bigint[])
ORDER BY article_id, category;
