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
