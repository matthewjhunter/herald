-- Screen history (migration 0024). The gating verdict stays on articles;
-- these rows record every scan, including shadow ones that gate nothing.

-- name: InsertArticleScreen :exec
INSERT INTO article_screens (article_id, model, prompt_hash, threat, category, verified, flagged, via, effective)
VALUES (@article_id, @model, sqlc.narg('prompt_hash')::text, @threat, sqlc.narg('category')::text,
        sqlc.narg('verified')::boolean, @flagged, @via, @effective);

-- name: ListArticleScreens :many
SELECT id, article_id, model, prompt_hash, threat, category, verified, flagged, via, effective, screened_at
FROM article_screens
WHERE article_id = @article_id
ORDER BY screened_at DESC, id DESC;
