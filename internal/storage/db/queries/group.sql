-- name: CreateArticleGroup :one
INSERT INTO article_groups (user_id, topic) VALUES (@user_id, @topic) RETURNING id;

-- name: AddArticleGroupMember :exec
INSERT INTO article_group_members (group_id, article_id)
VALUES (@group_id, @article_id)
ON CONFLICT DO NOTHING;

-- name: TouchArticleGroup :exec
UPDATE article_groups SET updated_at = NOW() WHERE id = @id;

-- name: GetGroupArticles :many
SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
       a.author, a.published_date, a.fetched_date
FROM articles a
JOIN article_group_members agm ON a.id = agm.article_id
WHERE agm.group_id = @group_id
ORDER BY a.published_date DESC;

-- name: GetArticleInterestScores :many
SELECT article_id, interest_score FROM read_state
WHERE user_id = @user_id
  AND article_id = ANY(@article_ids::bigint[])
  AND interest_score IS NOT NULL;

-- name: GetArticleSecurityScores :many
SELECT id, security_score FROM articles
WHERE id = ANY(@article_ids::bigint[])
  AND security_score IS NOT NULL;

-- name: UpdateGroupSummary :exec
INSERT INTO group_summaries (group_id, headline, summary, article_count, max_interest_score, generated_at)
VALUES (@group_id, @headline, @summary, @article_count, @max_interest_score, NOW())
ON CONFLICT (group_id) DO UPDATE SET
  headline = excluded.headline,
  summary = excluded.summary,
  article_count = excluded.article_count,
  max_interest_score = excluded.max_interest_score,
  generated_at = NOW();

-- name: GetGroupSummary :one
SELECT group_id, headline, summary, article_count, max_interest_score, generated_at
FROM group_summaries WHERE group_id = @group_id;

-- name: GetUserGroups :many
SELECT ag.id, ag.user_id, ag.topic, ag.display_name, ag.muted, ag.created_at, ag.updated_at
FROM article_groups ag
WHERE ag.user_id = @user_id
  AND (SELECT COUNT(*) FROM article_group_members WHERE group_id = ag.id) >= 2
ORDER BY ag.updated_at DESC;

-- name: GetGroup :one
SELECT id, user_id, topic, display_name, muted, created_at, updated_at
FROM article_groups WHERE id = @id;

-- name: FindArticleGroup :one
SELECT agm.group_id FROM article_group_members agm
JOIN article_groups ag ON agm.group_id = ag.id
WHERE agm.article_id = @article_id AND ag.user_id = @user_id;

-- name: GetGroupStats :many
SELECT ag.id AS group_id,
       COALESCE(ag.display_name, ag.topic) AS display_name,
       SUM(CASE WHEN rs.read IS NULL OR rs.read = FALSE THEN 1 ELSE 0 END)::int AS unread_articles
FROM article_groups ag
JOIN article_group_members agm ON agm.group_id = ag.id
LEFT JOIN read_state rs ON rs.article_id = agm.article_id AND rs.user_id = @user_id
WHERE ag.user_id = @user_id AND ag.muted = FALSE
GROUP BY ag.id
HAVING COUNT(agm.article_id) >= 2
   AND SUM(CASE WHEN rs.read IS NULL OR rs.read = FALSE THEN 1 ELSE 0 END) > 0
ORDER BY COALESCE(ag.display_name, ag.topic);

-- name: SetGroupMuted :exec
UPDATE article_groups SET muted = @muted WHERE id = @id;

-- name: IsGroupMuted :one
SELECT muted FROM article_groups WHERE id = @id;

-- name: DisbandGroup :exec
DELETE FROM article_groups WHERE id = @id;

-- name: UpdateGroupDisplayName :exec
UPDATE article_groups SET display_name = @display_name WHERE id = @id;

-- name: UpdateGroupEmbedding :exec
UPDATE article_groups SET embedding = @embedding, embedding_model = @embedding_model WHERE id = @id;

-- name: GetGroupsWithEmbeddings :many
SELECT id, user_id, topic, display_name, muted, embedding, created_at, updated_at
FROM article_groups
WHERE user_id = @user_id AND embedding IS NOT NULL AND embedding_model = @embedding_model;

-- name: GetGroupEmbedding :one
SELECT embedding FROM article_groups WHERE id = @id;

-- name: GetGroupArticleCount :one
SELECT COUNT(*)::int FROM article_group_members WHERE group_id = @group_id;

-- name: UpdateGroupTopic :exec
UPDATE article_groups SET topic = @topic WHERE id = @id;
