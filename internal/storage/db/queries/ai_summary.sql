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

-- GetUnreadArticlesForSummary moved to a hand-written pool query in
-- internal/storage/ai_summary.go (#259): sqlc cannot conditionally include the
-- filter-rule LATERAL join, so keeping it here would put a correlated subquery
-- on the digest path for every user, rules or not.
