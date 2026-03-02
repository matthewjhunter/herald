package storage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// FeverItemRow is an article enriched with per-user read/starred state,
// as needed by the Fever API items endpoint.
type FeverItemRow struct {
	Article
	IsRead    bool
	IsStarred bool
}

// SetFeverCredential stores the MD5(email:password) API key for a user.
// Replaces any existing credential.
func (s *SQLiteStore) SetFeverCredential(userID int64, apiKey string) error {
	_, err := s.db.Exec(`
		INSERT INTO fever_credentials (user_id, api_key) VALUES (?, ?)
		ON CONFLICT(user_id) DO UPDATE SET api_key = excluded.api_key`,
		userID, apiKey)
	return err
}

// GetUserByFeverAPIKey returns the user whose Fever api_key matches, or
// sql.ErrNoRows if no match.
func (s *SQLiteStore) GetUserByFeverAPIKey(apiKey string) (*User, error) {
	var u User
	var email sql.NullString
	var oidcSub sql.NullString
	err := s.db.QueryRow(`
		SELECT u.id, u.name, u.oidc_sub, u.email, u.created_at
		FROM users u
		JOIN fever_credentials fc ON fc.user_id = u.id
		WHERE fc.api_key = ?`, apiKey).Scan(
		&u.ID, &u.Name, &oidcSub, &email, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	if email.Valid {
		u.Email = &email.String
	}
	if oidcSub.Valid {
		u.OIDCSub = &oidcSub.String
	}
	return &u, nil
}

// GetFeverItems returns articles for a user's subscribed feeds with per-user
// read/starred state. Pagination:
//   - sinceID > 0: articles with id > sinceID (newest items after a cursor)
//   - maxID > 0:   articles with id <= maxID  (page backward)
//   - withIDs:     fetch specific article IDs only (overrides sinceID/maxID)
//
// Results are ordered by id DESC; at most limit items returned (max 50).
func (s *SQLiteStore) GetFeverItems(userID int64, sinceID, maxID int64, withIDs []int64, limit int) ([]FeverItemRow, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}

	args := []any{userID, userID}
	q := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(rs.read, 0)    AS is_read,
		       COALESCE(rs.starred, 0) AS is_starred
		FROM articles a
		JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = ?
		LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = ?`

	if len(withIDs) > 0 {
		placeholders := make([]string, len(withIDs))
		for i, id := range withIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		q += ` WHERE a.id IN (` + strings.Join(placeholders, ",") + `)`
	} else {
		q += ` WHERE 1=1`
		if sinceID > 0 {
			q += ` AND a.id > ?`
			args = append(args, sinceID)
		}
		if maxID > 0 {
			q += ` AND a.id <= ?`
			args = append(args, maxID)
		}
	}

	q += ` ORDER BY a.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("fever items: %w", err)
	}
	defer rows.Close()

	var results []FeverItemRow
	for rows.Next() {
		var r FeverItemRow
		if err := rows.Scan(
			&r.ID, &r.FeedID, &r.GUID, &r.Title, &r.URL,
			&r.Content, &r.Summary, &r.Author,
			&r.PublishedDate, &r.FetchedDate,
			&r.IsRead, &r.IsStarred,
		); err != nil {
			return nil, fmt.Errorf("fever items scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// GetFeverItemCount returns the total number of articles available to a user
// across all subscribed feeds.
func (s *SQLiteStore) GetFeverItemCount(userID int64) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*)
		FROM articles a
		JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = ?`, userID).Scan(&count)
	return count, err
}

// GetUnreadArticleIDsForUser returns IDs of all unread articles for a user,
// ordered by id ascending. Used by the Fever &unread_item_ids endpoint.
func (s *SQLiteStore) GetUnreadArticleIDsForUser(userID int64) ([]int64, error) {
	rows, err := s.db.Query(`
		SELECT a.id
		FROM articles a
		JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = ?
		LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = ?
		WHERE COALESCE(rs.read, 0) = 0
		ORDER BY a.id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetStarredArticleIDsForUser returns IDs of all starred articles for a user,
// ordered by id ascending. Used by the Fever &saved_item_ids endpoint.
func (s *SQLiteStore) GetStarredArticleIDsForUser(userID int64) ([]int64, error) {
	rows, err := s.db.Query(`
		SELECT article_id FROM read_state
		WHERE user_id = ? AND starred = 1
		ORDER BY article_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkFeedArticlesRead marks all articles in a feed as read for a user,
// where published_date <= time.Unix(before, 0). before=0 marks everything.
func (s *SQLiteStore) MarkFeedArticlesRead(userID, feedID int64, before int64) error {
	var beforeCond string
	args := []any{userID, feedID}
	if before > 0 {
		beforeCond = `AND (a.published_date IS NULL OR a.published_date <= ?)`
		args = append(args, time.Unix(before, 0))
	}
	_, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO read_state (user_id, article_id, read, read_date)
		SELECT ?, a.id, 1, CURRENT_TIMESTAMP
		FROM articles a
		WHERE a.feed_id = ? %s
		ON CONFLICT(user_id, article_id) DO UPDATE SET read = 1, read_date = CURRENT_TIMESTAMP`,
		beforeCond), args...)
	return err
}

// MarkGroupArticlesRead marks all articles in an article group as read for a
// user, where published_date <= time.Unix(before, 0). before=0 marks everything.
func (s *SQLiteStore) MarkGroupArticlesRead(userID, groupID int64, before int64) error {
	var beforeCond string
	args := []any{userID, groupID}
	if before > 0 {
		beforeCond = `AND (a.published_date IS NULL OR a.published_date <= ?)`
		args = append(args, time.Unix(before, 0))
	}
	_, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO read_state (user_id, article_id, read, read_date)
		SELECT ?, a.id, 1, CURRENT_TIMESTAMP
		FROM articles a
		JOIN article_group_members agm ON agm.article_id = a.id
		WHERE agm.group_id = ? %s
		ON CONFLICT(user_id, article_id) DO UPDATE SET read = 1, read_date = CURRENT_TIMESTAMP`,
		beforeCond), args...)
	return err
}

// MarkAllArticlesRead marks all articles as read for a user across all
// subscribed feeds, where published_date <= time.Unix(before, 0).
func (s *SQLiteStore) MarkAllArticlesRead(userID int64, before int64) error {
	var beforeCond string
	args := []any{userID, userID}
	if before > 0 {
		beforeCond = `AND (a.published_date IS NULL OR a.published_date <= ?)`
		args = append(args, time.Unix(before, 0))
	}
	_, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO read_state (user_id, article_id, read, read_date)
		SELECT ?, a.id, 1, CURRENT_TIMESTAMP
		FROM articles a
		JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = ?
		WHERE 1=1 %s
		ON CONFLICT(user_id, article_id) DO UPDATE SET read = 1, read_date = CURRENT_TIMESTAMP`,
		beforeCond), args...)
	return err
}

