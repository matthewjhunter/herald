-- Asynchronous scoring batches (docs/batch-scoring.md, migration 0020).

-- name: InsertAIBatch :exec
INSERT INTO ai_batches (id, provider, kind, model, status, request_count, submitted_at, expires_at, adopted)
VALUES (@id, @provider, @kind, @model, 'running', @request_count, @submitted_at, sqlc.narg('expires_at')::timestamptz, @adopted);

-- name: InsertAIBatchRequest :exec
INSERT INTO ai_batch_requests (batch_id, custom_id, kind, user_id, subject_key, article_id, chunk_index, chunk_count, model, prompt_hash)
VALUES (@batch_id, @custom_id, @kind, @user_id, @subject_key, sqlc.narg('article_id')::bigint, @chunk_index, @chunk_count, @model, sqlc.narg('prompt_hash')::text);

-- name: InsertAIBatchRequestArticle :exec
INSERT INTO ai_batch_request_articles (batch_id, custom_id, article_id)
VALUES (@batch_id, @custom_id, @article_id)
ON CONFLICT DO NOTHING;

-- name: GetAIBatch :one
SELECT id, provider, kind, model, status, request_count, submitted_at, expires_at, last_polled_at, provider_ended_at, ended_at, error, adopted,
       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
FROM ai_batches WHERE id = @id;

-- name: ListRunningAIBatches :many
SELECT id, provider, kind, model, status, request_count, submitted_at, expires_at, last_polled_at, provider_ended_at, ended_at, error, adopted,
       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
FROM ai_batches WHERE status = 'running' ORDER BY submitted_at;

-- name: TouchAIBatchPolled :exec
UPDATE ai_batches SET last_polled_at = NOW() WHERE id = @id;

-- name: FinishAIBatch :exec
-- Terminal update: the status, the summed usage, the provider's own end time
-- when it reported one, and the moment herald applied the results.
UPDATE ai_batches
SET status = @status, error = sqlc.narg('error')::text, ended_at = NOW(),
    provider_ended_at = sqlc.narg('provider_ended_at')::timestamptz,
    input_tokens = @input_tokens, output_tokens = @output_tokens,
    cache_read_tokens = @cache_read_tokens, cache_write_tokens = @cache_write_tokens
WHERE id = @id;

-- name: ListAIBatchRequests :many
SELECT batch_id, custom_id, kind, user_id, subject_key, article_id, chunk_index, chunk_count, model, prompt_hash,
       outcome, resolved_at, error, threat, category, verified,
       input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
FROM ai_batch_requests WHERE batch_id = @batch_id ORDER BY custom_id;

-- name: ListAIBatchRequestArticles :many
SELECT custom_id, article_id FROM ai_batch_request_articles WHERE batch_id = @batch_id ORDER BY custom_id, article_id;

-- name: ResolveAIBatchRequest :exec
-- Record one result. The screen's per-chunk finding is kept so the article's
-- verdict can be assembled once its sibling chunks have answered too.
UPDATE ai_batch_requests
SET outcome = @outcome, resolved_at = NOW(), error = sqlc.narg('error')::text,
    threat = sqlc.narg('threat')::double precision, category = sqlc.narg('category')::text, verified = sqlc.narg('verified')::boolean,
    input_tokens = @input_tokens, output_tokens = @output_tokens,
    cache_read_tokens = @cache_read_tokens, cache_write_tokens = @cache_write_tokens
WHERE batch_id = @batch_id AND custom_id = @custom_id;

-- name: GetUnbatchedCurationArticles :many
-- GetUnscoredCurationArticles minus the in-flight set: articles that already
-- have a pending curation request for this user are waiting on a batch and
-- must not be submitted again.
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date,
       a.linked_url, a.linked_content
FROM articles a
JOIN user_feeds uf ON a.feed_id = uf.feed_id
LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = uf.user_id
WHERE uf.user_id = @user_id
  AND a.security_threat <= @max_security_threat::double precision
  AND rs.interest_score IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM ai_batch_requests r
      WHERE r.kind = 'curate' AND r.user_id = uf.user_id AND r.article_id = a.id AND r.outcome = 'pending'
  )
ORDER BY a.published_date DESC
LIMIT @lim;
