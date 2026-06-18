-- name: CreateNewsletter :one
INSERT INTO newsletters (user_id, name, schedule, config_json, prompt_template, email_recipient, enabled)
VALUES (@user_id, @name, @schedule, @config_json, @prompt_template, @email_recipient, @enabled)
RETURNING id;

-- name: UpdateNewsletter :exec
UPDATE newsletters
SET name = @name, schedule = @schedule, config_json = @config_json,
    prompt_template = @prompt_template, email_recipient = @email_recipient,
    enabled = @enabled, updated_at = NOW()
WHERE id = @id;

-- name: DeleteNewsletter :exec
DELETE FROM newsletters WHERE id = @id;

-- name: GetNewsletter :one
SELECT * FROM newsletters WHERE id = @id;

-- name: GetUserNewsletters :many
SELECT * FROM newsletters WHERE user_id = @user_id ORDER BY name;

-- name: GetDueNewsletters :many
SELECT * FROM newsletters
WHERE enabled = TRUE AND schedule = @schedule
  AND (last_generated_at IS NULL OR last_generated_at < @cutoff::timestamptz)
ORDER BY id;

-- name: CreateNewsletterIssue :one
INSERT INTO newsletter_issues (newsletter_id, headline, content_html, content_text, article_ids_json)
VALUES (@newsletter_id, @headline, @content_html, @content_text, @article_ids_json)
RETURNING id;

-- name: GetNewsletterIssue :one
SELECT * FROM newsletter_issues WHERE id = @id;

-- name: GetLatestNewsletterIssue :one
SELECT * FROM newsletter_issues WHERE newsletter_id = @newsletter_id
ORDER BY generated_at DESC LIMIT 1;

-- name: GetNewsletterIssues :many
SELECT id, newsletter_id, headline, content_text, generated_at, sent_at
FROM newsletter_issues WHERE newsletter_id = @newsletter_id
ORDER BY generated_at DESC LIMIT @lim OFFSET @off;

-- name: MarkNewsletterIssueSent :exec
UPDATE newsletter_issues SET sent_at = NOW() WHERE id = @id;

-- name: UpdateNewsletterLastGenerated :exec
UPDATE newsletters SET last_generated_at = NOW() WHERE id = @id;

-- name: GetNewsletterStats :many
SELECT n.id AS newsletter_id, n.name, COUNT(ni.id)::int AS issue_count
FROM newsletters n
LEFT JOIN newsletter_issues ni ON ni.newsletter_id = n.id
WHERE n.user_id = @user_id AND n.enabled = TRUE
GROUP BY n.id, n.name
ORDER BY n.name;
