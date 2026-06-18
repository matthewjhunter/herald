-- name: GetArticleBacklinks :many
-- "Which of my feeds linked to this article?" -- find link-blog posts in the
-- user's subscribed feeds whose extracted outbound link (articles.linked_url,
-- populated by extractLinkPostURL at full-text time) points at the target
-- article's URL. Both sides are normalized identically in SQL -- lowercased,
-- scheme + leading "www." stripped, query string and fragment dropped, trailing
-- slash removed -- so session/tracking params (e.g. ?r=...&triedRedirect=true)
-- and http/https/www differences don't cause misses. The target itself is
-- excluded. (FTS can't do this: the parser drops href URLs as HTML tags.)
WITH target AS (
  SELECT rtrim(
           regexp_replace(
             split_part(split_part(lower(@target_url::text), '#', 1), '?', 1),
             '^https?://(www\.)?', ''),
           '/') AS norm
)
SELECT a.id, a.title, a.url,
       COALESCE(uf.user_title, f.title) AS feed_title,
       a.published_date, a.fetched_date
FROM articles a
JOIN feeds f ON f.id = a.feed_id
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id
WHERE a.id <> @exclude_id
  AND a.linked_url <> ''
  AND rtrim(regexp_replace(split_part(split_part(lower(a.linked_url), '#', 1), '?', 1),
            '^https?://(www\.)?', ''), '/') = (SELECT norm FROM target)
ORDER BY a.published_date DESC NULLS LAST, a.fetched_date DESC
LIMIT @lim;
