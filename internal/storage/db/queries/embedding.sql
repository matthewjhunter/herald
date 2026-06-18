-- name: StoreArticleEmbedding :exec
INSERT INTO article_embeddings (article_id, embedding, embedding_model, status, attempts, error_message, last_attempted_at)
VALUES (@article_id, @embedding, @embedding_model, @status, 0, NULL, NULL)
ON CONFLICT (article_id) DO UPDATE
SET embedding = EXCLUDED.embedding,
    embedding_model = EXCLUDED.embedding_model,
    status = @status,
    attempts = 0,
    error_message = NULL,
    last_attempted_at = NULL,
    created_at = NOW();

-- name: MarkArticleEmbeddingSkipped :exec
INSERT INTO article_embeddings (article_id, embedding, embedding_model, status, attempts, error_message, last_attempted_at)
VALUES (@article_id, @embedding, @embedding_model, @status, 0, NULL, NULL)
ON CONFLICT (article_id) DO UPDATE
SET embedding_model = EXCLUDED.embedding_model,
    status = @status,
    attempts = 0,
    error_message = NULL,
    last_attempted_at = NULL,
    created_at = NOW();

-- name: MarkArticleEmbeddingFailed :exec
INSERT INTO article_embeddings (article_id, embedding, embedding_model, status, attempts, error_message, last_attempted_at)
VALUES (@article_id, @embedding, @embedding_model, @status, 1, @error_message::text, NOW())
ON CONFLICT (article_id) DO UPDATE
SET embedding_model = EXCLUDED.embedding_model,
    status = @status,
    attempts = article_embeddings.attempts + 1,
    error_message = EXCLUDED.error_message,
    last_attempted_at = NOW(),
    created_at = NOW();

-- name: GetArticleEmbeddings :many
SELECT ae.article_id, ae.embedding
FROM article_embeddings ae
JOIN articles a ON a.id = ae.article_id
JOIN user_feeds uf ON a.feed_id = uf.feed_id
WHERE uf.user_id = @user_id AND ae.embedding_model = @embedding_model;

-- name: ResetAllArticleEmbeddings :execrows
DELETE FROM article_embeddings;

-- name: GetArticleEmbeddingsByIDs :many
SELECT article_id, embedding FROM article_embeddings
WHERE article_id = ANY(@article_ids::bigint[])
  AND embedding_model = @embedding_model AND status = @status;

-- name: ResetAllGroupEmbeddings :execrows
UPDATE article_groups SET embedding = NULL, embedding_model = '';

-- name: ResetStuckEmbeddings :execrows
UPDATE article_embeddings
SET attempts = 0, last_attempted_at = NULL, error_message = NULL
WHERE embedding_model = @embedding_model AND status = @status AND attempts >= @max_attempts::int;

-- name: ResetStuckEmbeddingsLike :execrows
UPDATE article_embeddings
SET attempts = 0, last_attempted_at = NULL, error_message = NULL
WHERE embedding_model = @embedding_model AND status = @status AND attempts >= @max_attempts::int
  AND error_message LIKE @error_pattern::text;

-- name: GetArticlesWithoutEmbeddings :many
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date
FROM articles a
LEFT JOIN article_embeddings ae ON a.id = ae.article_id AND ae.embedding_model = @embedding_model
WHERE ae.article_id IS NULL
   OR (ae.status = @status AND ae.attempts < @max_attempts::int
       AND (ae.last_attempted_at IS NULL OR ae.last_attempted_at < @cutoff::timestamptz))
ORDER BY a.published_date DESC
LIMIT @lim;
