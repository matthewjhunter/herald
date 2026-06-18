-- Queries that read or write the embedding vector itself live in the
-- hand-written pgvector layer (internal/storage/vector.go), not here: sqlc does
-- not model the vector type or its distance operators (#186). This file holds
-- the embedding bookkeeping queries -- status, retries, resets -- that never
-- touch the vector value.

-- name: MarkArticleEmbeddingSkipped :exec
INSERT INTO article_embeddings (article_id, embedding_model, status, attempts, error_message, last_attempted_at)
VALUES (@article_id, @embedding_model, @status, 0, NULL, NULL)
ON CONFLICT (article_id) DO UPDATE
SET embedding = NULL,
    embedding_model = EXCLUDED.embedding_model,
    status = @status,
    attempts = 0,
    error_message = NULL,
    last_attempted_at = NULL,
    created_at = NOW();

-- name: MarkArticleEmbeddingFailed :exec
INSERT INTO article_embeddings (article_id, embedding_model, status, attempts, error_message, last_attempted_at)
VALUES (@article_id, @embedding_model, @status, 1, @error_message::text, NOW())
ON CONFLICT (article_id) DO UPDATE
SET embedding = NULL,
    embedding_model = EXCLUDED.embedding_model,
    status = @status,
    attempts = article_embeddings.attempts + 1,
    error_message = EXCLUDED.error_message,
    last_attempted_at = NOW(),
    created_at = NOW();

-- name: ResetAllArticleEmbeddings :execrows
DELETE FROM article_embeddings;

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

-- name: GroupsNeedingCentroid :many
-- Groups whose centroid is missing or was built under a different embedding
-- model, but that have at least one member with a usable (status ok) embedding
-- under the current model -- so a centroid recompute will actually produce a
-- vector. Drives the cluster stage's self-healing centroid-repair pass (#186):
-- after the BYTEA->vector reset (and after any model switch) existing groups
-- have no centroid until their members re-embed.
SELECT DISTINCT ag.id
FROM article_groups ag
JOIN article_group_members m ON m.group_id = ag.id
JOIN article_embeddings ae ON ae.article_id = m.article_id
  AND ae.embedding_model = @embedding_model AND ae.status = @status
WHERE ag.user_id = @user_id
  AND (ag.embedding IS NULL OR ag.embedding_model <> @embedding_model);
