package storage

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Feed struct {
	ID           int64
	URL          string
	Title        string
	Description  string
	LastFetched  *time.Time
	LastError    *string
	ETag         string
	LastModified string
	Enabled      bool
	CreatedAt    time.Time
}

type Article struct {
	ID            int64
	FeedID        int64
	GUID          string
	Title         string
	URL           string
	Content       string
	Summary       string
	Author        string
	PublishedDate *time.Time
	FetchedDate   time.Time
}

type ArticleSummary struct {
	UserID      int64
	ArticleID   int64
	AISummary   string
	GeneratedAt time.Time
}

type ReadState struct {
	ArticleID     int64
	Read          bool
	Starred       bool
	InterestScore *float64
	SecurityScore *float64
	ReadDate      *time.Time
}

type ArticleGroup struct {
	ID        int64
	UserID    int64
	Topic     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type GroupSummary struct {
	GroupID          int64
	Summary          string
	ArticleCount     int
	MaxInterestScore *float64
	GeneratedAt      time.Time
}

type UserPrompt struct {
	UserID         int64
	PromptType     string
	PromptTemplate string
	Temperature    *float64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewStore creates a new database connection and initializes the schema
func NewStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_time_format=sqlite")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Enable foreign keys
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to enable foreign keys: %w", err)
	}

	// Initialize schema
	if _, err := db.Exec(Schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	// Migrations for existing databases.
	migrations := []string{
		"ALTER TABLE feeds ADD COLUMN last_error TEXT",
		"ALTER TABLE feeds ADD COLUMN etag TEXT",
		"ALTER TABLE feeds ADD COLUMN last_modified TEXT",
	}
	for _, m := range migrations {
		db.Exec(m) // ignore "duplicate column" errors
	}

	// Migrate read_state from single-column PK to composite (user_id, article_id) PK.
	// Detect old schema by checking whether user_id column exists.
	if needsReadStateMigration(db) {
		migrationSQL := `
			CREATE TABLE read_state_new (
				user_id INTEGER NOT NULL DEFAULT 1,
				article_id INTEGER NOT NULL,
				read BOOLEAN NOT NULL DEFAULT 0,
				starred BOOLEAN NOT NULL DEFAULT 0,
				interest_score REAL,
				security_score REAL,
				read_date DATETIME,
				PRIMARY KEY (user_id, article_id),
				FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
			);
			INSERT OR IGNORE INTO read_state_new
				(user_id, article_id, read, starred, interest_score, security_score, read_date)
				SELECT 1, article_id, read, starred, interest_score, security_score, read_date
				FROM read_state;
			DROP TABLE read_state;
			ALTER TABLE read_state_new RENAME TO read_state;
		`
		if _, err := db.Exec(migrationSQL); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to migrate read_state: %w", err)
		}
	}

	return &Store{db: db}, nil
}

// needsReadStateMigration checks whether the read_state table uses the old
// single-column PK (no user_id column). Returns false for fresh databases
// that already have the composite key schema.
func needsReadStateMigration(db *sql.DB) bool {
	rows, err := db.Query("PRAGMA table_info(read_state)")
	if err != nil {
		return false
	}
	defer rows.Close()

	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == "user_id" {
			return false
		}
	}
	return true // table exists but has no user_id column
}

// Close closes the database connection
func (s *Store) Close() error {
	return s.db.Close()
}

// User prompt management

// GetUserPrompt retrieves a user's custom prompt template
func (s *Store) GetUserPrompt(userID int64, promptType string) (string, error) {
	var promptTemplate string
	err := s.db.QueryRow(
		"SELECT prompt_template FROM user_prompts WHERE user_id = ? AND prompt_type = ?",
		userID, promptType,
	).Scan(&promptTemplate)

	if err != nil {
		return "", err
	}
	return promptTemplate, nil
}

// GetUserPromptTemperature retrieves a user's custom temperature setting
func (s *Store) GetUserPromptTemperature(userID int64, promptType string) (float64, error) {
	var temperature sql.NullFloat64
	err := s.db.QueryRow(
		"SELECT temperature FROM user_prompts WHERE user_id = ? AND prompt_type = ?",
		userID, promptType,
	).Scan(&temperature)

	if err != nil {
		return 0, err
	}

	if !temperature.Valid {
		return 0, nil
	}
	return temperature.Float64, nil
}

