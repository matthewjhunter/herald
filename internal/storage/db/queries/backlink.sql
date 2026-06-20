-- name: GetArticleBacklinks :many
-- "Which of my feeds linked to this?" -- articles in the user's subscribed
-- feeds with an outbound link (parsed from body/summary into article_links)
-- whose normalized form contains @needle as a substring. Substring (not prefix)
-- so a domain finds links to any of its subdomains too ("substack.com" ->
-- links to every *.substack.com publication) and a partial URL still matches;
-- the goal is to find links, not to exclude non-exact ones. Both sides are
-- lower-cased and stripped (urlnorm), so matching is case-insensitive and
-- ignores scheme/www/query/fragment/trailing-slash. The target article itself
-- is excluded.
SELECT DISTINCT a.id, a.title, a.url,
       COALESCE(uf.user_title, f.title) AS feed_title,
       a.published_date, a.fetched_date
FROM article_links al
JOIN articles a ON a.id = al.article_id
JOIN feeds f ON f.id = a.feed_id
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id
WHERE strpos(al.url_norm, @needle::text) > 0
  AND a.id <> @exclude_id
ORDER BY a.published_date DESC NULLS LAST, a.fetched_date DESC
LIMIT @lim;

-- name: GetArticleBacklinksExact :many
-- "Which of my feeds linked to THIS exact article?" -- the article-view variant
-- of GetArticleBacklinks. Matches @needle (a full urlnorm.Normalize key) for
-- EQUALITY, not as a substring, so a post whose identity lives in the bare host
-- (e.g. a WordPress ?p= site whose key is "blog.example?p=58410") does not match
-- every other link to the same host. The target article itself is excluded.
SELECT DISTINCT a.id, a.title, a.url,
       COALESCE(uf.user_title, f.title) AS feed_title,
       a.published_date, a.fetched_date
FROM article_links al
JOIN articles a ON a.id = al.article_id
JOIN feeds f ON f.id = a.feed_id
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id
WHERE al.url_norm = @needle::text
  AND a.id <> @exclude_id
ORDER BY a.published_date DESC NULLS LAST, a.fetched_date DESC
LIMIT @lim;
