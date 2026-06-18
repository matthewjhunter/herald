-- name: UserSubscribedToArticleFeed :one
SELECT EXISTS(
  SELECT 1 FROM articles a
  JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id
  WHERE a.id = @article_id
);

-- name: UpdateStarred :exec
INSERT INTO read_state (user_id, article_id, starred)
VALUES (@user_id, @article_id, @starred)
ON CONFLICT (user_id, article_id) DO UPDATE SET starred = excluded.starred;

-- name: UpsertReadStateScores :exec
-- AI pipeline: record scores, mark ai_scored, leave the user's read flag alone.
INSERT INTO read_state
  (user_id, article_id, read, interest_score, security_score, security_reason, security_flagged, ai_scored)
VALUES
  (@user_id, @article_id, FALSE, @interest_score, @security_score, @security_reason, COALESCE(@security_flagged, FALSE), TRUE)
ON CONFLICT (user_id, article_id) DO UPDATE SET
  interest_score = excluded.interest_score,
  security_score = excluded.security_score,
  security_reason = excluded.security_reason,
  security_flagged = COALESCE(excluded.security_flagged, read_state.security_flagged),
  ai_scored = TRUE;

-- name: UpsertReadStateRead :exec
INSERT INTO read_state (user_id, article_id, read, read_date)
VALUES (@user_id, @article_id, @read, NOW())
ON CONFLICT (user_id, article_id) DO UPDATE SET
  read = excluded.read,
  read_date = NOW();

-- name: GetScoreStats :many
SELECT
  f.id AS feed_id,
  COALESCE(uf.user_title, f.title) AS feed_title,
  COUNT(*) FILTER (WHERE a.security_screened_at IS NOT NULL)::int AS total_scored,
  COUNT(*) FILTER (WHERE a.security_score >= 7.0)::int AS sec_pass,
  COUNT(*) FILTER (WHERE a.security_score >= 4.0 AND a.security_score < 7.0)::int AS sec_borderline,
  COUNT(*) FILTER (WHERE a.security_score IS NOT NULL AND a.security_score < 4.0)::int AS sec_fail,
  COUNT(*) FILTER (WHERE a.security_score >= 7.0 AND rs.interest_score >= 8.0)::int AS int_high,
  COUNT(*) FILTER (WHERE a.security_score >= 7.0 AND rs.interest_score >= 5.0 AND rs.interest_score < 8.0)::int AS int_medium,
  COUNT(*) FILTER (WHERE a.security_score >= 7.0 AND rs.interest_score IS NOT NULL AND rs.interest_score < 5.0)::int AS int_low
FROM feeds f
JOIN user_feeds uf ON uf.feed_id = f.id AND uf.user_id = @user_id
JOIN articles a ON a.feed_id = f.id
LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = @user_id
GROUP BY f.id, uf.user_title, f.title
ORDER BY COALESCE(uf.user_title, f.title);

-- name: IncrementAIRetries :exec
INSERT INTO read_state (user_id, article_id, ai_retries)
VALUES (@user_id, @article_id, 1)
ON CONFLICT (user_id, article_id) DO UPDATE SET
  ai_retries = read_state.ai_retries + 1;

-- name: ResetReadStateScores :exec
UPDATE read_state SET ai_scored = FALSE, ai_retries = 0, interest_score = NULL
WHERE user_id = @user_id;

-- name: ResetArticleScores :execrows
UPDATE articles
SET security_score = NULL, security_reason = NULL, security_flagged = FALSE,
    security_screened_at = NULL, security_attempts = 0
WHERE feed_id IN (SELECT feed_id FROM user_feeds WHERE user_id = @user_id);

-- name: ResetArticleScoresBelow :execrows
UPDATE articles
SET security_score = NULL, security_reason = NULL, security_flagged = FALSE,
    security_screened_at = NULL, security_attempts = 0
WHERE feed_id IN (SELECT feed_id FROM user_feeds WHERE user_id = @user_id)
  AND security_score IS NOT NULL AND security_score < @below_score::double precision;
