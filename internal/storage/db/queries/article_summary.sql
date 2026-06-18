-- name: UpdateArticleAISummary :exec
INSERT INTO article_summaries (article_id, ai_summary, skip_reason)
VALUES (@article_id, @ai_summary, NULL)
ON CONFLICT (article_id) DO UPDATE SET
  ai_summary = EXCLUDED.ai_summary,
  skip_reason = NULL,
  generated_at = NOW();

-- name: MarkSummarizationSkipped :exec
INSERT INTO article_summaries (article_id, ai_summary, skip_reason)
VALUES (@article_id, '', @skip_reason::text)
ON CONFLICT (article_id) DO UPDATE SET
  skip_reason = EXCLUDED.skip_reason,
  generated_at = NOW();

-- name: GetArticleSummary :one
SELECT article_id, ai_summary, generated_at
FROM article_summaries
WHERE article_id = @article_id;
