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

-- name: ClaimUnscreenedArticles :many
-- Atomically claim up to @lim unscreened articles for a screener worker (#233):
-- pick rows not yet screened, within the retry budget, and either unclaimed or
-- whose prior claim lease has expired (@lease_seconds ago). FOR UPDATE SKIP
-- LOCKED means concurrent workers never contend on or double-grab a row. The
-- claim is stamped and the attempt counted here, not on failure, so a poison
-- article that keeps crashing a worker is bounded by security_attempts < 3
-- instead of being reclaimed forever; a clean backend-unavailable result refunds
-- the attempt (RefundSecurityClaim).
UPDATE articles a
SET screening_claimed_at = NOW(), security_attempts = a.security_attempts + 1
FROM (
    SELECT id FROM articles
    WHERE security_screened_at IS NULL
      AND security_attempts < 3
      AND (screening_claimed_at IS NULL
           OR screening_claimed_at < NOW() - make_interval(secs => @lease_seconds::double precision))
    ORDER BY published_date DESC
    LIMIT @lim
    FOR UPDATE SKIP LOCKED
) claimed
WHERE a.id = claimed.id
RETURNING a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
          a.author, a.published_date, a.fetched_date;

-- name: ReleaseSecurityClaim :exec
-- Clear the claim after a genuine (non-backend) screening failure so the row can
-- be retried without waiting for the lease to expire. The attempt was already
-- counted at claim time.
UPDATE articles SET screening_claimed_at = NULL WHERE id = @id;

-- name: RefundSecurityClaim :exec
-- Clear the claim AND return the attempt after a backend-unavailable result: we
-- never got a verdict, so an olla outage must not burn the retry budget (#100).
UPDATE articles
SET screening_claimed_at = NULL,
    security_attempts = GREATEST(security_attempts - 1, 0)
WHERE id = @id;

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
-- score_model/prompt_hash are captured here, at scoring time, because this is
-- the only point where which model and prompt produced the score is known for
-- certain (#258). Reconstructing them later reads back whatever is current, not
-- what was in force.
INSERT INTO read_state (user_id, article_id, read, interest_score, ai_scored, score_model, prompt_hash)
VALUES (@user_id, @article_id, FALSE, @interest_score::double precision, TRUE,
        NULLIF(@score_model::text, ''), NULLIF(@prompt_hash::text, ''))
ON CONFLICT (user_id, article_id) DO UPDATE SET
  interest_score = excluded.interest_score,
  ai_scored = TRUE,
  score_model = excluded.score_model,
  prompt_hash = excluded.prompt_hash;

-- name: ScreenArticleSecurity :exec
UPDATE articles
SET security_threat = @security_threat::double precision,
    security_category = @security_category::text,
    security_verified = @security_verified,
    security_flagged = @security_flagged,
    security_screened_at = NOW(),
    screening_claimed_at = NULL
WHERE id = @id;

-- name: SkipArticleSecurity :exec
-- Marks an article screened-but-skipped (screened_at set, threat left NULL). The
-- skip reason is herald-authored, not attacker text, but the column that held it
-- is gone (plan 012); screened_at + NULL threat already encodes the state.
UPDATE articles
SET security_screened_at = NOW(),
    screening_claimed_at = NULL
WHERE id = @id;

-- name: IncrementArticleSecurityAttempts :exec
UPDATE articles SET security_attempts = security_attempts + 1 WHERE id = @id;

-- name: GetScreenedArticleSample :many
-- A random sample of already-screened articles that still have content, for the
-- plan-012 score-comparison harness (herald screen-compare). Diagnostic only.
SELECT id, title, content, security_threat
FROM articles
WHERE security_screened_at IS NOT NULL AND content <> ''
ORDER BY RANDOM()
LIMIT @lim;

-- name: GetLowSafetyArticleSample :many
-- The lowest-scoring screened articles (worst stored verdict first), for the
-- plan-012 harness's --unsafe-first mode. Pre-rescore the stored column still
-- holds the old 10=safe value, so ASC surfaces what prod flagged. Diagnostic only.
SELECT id, title, content, security_threat
FROM articles
WHERE security_screened_at IS NOT NULL AND content <> '' AND security_threat IS NOT NULL
ORDER BY security_threat ASC, RANDOM()
LIMIT @lim;

-- name: GetReaderPipelineCounts :one
-- Reader-page status gauge (#232): partition the user's in-view article set into
-- three DISJOINT states for an at-a-glance donut --
--   pending (grey)  fetched but not yet screened (pipeline behind)
--   ready   (yellow) screened, security-passed, still unread (the inbox)
--   read    (green)  the user has read it
-- Blocked articles (screened but over the threat ceiling) are omitted: they are
-- not part of the user's reading, so they do not belong in the ring. Scoped to
-- the user's subscribed feeds, optionally one feed (@feed_id = 0 for all), and
-- bounded to a recent window (@since, on fetched_date) so the counts track
-- current flow rather than lifetime totals.
SELECT
  COUNT(*) FILTER (
    WHERE NOT COALESCE(rs.read, FALSE) AND a.security_screened_at IS NULL
  )::int AS pending,
  COUNT(*) FILTER (
    WHERE NOT COALESCE(rs.read, FALSE)
      AND a.security_screened_at IS NOT NULL
      AND a.security_threat IS NOT NULL
      AND a.security_threat <= @max_threat::double precision
  )::int AS ready,
  COUNT(*) FILTER (
    WHERE COALESCE(rs.read, FALSE)
  )::int AS read
FROM articles a
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id
LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = @user_id
WHERE a.fetched_date >= @since
  AND (@feed_id::bigint = 0 OR a.feed_id = @feed_id::bigint);
