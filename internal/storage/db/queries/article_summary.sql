-- article_summaries is a history (migration 0024): one row per generation,
-- with is_current marking the one row per article that readers use. Writers
-- go through PostgresStore.addSummaryRow, which takes LockArticleForSummary
-- first so two writers for the same article cannot both insert a current row.

-- name: LockArticleForSummary :one
SELECT id FROM articles WHERE id = @id FOR NO KEY UPDATE;

-- name: GetCurrentArticleSummaryText :one
SELECT ai_summary FROM article_summaries
WHERE article_id = @article_id AND is_current;

-- name: DemoteCurrentArticleSummary :exec
UPDATE article_summaries SET is_current = FALSE
WHERE article_id = @article_id AND is_current;

-- name: InsertArticleSummary :exec
-- Provenance (model, prompt hash, duration) travels with the text, so a row
-- never pairs one model's text with another's name.
INSERT INTO article_summaries (article_id, ai_summary, skip_reason, model, prompt_hash, duration_ms, is_current)
VALUES (@article_id, @ai_summary, sqlc.narg('skip_reason')::text, sqlc.narg('model')::text,
        sqlc.narg('prompt_hash')::text, sqlc.narg('duration_ms')::bigint, @is_current);

-- name: GetArticleSummary :one
SELECT id, article_id, ai_summary, skip_reason, generated_at, model, prompt_hash, duration_ms, is_current
FROM article_summaries
WHERE article_id = @article_id AND is_current;

-- name: ListArticleSummaries :many
SELECT id, article_id, ai_summary, skip_reason, generated_at, model, prompt_hash, duration_ms, is_current
FROM article_summaries
WHERE article_id = @article_id
ORDER BY generated_at DESC, id DESC;

-- name: GetArticleSummaries :many
SELECT article_id, ai_summary
FROM article_summaries
WHERE article_id = ANY(@article_ids::bigint[])
  AND is_current
  AND ai_summary <> '';