// SetUserPrompt sets a custom prompt template for a user
func (s *Store) SetUserPrompt(userID int64, promptType, promptTemplate string, temperature *float64) error {
	_, err := s.db.Exec(
		`INSERT INTO user_prompts (user_id, prompt_type, prompt_template, temperature, updated_at)
		 VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(user_id, prompt_type) DO UPDATE SET
		   prompt_template = excluded.prompt_template,
		   temperature = excluded.temperature,
		   updated_at = CURRENT_TIMESTAMP`,
		userID, promptType, promptTemplate, temperature,
	)
	return err
}

// DeleteUserPrompt removes a custom prompt, reverting to config/default
func (s *Store) DeleteUserPrompt(userID int64, promptType string) error {
	_, err := s.db.Exec(
		"DELETE FROM user_prompts WHERE user_id = ? AND prompt_type = ?",
		userID, promptType,
	)
	return err
}

// ListUserPrompts lists all custom prompts for a user
func (s *Store) ListUserPrompts(userID int64) ([]UserPrompt, error) {
	rows, err := s.db.Query(
		`SELECT prompt_type, prompt_template, temperature, created_at, updated_at
		 FROM user_prompts WHERE user_id = ? ORDER BY prompt_type`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prompts []UserPrompt
	for rows.Next() {
		var p UserPrompt
		var temp sql.NullFloat64
		err := rows.Scan(&p.PromptType, &p.PromptTemplate, &temp, &p.CreatedAt, &p.UpdatedAt)
		if err != nil {
			return nil, err
		}
		p.UserID = userID
		if temp.Valid {
			tempVal := temp.Float64
			p.Temperature = &tempVal
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
}

// User preference management

// GetUserPreference retrieves a single preference value for a user.
func (s *Store) GetUserPreference(userID int64, key string) (string, error) {
	var value string
	err := s.db.QueryRow(
		"SELECT value FROM user_preferences WHERE user_id = ? AND key = ?",
		userID, key,
	).Scan(&value)
	if err != nil {
		return "", err
	}
	return value, nil
}

// SetUserPreference sets a preference value, creating or updating as needed.
func (s *Store) SetUserPreference(userID int64, key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO user_preferences (user_id, key, value)
		 VALUES (?, ?, ?)
		 ON CONFLICT(user_id, key) DO UPDATE SET
		   value = excluded.value`,
		userID, key, value,
	)
	return err
}

// GetAllUserPreferences returns all preferences for a user as a key-value map.
func (s *Store) GetAllUserPreferences(userID int64) (map[string]string, error) {
	rows, err := s.db.Query(
		"SELECT key, value FROM user_preferences WHERE user_id = ?",
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("get user preferences: %w", err)
	}
	defer rows.Close()

	prefs := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan preference: %w", err)
		}
		prefs[k] = v
	}
	return prefs, rows.Err()
}

// DeleteUserPreference removes a single preference for a user.
func (s *Store) DeleteUserPreference(userID int64, key string) error {
	_, err := s.db.Exec(
		"DELETE FROM user_preferences WHERE user_id = ? AND key = ?",
		userID, key,
	)
	return err
}

// UpdateStarred sets the starred flag on an article's read state.
func (s *Store) UpdateStarred(articleID int64, starred bool) error {
	_, err := s.db.Exec(
		`INSERT INTO read_state (user_id, article_id, starred)
		 VALUES (1, ?, ?)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
		   starred = excluded.starred`,
		articleID, starred,
	)
	if err != nil {
		return fmt.Errorf("update starred: %w", err)
	}
	return nil
}

// AddFeed adds a new feed to the database
func (s *Store) AddFeed(url, title, description string) (int64, error) {
	result, err := s.db.Exec(
		"INSERT INTO feeds (url, title, description) VALUES (?, ?, ?)",
		url, title, description,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to add feed: %w", err)
	}
	return result.LastInsertId()
}

// GetAllFeeds returns all enabled feeds
func (s *Store) GetAllFeeds() ([]Feed, error) {
	rows, err := s.db.Query("SELECT id, url, title, description, last_fetched, last_error, etag, last_modified, enabled, created_at FROM feeds WHERE enabled = 1")
	if err != nil {
		return nil, fmt.Errorf("failed to get feeds: %w", err)
	}
	defer rows.Close()

	var feeds []Feed
	for rows.Next() {
		var f Feed
		var etag, lastMod sql.NullString
		if err := rows.Scan(&f.ID, &f.URL, &f.Title, &f.Description, &f.LastFetched, &f.LastError, &etag, &lastMod, &f.Enabled, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan feed: %w", err)
		}
		f.ETag = etag.String
		f.LastModified = lastMod.String
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

// UpdateFeedError records a fetch error for a feed.
func (s *Store) UpdateFeedError(feedID int64, errMsg string) error {
	_, err := s.db.Exec("UPDATE feeds SET last_error = ? WHERE id = ?", errMsg, feedID)
	if err != nil {
		return fmt.Errorf("failed to update feed error: %w", err)
	}
	return nil
}

// ClearFeedError clears the last error and updates last_fetched for a feed.
func (s *Store) ClearFeedError(feedID int64) error {
	_, err := s.db.Exec("UPDATE feeds SET last_error = NULL, last_fetched = CURRENT_TIMESTAMP WHERE id = ?", feedID)
	if err != nil {
		return fmt.Errorf("failed to clear feed error: %w", err)
	}
	return nil
}

// UpdateFeedCacheHeaders stores the HTTP cache headers from the last successful fetch.
func (s *Store) UpdateFeedCacheHeaders(feedID int64, etag, lastModified string) error {
	_, err := s.db.Exec("UPDATE feeds SET etag = ?, last_modified = ? WHERE id = ?", etag, lastModified, feedID)
	if err != nil {
		return fmt.Errorf("failed to update feed cache headers: %w", err)
	}
	return nil
}

// UpdateFeedLastFetched updates the last fetched timestamp for a feed
func (s *Store) UpdateFeedLastFetched(feedID int64) error {
	_, err := s.db.Exec("UPDATE feeds SET last_fetched = CURRENT_TIMESTAMP WHERE id = ?", feedID)
	if err != nil {
		return fmt.Errorf("failed to update feed last_fetched: %w", err)
	}
	return nil
}

// AddArticle adds a new article to the database
func (s *Store) AddArticle(article *Article) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO articles (feed_id, guid, title, url, content, summary, author, published_date)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(feed_id, guid) DO NOTHING`,
		article.FeedID, article.GUID, article.Title, article.URL,
		article.Content, article.Summary, article.Author, article.PublishedDate,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to add article: %w", err)
	}
	return result.LastInsertId()
}

// GetUnreadArticles returns all unread articles
func (s *Store) GetUnreadArticles(limit int) ([]Article, error) {
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		LEFT JOIN read_state rs ON a.id = rs.article_id
		WHERE rs.article_id IS NULL OR rs.read = 0
		ORDER BY a.published_date DESC
		LIMIT ?
	`
	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get unread articles: %w", err)
	}
	defer rows.Close()

	var articles []Article
	for rows.Next() {
		var a Article
		if err := rows.Scan(&a.ID, &a.FeedID, &a.GUID, &a.Title, &a.URL,
			&a.Content, &a.Summary, &a.Author, &a.PublishedDate, &a.FetchedDate); err != nil {
			return nil, fmt.Errorf("failed to scan article: %w", err)
		}
		articles = append(articles, a)
	}
	return articles, rows.Err()
}

// UpdateReadState updates or creates the read state for an article
func (s *Store) UpdateReadState(articleID int64, read bool, interestScore, securityScore *float64) error {
	_, err := s.db.Exec(
		`INSERT INTO read_state (user_id, article_id, read, interest_score, security_score, read_date)
		 VALUES (1, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
		   read = excluded.read,
		   interest_score = excluded.interest_score,
		   security_score = excluded.security_score,
		   read_date = CURRENT_TIMESTAMP`,
		articleID, read, interestScore, securityScore,
	)
	if err != nil {
		return fmt.Errorf("failed to update read state: %w", err)
	}
	return nil
}

// GetArticlesByInterestScore returns unread articles with interest scores above
// threshold, ordered by a time-decayed effective score. The decay formula is:
//
//	effective = interest_score * (1.0 / (1.0 + days_old * 0.1))
//
// This causes older articles to gradually sink in priority: a 10-day-old article
// is weighted at 50% of its raw score, 20-day at 33%, 30-day at 25%. The WHERE
// clause still filters on the raw score so legitimately interesting articles
// remain visible — they just sort lower as they age. Returned scores are the
// decayed effective scores, not the raw stored values.
func (s *Store) GetArticlesByInterestScore(threshold float64, limit, offset int) ([]Article, []float64, error) {
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(rs.interest_score, 0) * (1.0 / (1.0 + MAX(0, julianday('now') - julianday(COALESCE(a.published_date, a.fetched_date))) * 0.1)) AS decayed_score
		FROM articles a
		JOIN read_state rs ON a.id = rs.article_id
		WHERE rs.interest_score >= ? AND rs.read = 0
		ORDER BY decayed_score DESC
		LIMIT ? OFFSET ?
	`
	rows, err := s.db.Query(query, threshold, limit, offset)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get articles by interest score: %w", err)
	}
	defer rows.Close()

	var articles []Article
	var scores []float64
	for rows.Next() {
		var a Article
		var score float64
		if err := rows.Scan(&a.ID, &a.FeedID, &a.GUID, &a.Title, &a.URL,
			&a.Content, &a.Summary, &a.Author, &a.PublishedDate, &a.FetchedDate, &score); err != nil {
			return nil, nil, fmt.Errorf("failed to scan article: %w", err)
		}
		articles = append(articles, a)
		scores = append(scores, score)
	}
	return articles, scores, rows.Err()
}

// UpdateArticleAISummary stores the AI-generated summary for an article (per-user)
func (s *Store) UpdateArticleAISummary(userID, articleID int64, aiSummary string) error {
	_, err := s.db.Exec(
		`INSERT INTO article_summaries (user_id, article_id, ai_summary)
		 VALUES (?, ?, ?)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
		   ai_summary = excluded.ai_summary,
		   generated_at = CURRENT_TIMESTAMP`,
		userID, articleID, aiSummary,
	)
	if err != nil {
		return fmt.Errorf("failed to update AI summary: %w", err)
	}
	return nil
}

// GetArticleSummary retrieves the AI summary for an article for a specific user
func (s *Store) GetArticleSummary(userID, articleID int64) (*ArticleSummary, error) {
	var as ArticleSummary
	err := s.db.QueryRow(
		"SELECT user_id, article_id, ai_summary, generated_at FROM article_summaries WHERE user_id = ? AND article_id = ?",
		userID, articleID,
	).Scan(&as.UserID, &as.ArticleID, &as.AISummary, &as.GeneratedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get article summary: %w", err)
	}
	return &as, nil
}

// FeedStats holds per-feed article counts.
type FeedStats struct {
	FeedID               int64
	FeedTitle            string
	TotalArticles        int
	UnreadArticles       int
	UnsummarizedArticles int
}

// GetFeedStats returns article counts per feed for a user.
func (s *Store) GetFeedStats(userID int64) ([]FeedStats, error) {
	rows, err := s.db.Query(`
		SELECT f.id, f.title,
			COUNT(a.id),
			COUNT(a.id) - COALESCE(SUM(CASE WHEN rs.read = 1 THEN 1 ELSE 0 END), 0),
			COUNT(a.id) - COUNT(asumm.article_id)
		FROM feeds f
		JOIN user_feeds uf ON uf.feed_id = f.id AND uf.user_id = ?
		JOIN articles a ON a.feed_id = f.id
		LEFT JOIN read_state rs ON rs.article_id = a.id
		LEFT JOIN article_summaries asumm ON asumm.article_id = a.id AND asumm.user_id = ?
		GROUP BY f.id, f.title
		ORDER BY f.title`,
		userID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("get feed stats: %w", err)
	}
	defer rows.Close()

	var stats []FeedStats
	for rows.Next() {
		var fs FeedStats
		if err := rows.Scan(&fs.FeedID, &fs.FeedTitle, &fs.TotalArticles, &fs.UnreadArticles, &fs.UnsummarizedArticles); err != nil {
			return nil, fmt.Errorf("scan feed stats: %w", err)
		}
		stats = append(stats, fs)
	}
	return stats, rows.Err()
}

// CreateArticleGroup creates a new article group
func (s *Store) CreateArticleGroup(userID int64, topic string) (int64, error) {
	result, err := s.db.Exec(
		"INSERT INTO article_groups (user_id, topic) VALUES (?, ?)",
		userID, topic,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to create article group: %w", err)
	}
	return result.LastInsertId()
}

// AddArticleToGroup adds an article to a group
func (s *Store) AddArticleToGroup(groupID, articleID int64) error {
	_, err := s.db.Exec(
		"INSERT OR IGNORE INTO article_group_members (group_id, article_id) VALUES (?, ?)",
		groupID, articleID,
	)
	if err != nil {
		return fmt.Errorf("failed to add article to group: %w", err)
	}

	// Update group's updated_at timestamp
	_, err = s.db.Exec("UPDATE article_groups SET updated_at = CURRENT_TIMESTAMP WHERE id = ?", groupID)
	return err
}

// GetGroupArticles returns all articles in a group
func (s *Store) GetGroupArticles(groupID int64) ([]Article, error) {
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN article_group_members agm ON a.id = agm.article_id
		WHERE agm.group_id = ?
		ORDER BY a.published_date DESC
	`
	rows, err := s.db.Query(query, groupID)
	if err != nil {
		return nil, fmt.Errorf("failed to get group articles: %w", err)
	}
	defer rows.Close()

	var articles []Article
	for rows.Next() {
		var a Article
		if err := rows.Scan(&a.ID, &a.FeedID, &a.GUID, &a.Title, &a.URL,
			&a.Content, &a.Summary, &a.Author, &a.PublishedDate, &a.FetchedDate); err != nil {
			return nil, fmt.Errorf("failed to scan article: %w", err)
		}
		articles = append(articles, a)
	}
	return articles, rows.Err()
}

// UpdateGroupSummary stores or updates the summary for a group
func (s *Store) UpdateGroupSummary(groupID int64, summary string, articleCount int, maxInterestScore *float64) error {
	_, err := s.db.Exec(
		`INSERT INTO group_summaries (group_id, summary, article_count, max_interest_score, generated_at)
		 VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(group_id) DO UPDATE SET
		   summary = excluded.summary,
		   article_count = excluded.article_count,
		   max_interest_score = excluded.max_interest_score,
		   generated_at = CURRENT_TIMESTAMP`,
		groupID, summary, articleCount, maxInterestScore,
	)
	if err != nil {
		return fmt.Errorf("failed to update group summary: %w", err)
	}
	return nil
}

// GetGroupSummary retrieves the summary for a group
func (s *Store) GetGroupSummary(groupID int64) (*GroupSummary, error) {
	var gs GroupSummary
	err := s.db.QueryRow(
		"SELECT group_id, summary, article_count, max_interest_score, generated_at FROM group_summaries WHERE group_id = ?",
		groupID,
	).Scan(&gs.GroupID, &gs.Summary, &gs.ArticleCount, &gs.MaxInterestScore, &gs.GeneratedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to get group summary: %w", err)
	}
	return &gs, nil
}

// GetUserGroups returns all groups for a user
func (s *Store) GetUserGroups(userID int64) ([]ArticleGroup, error) {
	query := "SELECT id, user_id, topic, created_at, updated_at FROM article_groups WHERE user_id = ? ORDER BY updated_at DESC"
	rows, err := s.db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user groups: %w", err)
	}
	defer rows.Close()

	var groups []ArticleGroup
	for rows.Next() {
		var g ArticleGroup
		if err := rows.Scan(&g.ID, &g.UserID, &g.Topic, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan group: %w", err)
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// FindArticleGroup finds the group ID for an article, if it belongs to one
func (s *Store) FindArticleGroup(articleID, userID int64) (*int64, error) {
	var groupID int64
	err := s.db.QueryRow(
		`SELECT agm.group_id FROM article_group_members agm
		 JOIN article_groups ag ON agm.group_id = ag.id
		 WHERE agm.article_id = ? AND ag.user_id = ?`,
		articleID, userID,
	).Scan(&groupID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to find article group: %w", err)
	}
	return &groupID, nil
}

// SubscribeUserToFeed subscribes a user to a feed
func (s *Store) SubscribeUserToFeed(userID, feedID int64) error {
	_, err := s.db.Exec(
		"INSERT OR IGNORE INTO user_feeds (user_id, feed_id) VALUES (?, ?)",
		userID, feedID,
	)
	if err != nil {
		return fmt.Errorf("failed to subscribe user to feed: %w", err)
	}
	return nil
}

// GetUserFeeds returns all feeds a user is subscribed to
func (s *Store) GetUserFeeds(userID int64) ([]Feed, error) {
	query := `
		SELECT f.id, f.url, f.title, f.description, f.last_fetched, f.last_error, f.etag, f.last_modified, f.enabled, f.created_at
		FROM feeds f
		JOIN user_feeds uf ON f.id = uf.feed_id
		WHERE uf.user_id = ? AND f.enabled = 1
		ORDER BY f.title
	`
	rows, err := s.db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user feeds: %w", err)
	}
	defer rows.Close()

	var feeds []Feed
	for rows.Next() {
		var f Feed
		var etag, lastMod sql.NullString
		if err := rows.Scan(&f.ID, &f.URL, &f.Title, &f.Description, &f.LastFetched, &f.LastError, &etag, &lastMod, &f.Enabled, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan feed: %w", err)
		}
		f.ETag = etag.String
		f.LastModified = lastMod.String
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

// GetAllSubscribedFeeds returns all feeds that ANY user is subscribed to
func (s *Store) GetAllSubscribedFeeds() ([]Feed, error) {
	query := `
		SELECT DISTINCT f.id, f.url, f.title, f.description, f.last_fetched, f.last_error, f.etag, f.last_modified, f.enabled, f.created_at
		FROM feeds f
		JOIN user_feeds uf ON f.id = uf.feed_id
		WHERE f.enabled = 1
		ORDER BY f.title
	`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get subscribed feeds: %w", err)
	}
	defer rows.Close()

	var feeds []Feed
	for rows.Next() {
		var f Feed
		var etag, lastMod sql.NullString
		if err := rows.Scan(&f.ID, &f.URL, &f.Title, &f.Description, &f.LastFetched, &f.LastError, &etag, &lastMod, &f.Enabled, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan feed: %w", err)
		}
		f.ETag = etag.String
		f.LastModified = lastMod.String
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

// GetFeedSubscribers returns all user IDs subscribed to a feed
func (s *Store) GetFeedSubscribers(feedID int64) ([]int64, error) {
	rows, err := s.db.Query("SELECT user_id FROM user_feeds WHERE feed_id = ?", feedID)
	if err != nil {
		return nil, fmt.Errorf("failed to get feed subscribers: %w", err)
	}
	defer rows.Close()

	var userIDs []int64
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("failed to scan user ID: %w", err)
		}
		userIDs = append(userIDs, userID)
	}
	return userIDs, rows.Err()
}

// UnsubscribeUserFromFeed removes a user's subscription to a feed.
func (s *Store) UnsubscribeUserFromFeed(userID, feedID int64) error {
	_, err := s.db.Exec(
		"DELETE FROM user_feeds WHERE user_id = ? AND feed_id = ?",
		userID, feedID,
	)
	if err != nil {
		return fmt.Errorf("failed to unsubscribe user from feed: %w", err)
	}
	return nil
}

// DeleteFeedIfOrphaned deletes a feed only if no users are subscribed to it.
// Returns true if the feed was deleted. CASCADE handles articles, read_state,
// summaries, and group member cleanup.
func (s *Store) DeleteFeedIfOrphaned(feedID int64) (bool, error) {
	result, err := s.db.Exec(
		"DELETE FROM feeds WHERE id = ? AND NOT EXISTS (SELECT 1 FROM user_feeds WHERE feed_id = ?)",
		feedID, feedID,
	)
	if err != nil {
		return false, fmt.Errorf("failed to delete orphaned feed: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to check rows affected: %w", err)
	}
	return rows > 0, nil
}

// RenameFeed updates the display title of a feed.
func (s *Store) RenameFeed(feedID int64, title string) error {
	_, err := s.db.Exec("UPDATE feeds SET title = ? WHERE id = ?", title, feedID)
	if err != nil {
		return fmt.Errorf("failed to rename feed: %w", err)
	}
	return nil
}

// GetAllSubscribingUsers returns all user IDs that have feed subscriptions
func (s *Store) GetAllSubscribingUsers() ([]int64, error) {
	rows, err := s.db.Query("SELECT DISTINCT user_id FROM user_feeds ORDER BY user_id")
	if err != nil {
		return nil, fmt.Errorf("failed to get subscribing users: %w", err)
	}
	defer rows.Close()

	var userIDs []int64
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("failed to scan user ID: %w", err)
		}
		userIDs = append(userIDs, userID)
	}
	return userIDs, rows.Err()
}

// GetArticle returns a single article by ID.
func (s *Store) GetArticle(articleID int64) (*Article, error) {
	var a Article
	err := s.db.QueryRow(
		`SELECT id, feed_id, guid, title, url, content, summary,
		        author, published_date, fetched_date
		 FROM articles WHERE id = ?`, articleID,
	).Scan(&a.ID, &a.FeedID, &a.GUID, &a.Title, &a.URL,
		&a.Content, &a.Summary, &a.Author, &a.PublishedDate, &a.FetchedDate)
	if err != nil {
		return nil, fmt.Errorf("get article %d: %w", articleID, err)
	}
	return &a, nil
}

// GetUnscoredArticleCount returns the number of articles from the user's
// subscribed feeds that have no read_state entry (pending security/interest scoring).
func (s *Store) GetUnscoredArticleCount(userID int64) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*)
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id
		WHERE uf.user_id = ? AND rs.article_id IS NULL`,
		userID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get unscored article count: %w", err)
	}
	return count, nil
}

// GetUnsummarizedArticleCount returns the number of articles from the user's
// subscribed feeds that have no AI summary yet (pending content summarization).
func (s *Store) GetUnsummarizedArticleCount(userID int64) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*)
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN article_summaries asumm ON asumm.article_id = a.id AND asumm.user_id = ?
		WHERE uf.user_id = ? AND asumm.article_id IS NULL`,
		userID, userID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get unsummarized article count: %w", err)
	}
	return count, nil
}

// GetUnscoredArticlesForUser returns articles from the user's subscribed feeds
// that have no read_state entry (never been scored by the AI pipeline).
func (s *Store) GetUnscoredArticlesForUser(userID int64, limit int) ([]Article, error) {
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id
		WHERE uf.user_id = ? AND rs.article_id IS NULL
		ORDER BY a.published_date DESC
		LIMIT ?
	`
	rows, err := s.db.Query(query, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("get unscored articles for user: %w", err)
	}
	defer rows.Close()

	var articles []Article
	for rows.Next() {
		var a Article
		if err := rows.Scan(&a.ID, &a.FeedID, &a.GUID, &a.Title, &a.URL,
			&a.Content, &a.Summary, &a.Author, &a.PublishedDate, &a.FetchedDate); err != nil {
			return nil, fmt.Errorf("scan article: %w", err)
		}
		articles = append(articles, a)
	}
	return articles, rows.Err()
}

// GetUnreadArticlesForUser returns unread articles from feeds the user subscribes to
func (s *Store) GetUnreadArticlesForUser(userID int64, limit, offset int) ([]Article, error) {
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id
		WHERE uf.user_id = ? AND (rs.article_id IS NULL OR rs.read = 0)
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?
	`
	rows, err := s.db.Query(query, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to get unread articles for user: %w", err)
	}
	defer rows.Close()

	var articles []Article
	for rows.Next() {
		var a Article
		if err := rows.Scan(&a.ID, &a.FeedID, &a.GUID, &a.Title, &a.URL,
			&a.Content, &a.Summary, &a.Author, &a.PublishedDate, &a.FetchedDate); err != nil {
			return nil, fmt.Errorf("failed to scan article: %w", err)
		}
		articles = append(articles, a)
	}
	return articles, rows.Err()
}
