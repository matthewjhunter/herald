-- Queries that read or write the embedding vector itself live in the
-- hand-written pgvector layer (internal/storage/vector.go), not here: sqlc does
-- not model the vector type or its distance operators (#186). This file holds
-- the embedding bookkeeping queries -- status, retries, resets -- that never
-- touch the vector value.

-- The two Mark* queries are paired with a DropArticleEmbeddingChunks call: an
-- article that is now too short, or that failed to embed, must not keep the
-- chunk vectors of an earlier successful pass, or a stale vector would go on
-- answering searches for content the article no longer has.

-- name: DropArticleEmbeddingChunks :exec
DELETE FROM article_embedding_chunks WHERE article_id = @article_id;

-- name: MarkArticleEmbeddingSkipped :exec
INSERT INTO article_embeddings (article_id, embedding_model, status, attempts, error_message, last_attempted_at)
VALUES (@article_id, @embedding_model, @status, 0, NULL, NULL)
ON CONFLICT (article_id) DO UPDATE
SET embedding_model = EXCLUDED.embedding_model,
    status = @status,
    attempts = 0,
    error_message = NULL,
    last_attempted_at = NULL,
    created_at = NOW();

-- name: MarkArticleEmbeddingFailed :exec
INSERT INTO article_embeddings (article_id, embedding_model, status, attempts, error_message, last_attempted_at)
VALUES (@article_id, @embedding_model, @status, 1, @error_message::text, NOW())
ON CONFLICT (article_id) DO UPDATE
SET embedding_model = EXCLUDED.embedding_model,
    status = @status,
    attempts = article_embeddings.attempts + 1,
    error_message = EXCLUDED.error_message,
    last_attempted_at = NOW(),
    created_at = NOW();

-- name: ResetAllArticleEmbeddings :execrows
DELETE FROM article_embeddings;

-- name: ResetAllArticleEmbeddingChunks :execrows
DELETE FROM article_embedding_chunks;

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
-- The embed queue. An article is eligible once the summary stage has had its
-- say: ai_summary is prefixed to every chunk as retrieval context (#286), and a
-- vector built before the summary existed never picks it up, since nothing
-- re-embeds. So the queue waits for an article_summaries row (a summary or a
-- recorded skip) the way curation waits for the screen. Articles the summary
-- stage will never reach still embed, else they would sit in the queue forever:
-- screened but over the threat threshold (or screened with no score), and
-- those whose screening attempts are exhausted (GetUnscreenedArticles' cap).
-- linked_content comes along because the embed input appends the linked page
-- to the body; a projection without it embeds a link post on its commentary
-- alone.
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date,
       a.linked_url, a.linked_content,
       COALESCE(s.ai_summary, '') AS ai_summary
FROM articles a
LEFT JOIN article_summaries s ON s.article_id = a.id
LEFT JOIN article_embeddings ae ON a.id = ae.article_id AND ae.embedding_model = @embedding_model
WHERE (ae.article_id IS NULL
   OR (ae.status = @status AND ae.attempts < @max_attempts::int
       AND (ae.last_attempted_at IS NULL OR ae.last_attempted_at < @cutoff::timestamptz)))
  AND (s.article_id IS NOT NULL
   OR (a.security_screened_at IS NOT NULL
       AND NOT COALESCE(a.security_threat <= @max_security_threat::double precision, FALSE))
   OR (a.security_screened_at IS NULL AND a.security_attempts >= 3))
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
