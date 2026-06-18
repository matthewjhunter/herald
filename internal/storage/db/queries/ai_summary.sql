-- name: CreateAISummary :one
INSERT INTO ai_summaries (user_id, newsletter_id, status, model, prompt)
VALUES (@user_id, @newsletter_id, 'generating', @model, @prompt)
RETURNING id;

-- name: UpdateAISummaryDone :exec
UPDATE ai_summaries
SET status = 'done',
    headline = @headline,
    content_html = @content_html,
    article_ids_json = @article_ids_json,
    article_count = @article_count,
    input_tokens = @input_tokens,
    output_tokens = @output_tokens,
    error = '',
    generated_at = @generated_at::timestamptz
WHERE id = @id;

-- name: UpdateAISummaryFailed :exec
UPDATE ai_summaries
SET status = 'failed', error = @error, generated_at = @generated_at::timestamptz
WHERE id = @id;

-- name: GetLatestAISummary :one
SELECT * FROM ai_summaries
WHERE user_id = @user_id
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetInProgressAISummary :one
SELECT * FROM ai_summaries
WHERE user_id = @user_id AND status = 'generating'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetAISummary :one
SELECT * FROM ai_summaries
WHERE user_id = @user_id AND id = @id;

-- name: GetAISummaries :many
SELECT * FROM ai_summaries
WHERE user_id = @user_id
ORDER BY created_at DESC, id DESC
LIMIT @lim;

-- name: GetAISummariesForNewsletter :many
SELECT * FROM ai_summaries
WHERE user_id = @user_id AND newsletter_id = @newsletter_id
ORDER BY created_at DESC, id DESC
LIMIT @lim;

-- name: GetUnreadArticlesForSummary :many
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date
FROM articles a
JOIN user_feeds uf ON a.feed_id = uf.feed_id
JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = uf.user_id
WHERE uf.user_id = @user_id
  AND rs.read = FALSE
  AND a.security_score >= @min_security::double precision
  AND rs.interest_score >= @min_interest::double precision
ORDER BY COALESCE(a.published_date, a.fetched_date) DESC
LIMIT @lim;
