-- name: GetProcessingFunnel :one
SELECT
  COUNT(*)::int AS total_articles,
  COALESCE(SUM(CASE WHEN a.security_screened_at IS NOT NULL THEN 1 ELSE 0 END), 0)::int AS scored,
  COALESCE(SUM(CASE WHEN a.security_screened_at IS NULL AND a.security_attempts < 3 THEN 1 ELSE 0 END), 0)::int AS pending,
  COALESCE(SUM(CASE WHEN a.security_screened_at IS NULL AND a.security_attempts >= 3 THEN 1 ELSE 0 END), 0)::int AS stuck,
  COALESCE(SUM(CASE WHEN a.security_score >= 7 THEN 1 ELSE 0 END), 0)::int AS security_passed,
  COALESCE(SUM(CASE WHEN a.security_score IS NOT NULL AND a.security_score < 7 THEN 1 ELSE 0 END), 0)::int AS security_rejected,
  COALESCE(SUM(CASE WHEN a.security_screened_at IS NOT NULL AND a.security_score IS NULL THEN 1 ELSE 0 END), 0)::int AS security_skipped,
  COALESCE(SUM(CASE WHEN rs.interest_score IS NOT NULL THEN 1 ELSE 0 END), 0)::int AS curated
FROM articles a
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id
LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = @user_id;

-- name: GetProcessingSummaryCounts :one
-- Summaries are per-article and shared by all subscribers (#162), so these two
-- funnel numbers are global, like the security columns above.
SELECT
  (SELECT COUNT(*) FROM article_summaries WHERE ai_summary <> '')::int AS summarized,
  (SELECT COUNT(*) FROM article_summaries WHERE COALESCE(skip_reason, '') <> '')::int AS summarize_skipped;

-- name: GetProcessingFeedCounts :one
SELECT
  (SELECT COUNT(*) FROM user_feeds WHERE user_feeds.user_id = @user_id)::int AS feeds_total,
  (SELECT COUNT(*) FROM feeds f
   JOIN user_feeds uf ON uf.feed_id = f.id
   WHERE uf.user_id = @user_id AND f.consecutive_errors > 0)::int AS feeds_erroring;

-- name: RecordCycleStats :exec
INSERT INTO cycle_stats
  (completed_at, duration_ms, feeds_total, feeds_downloaded, feeds_not_modified,
   feeds_errored, new_articles, processed, high_interest, ai_backend_available)
VALUES (@completed_at, @duration_ms, @feeds_total, @feeds_downloaded, @feeds_not_modified,
        @feeds_errored, @new_articles, @processed, @high_interest, @ai_backend_available);

-- name: PruneCycleStats :exec
-- Keep the most recent 500 cycles (~10 days at a 30m cadence).
DELETE FROM cycle_stats WHERE id NOT IN (
  SELECT id FROM cycle_stats ORDER BY completed_at DESC LIMIT 500);

-- name: GetRecentCycleStats :many
SELECT * FROM cycle_stats ORDER BY completed_at DESC LIMIT @lim;

-- name: GetFeedStats :many
SELECT
  f.id AS feed_id,
  COALESCE(uf.user_title, f.title) AS feed_title,
  COUNT(a.id)::int AS total_articles,
  COALESCE(SUM(CASE WHEN (rs.read IS NULL OR rs.read = FALSE)
           AND NOT EXISTS (
             SELECT 1 FROM article_group_members agm
             JOIN article_groups ag ON agm.group_id = ag.id
             WHERE agm.article_id = a.id AND ag.user_id = uf.user_id
           ) THEN 1 ELSE 0 END), 0)::int AS unread_articles,
  (COUNT(a.id) - COUNT(asumm.article_id))::int AS unsummarized_articles,
  MAX(a.published_date) AS last_post_date
FROM feeds f
JOIN user_feeds uf ON uf.feed_id = f.id AND uf.user_id = @user_id
JOIN articles a ON a.feed_id = f.id
LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = @user_id
LEFT JOIN article_summaries asumm ON asumm.article_id = a.id
GROUP BY f.id, uf.user_title
ORDER BY COALESCE(uf.user_title, f.title);

-- name: GetFeedStatsForDB :many
SELECT
  f.id, f.title, f.url, f.status,
  COUNT(DISTINCT a.id)::int       AS articles,
  COUNT(DISTINCT uf.user_id)::int AS subscribers
FROM feeds f
LEFT JOIN articles   a  ON a.feed_id  = f.id
LEFT JOIN user_feeds uf ON uf.feed_id = f.id
GROUP BY f.id
ORDER BY articles DESC;

-- name: CountUsers :one
SELECT COUNT(*)::int FROM users;
