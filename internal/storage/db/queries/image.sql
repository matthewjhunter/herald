-- name: StoreArticleImage :one
INSERT INTO article_images (article_id, original_url, data, mime_type, width, height)
VALUES (@article_id, @original_url, @data, @mime_type, @width, @height)
ON CONFLICT (article_id, original_url) DO UPDATE SET
  data = excluded.data,
  mime_type = excluded.mime_type,
  width = excluded.width,
  height = excluded.height,
  fetched_at = NOW()
RETURNING id;

-- name: GetArticleImage :one
SELECT * FROM article_images WHERE id = @id;

-- name: GetArticleImageMap :many
SELECT id, original_url FROM article_images WHERE article_id = @article_id;

-- name: GetArticlesNeedingImageCache :many
SELECT id, feed_id, guid, title, url, content, summary, author, published_date, fetched_date
FROM articles
WHERE images_cached = FALSE
ORDER BY fetched_date DESC
LIMIT @lim;

-- name: MarkArticleImagesCached :exec
UPDATE articles SET images_cached = TRUE WHERE id = @id;