// GetFeverAPIKey returns the stored Fever api_key for a user, or sql.ErrNoRows
// if the user has no Fever credential set.
func (s *SQLiteStore) GetFeverAPIKey(userID int64) (string, error) {
	var apiKey string
	err := s.db.QueryRow(`SELECT api_key FROM fever_credentials WHERE user_id = ?`, userID).Scan(&apiKey)
	return apiKey, err
}

// DeleteFeverCredential removes the Fever API credential for a user.
func (s *SQLiteStore) DeleteFeverCredential(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM fever_credentials WHERE user_id = ?`, userID)
	return err
}

// GetFeedGroupMemberships returns a map from article_group_id to the set of
// feed IDs whose articles appear in that group. Used to build the Fever
// feeds_groups response field.
func (s *SQLiteStore) GetFeedGroupMemberships(userID int64) (map[int64][]int64, error) {
	rows, err := s.db.Query(`
		SELECT agm.group_id, a.feed_id
		FROM article_group_members agm
		JOIN articles a ON a.id = agm.article_id
		JOIN article_groups ag ON ag.id = agm.group_id
		WHERE ag.user_id = ?
		GROUP BY agm.group_id, a.feed_id
		ORDER BY agm.group_id, a.feed_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64][]int64)
	for rows.Next() {
		var groupID, feedID int64
		if err := rows.Scan(&groupID, &feedID); err != nil {
			return nil, err
		}
		result[groupID] = append(result[groupID], feedID)
	}
	return result, rows.Err()
}
