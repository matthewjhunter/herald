-- name: FindDuplicateArticle :one
SELECT id FROM articles
WHERE title = @title AND published_date = @published_date
LIMIT 1;

-- name: AddArticle :one
INSERT INTO articles (feed_id, guid, title, url, content, summary, author, published_date)
VALUES (@feed_id, @guid, @title, @url, @content::text, @summary::text, @author::text, @published_date)
ON CONFLICT (feed_id, guid) DO NOTHING
RETURNING id;

-- name: GetArticle :one
SELECT id, feed_id, guid, title, url, content, summary, author,
       published_date, fetched_date, linked_url, linked_content
FROM articles WHERE id = @id;

-- name: GetUnreadArticles :many
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date
FROM articles a
LEFT JOIN read_state rs ON a.id = rs.article_id
WHERE rs.article_id IS NULL OR rs.read = FALSE
ORDER BY a.published_date DESC
LIMIT @lim;

-- name: GetUnscoredArticleCount :one
SELECT COUNT(*)::int
FROM articles a
JOIN user_feeds uf ON a.feed_id = uf.feed_id
LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = @user_id
WHERE uf.user_id = @user_id
  AND (
    (a.security_screened_at IS NULL AND a.security_attempts < 3)
    OR (a.security_threat <= 3.0 AND rs.interest_score IS NULL)
  );

-- name: GetUnsummarizedScoredArticles :many
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date
FROM articles a
LEFT JOIN article_summaries asumm ON asumm.article_id = a.id
WHERE a.security_threat <= @max_security_threat::double precision AND asumm.article_id IS NULL
ORDER BY a.published_date DESC
LIMIT @lim;

-- name: GetUnscreenedArticles :many
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date
FROM articles a
WHERE a.security_screened_at IS NULL AND a.security_attempts < 3
ORDER BY a.published_date DESC
LIMIT @lim;

-- name: GetUnscoredCurationArticles :many
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date
FROM articles a
JOIN user_feeds uf ON a.feed_id = uf.feed_id
LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = uf.user_id
WHERE uf.user_id = @user_id
  AND a.security_threat <= @max_security_threat::double precision
  AND rs.interest_score IS NULL
ORDER BY a.published_date DESC
LIMIT @lim;

-- name: GetUngroupedEmbeddedArticles :many
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date
FROM articles a
JOIN user_feeds uf ON a.feed_id = uf.feed_id
JOIN article_embeddings ae ON ae.article_id = a.id
    AND ae.embedding_model = @model AND ae.status = @status
WHERE uf.user_id = @user_id
  AND a.security_threat <= @max_security_threat::double precision
  AND COALESCE(a.published_date, a.fetched_date) >= @since::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM article_group_members agm
      JOIN article_groups ag ON agm.group_id = ag.id
      WHERE agm.article_id = a.id AND ag.user_id = uf.user_id
  )
ORDER BY COALESCE(a.published_date, a.fetched_date) DESC
LIMIT @lim;

-- name: GetUnsummarizedArticleCount :one
SELECT COUNT(*)::int
FROM articles a
LEFT JOIN article_summaries asumm ON asumm.article_id = a.id
WHERE asumm.article_id IS NULL;

-- name: GetArticlesNeedingFullText :many
SELECT id, feed_id, guid, title, url, content, summary, author,
       published_date, fetched_date
FROM articles
WHERE full_text_fetched = FALSE
ORDER BY fetched_date DESC
LIMIT @lim;

-- name: UpdateArticleContent :exec
UPDATE articles SET content = @content::text WHERE id = @id;

-- name: UpdateArticleLinkedContent :exec
UPDATE articles SET linked_url = @linked_url, linked_content = @linked_content WHERE id = @id;

-- name: MarkArticleFullTextFetched :exec
UPDATE articles SET full_text_fetched = TRUE WHERE id = @id;

-- name: SetInterestScore :exec
INSERT INTO read_state (user_id, article_id, read, interest_score, ai_scored)
VALUES (@user_id, @article_id, FALSE, @interest_score::double precision, TRUE)
ON CONFLICT (user_id, article_id) DO UPDATE SET
  interest_score = excluded.interest_score,
  ai_scored = TRUE;

-- name: ScreenArticleSecurity :exec
UPDATE articles
SET security_threat = @security_threat::double precision,
    security_category = @security_category::text,
    security_verified = @security_verified,
    security_flagged = @security_flagged,
    security_screened_at = NOW()
WHERE id = @id;

-- name: SkipArticleSecurity :exec
-- Marks an article screened-but-skipped (screened_at set, threat left NULL). The
-- skip reason is herald-authored, not attacker text, but the column that held it
-- is gone (plan 012); screened_at + NULL threat already encodes the state.
UPDATE articles
SET security_screened_at = NOW()
WHERE id = @id;

-- name: IncrementArticleSecurityAttempts :exec
UPDATE articles SET security_attempts = security_attempts + 1 WHERE id = @id;
