-- name: GetArticleBacklinks :many
-- "Which of my feeds linked to this article?" -- articles in the user's
-- subscribed feeds with an outbound link (parsed from body/summary into
-- article_links) matching the target URL. @url_norm is the caller-normalized
-- target (urlnorm.Normalize), compared against the pre-normalized index, so the
-- match ignores scheme/www/query/fragment/trailing-slash differences. The target
-- article itself is excluded.
SELECT DISTINCT a.id, a.title, a.url,
       COALESCE(uf.user_title, f.title) AS feed_title,
       a.published_date, a.fetched_date
FROM article_links al
JOIN articles a ON a.id = al.article_id
JOIN feeds f ON f.id = a.feed_id
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id
WHERE al.url_norm = @url_norm::text
  AND a.id <> @exclude_id
ORDER BY a.published_date DESC NULLS LAST, a.fetched_date DESC
LIMIT @lim;
