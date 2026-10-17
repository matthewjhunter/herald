-- name: UpdateArticleAISummary :exec
-- Provenance (model, prompt hash, duration) travels with the text: a re-summary
-- replaces all of it, so a row never pairs new text with an old model.
INSERT INTO article_summaries (article_id, ai_summary, skip_reason, model, prompt_hash, duration_ms)
VALUES (@article_id, @ai_summary, NULL, @model, @prompt_hash, @duration_ms)
ON CONFLICT (article_id) DO UPDATE SET
  ai_summary = EXCLUDED.ai_summary,
  skip_reason = NULL,
  model = EXCLUDED.model,
  prompt_hash = EXCLUDED.prompt_hash,
  duration_ms = EXCLUDED.duration_ms,
  generated_at = NOW();

-- name: MarkSummarizationSkipped :exec
INSERT INTO article_summaries (article_id, ai_summary, skip_reason)
VALUES (@article_id, '', @skip_reason::text)
ON CONFLICT (article_id) DO UPDATE SET
  skip_reason = EXCLUDED.skip_reason,
  generated_at = NOW();

-- name: GetArticleSummary :one
SELECT article_id, ai_summary, generated_at, model, prompt_hash, duration_ms
FROM article_summaries
WHERE article_id = @article_id;

-- name: GetArticleSummaries :many
SELECT article_id, ai_summary
FROM article_summaries
WHERE article_id = ANY(@article_ids::bigint[])
  AND ai_summary <> '';
