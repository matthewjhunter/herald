-- name: SetFeverCredential :exec
INSERT INTO fever_credentials (user_id, api_key) VALUES (@user_id, @api_key)
ON CONFLICT (user_id) DO UPDATE SET api_key = excluded.api_key;

-- name: GetUserByFeverAPIKey :one
SELECT u.* FROM users u
JOIN fever_credentials fc ON fc.user_id = u.id
WHERE fc.api_key = @api_key;

-- name: GetFeverAPIKey :one
SELECT api_key FROM fever_credentials WHERE user_id = @user_id;

-- name: DeleteFeverCredential :exec
DELETE FROM fever_credentials WHERE user_id = @user_id;

-- name: GetFeverItemsByIDs :many
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date,
       COALESCE(rs.read, FALSE) AS is_read,
       COALESCE(rs.starred, FALSE) AS is_starred
FROM articles a
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id
LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = @user_id
WHERE a.id = ANY(@article_ids::bigint[])
ORDER BY a.id DESC LIMIT @lim;

-- name: GetFeverItemsRange :many
-- since_id / max_id of 0 disable that bound (the Fever API's default).
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date,
       COALESCE(rs.read, FALSE) AS is_read,
       COALESCE(rs.starred, FALSE) AS is_starred
FROM articles a
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id
LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = @user_id
WHERE (@since_id::bigint = 0 OR a.id > @since_id::bigint)
  AND (@max_id::bigint = 0 OR a.id <= @max_id::bigint)
ORDER BY a.id DESC LIMIT @lim;

-- name: GetFeverItemCount :one
SELECT COUNT(*)::int
FROM articles a
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id;

-- name: GetUnreadArticleIDsForUser :many
SELECT a.id
FROM articles a
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id
LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = @user_id
WHERE NOT COALESCE(rs.read, FALSE)
ORDER BY a.id;

-- name: GetStarredArticleIDsForUser :many
SELECT article_id FROM read_state
WHERE user_id = @user_id AND starred = TRUE
ORDER BY article_id;

-- name: MarkFeedArticlesRead :exec
-- filter_before = false marks every article; otherwise only those at or before
-- @before (or with no publish date).
INSERT INTO read_state (user_id, article_id, read, read_date)
SELECT @user_id, a.id, TRUE, NOW()
FROM articles a
WHERE a.feed_id = @feed_id
  AND (@filter_before::bool = FALSE OR a.published_date IS NULL OR a.published_date <= @before::timestamptz)
ON CONFLICT (user_id, article_id) DO UPDATE SET read = TRUE, read_date = NOW();

-- name: MarkGroupArticlesRead :exec
INSERT INTO read_state (user_id, article_id, read, read_date)
SELECT @user_id, a.id, TRUE, NOW()
FROM articles a
JOIN article_group_members agm ON agm.article_id = a.id
WHERE agm.group_id = @group_id
  AND (@filter_before::bool = FALSE OR a.published_date IS NULL OR a.published_date <= @before::timestamptz)
ON CONFLICT (user_id, article_id) DO UPDATE SET read = TRUE, read_date = NOW();

-- name: MarkAllArticlesRead :exec
INSERT INTO read_state (user_id, article_id, read, read_date)
SELECT @user_id, a.id, TRUE, NOW()
FROM articles a
JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = @user_id
WHERE (@filter_before::bool = FALSE OR a.published_date IS NULL OR a.published_date <= @before::timestamptz)
ON CONFLICT (user_id, article_id) DO UPDATE SET read = TRUE, read_date = NOW();

-- name: GetFeverLinks :many
SELECT
    ag.id AS group_id,
    agm.article_id,
    a.feed_id,
    a.title,
    a.url,
    (CASE WHEN COALESCE(rs.starred, FALSE) THEN 1 ELSE 0 END)::int AS is_saved,
    COALESCE(gs.max_interest_score, 0)::double precision AS score
FROM article_groups ag
JOIN article_group_members agm ON agm.group_id = ag.id
JOIN articles a ON a.id = agm.article_id
LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = @user_id
LEFT JOIN group_summaries gs ON gs.group_id = ag.id
WHERE ag.user_id = @user_id
ORDER BY ag.updated_at DESC, agm.added_at ASC;

-- name: GetFeedGroupMemberships :many
SELECT agm.group_id, a.feed_id
FROM article_group_members agm
JOIN articles a ON a.id = agm.article_id
JOIN article_groups ag ON ag.id = agm.group_id
WHERE ag.user_id = @user_id
GROUP BY agm.group_id, a.feed_id
ORDER BY agm.group_id, a.feed_id;
