-- name: SearchArticlesFTS :many
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date,
       COALESCE(a.security_flagged, FALSE) AS security_flagged,
       COALESCE(rs.read, FALSE) AS is_read, COALESCE(rs.starred, FALSE) AS is_starred
FROM articles a
JOIN user_feeds uf ON a.feed_id = uf.feed_id
LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = @user_id
WHERE uf.user_id = @user_id AND a.search_vector @@ websearch_to_tsquery('english', @query)
ORDER BY ts_rank_cd(a.search_vector, websearch_to_tsquery('english', @query)) DESC
LIMIT @lim OFFSET @off;
