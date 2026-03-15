package storage

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore implements the Store interface using SQLite.
type SQLiteStore struct {
	db *tracedDB
}

// Compile-time check that SQLiteStore implements Store.
var _ Store = (*SQLiteStore)(nil)

type Feed struct {
	ID                int64
	URL               string
	Title             string
	Description       string
	LastFetched       *time.Time
	LastError         *string
	ETag              string
	LastModified      string
	Enabled           bool
	CreatedAt         time.Time
	ConsecutiveErrors int
	NextFetchAt       *time.Time
	Status            string // "active" or "dead"
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
	LinkedURL     string // outbound link extracted from a link-blog post
	LinkedContent string // readability content fetched from LinkedURL
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
	ID          int64
	UserID      int64
	Topic       string
	DisplayName string
	Muted       bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ArticleGroupWithEmbedding extends ArticleGroup with the raw centroid vector blob.
type ArticleGroupWithEmbedding struct {
	ArticleGroup
	Embedding []byte // raw little-endian float32 blob, caller decodes
}

// GroupStats holds sidebar display data for an article group virtual feed.
type GroupStats struct {
	GroupID        int64
	DisplayName    string
	UnreadArticles int
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
	Model          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ArticleAuthor represents an author extracted from a feed item.
type ArticleAuthor struct {
	Name  string
	Email string
}

// FilterRule represents a user-defined scoring rule for article filtering.
type FilterRule struct {
	ID        int64
	UserID    int64
	FeedID    *int64 // nil = global rule
	Axis      string // "author", "category", "tag"
	Value     string
	Score     int
	CreatedAt time.Time
}

// NewSQLiteStore creates a new database connection and initializes the schema.
func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	// busy_timeout and foreign_keys are connection-level PRAGMAs; embedding
	// them in the DSN via _pragma ensures every connection in the pool gets
	// them automatically, avoiding write-lock hangs and broken FK cascades.
	// 15s timeout: the daemon writes aggressively during feed fetches and
	// image caching; 5s was too short for multi-process WAL contention.
	dsn := dbPath + "?_time_format=sqlite" +
		"&_pragma=busy_timeout(15000)" +
		"&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Enable WAL mode — a persistent database-level setting, so one Exec
	// at open time is sufficient. WAL allows concurrent reads alongside
	// a single writer.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to set WAL mode: %w", err)
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
		"ALTER TABLE article_groups ADD COLUMN embedding BLOB",
		"ALTER TABLE read_state ADD COLUMN ai_scored BOOLEAN NOT NULL DEFAULT 0",
		// Backfill article_authors from the existing articles.author column.
		`INSERT OR IGNORE INTO article_authors (article_id, name)
		 SELECT id, author FROM articles WHERE author != '' AND author IS NOT NULL`,
		// OIDC identity columns on users.
		"ALTER TABLE users ADD COLUMN oidc_sub TEXT",
		"ALTER TABLE users ADD COLUMN email TEXT",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oidc_sub ON users(oidc_sub) WHERE oidc_sub IS NOT NULL",
		// Adaptive fetch scheduling.
		"ALTER TABLE feeds ADD COLUMN consecutive_errors INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE feeds ADD COLUMN next_fetch_at DATETIME",
		"ALTER TABLE feeds ADD COLUMN status TEXT NOT NULL DEFAULT 'active'",
		"CREATE INDEX IF NOT EXISTS idx_feeds_due ON feeds(next_fetch_at) WHERE status = 'active' AND enabled = 1",
		// read_state PK is (user_id, article_id); joins on article_id alone need a separate index.
		"CREATE INDEX IF NOT EXISTS idx_read_state_article_user ON read_state(article_id, user_id)",
		// Composite index for feed+date queries (replaces two separate single-column indexes for this pattern).
		"CREATE INDEX IF NOT EXISTS idx_articles_feed_published ON articles(feed_id, published_date DESC)",
		// Partial indexes for starred and unscored article lookups.
		"CREATE INDEX IF NOT EXISTS idx_read_state_user_starred ON read_state(user_id) WHERE starred = 1",
		"CREATE INDEX IF NOT EXISTS idx_read_state_user_unscored ON read_state(user_id) WHERE ai_scored = 0",
		// Drop redundant indexes superseded by PKs or better composite indexes.
		"DROP INDEX IF EXISTS idx_articles_feed_id",
		"DROP INDEX IF EXISTS idx_user_feeds_user",
		"DROP INDEX IF EXISTS idx_user_prompts_user",
		"ALTER TABLE user_prompts ADD COLUMN model TEXT",
		// Full-text fetch tracking: marks whether we've attempted to replace
		// truncated feed content with the full article text.
		"ALTER TABLE articles ADD COLUMN full_text_fetched BOOLEAN NOT NULL DEFAULT 0",
		// Image cache tracking: marks whether we've attempted to cache all
		// images referenced in this article's content.
		"ALTER TABLE articles ADD COLUMN images_cached BOOLEAN NOT NULL DEFAULT 0",
		// Link-blog post support: outbound URL extracted from short link posts
		// and the readability content fetched from that URL.
		"ALTER TABLE articles ADD COLUMN linked_url TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE articles ADD COLUMN linked_content TEXT NOT NULL DEFAULT ''",
		// Per-user custom feed display name.
		"ALTER TABLE user_feeds ADD COLUMN user_title TEXT",
		// Security check reasoning for audit/debugging.
		"ALTER TABLE read_state ADD COLUMN security_reason TEXT",
		// Article groups as virtual feeds: display name and mute support.
		"ALTER TABLE article_groups ADD COLUMN display_name TEXT",
		"ALTER TABLE article_groups ADD COLUMN muted BOOLEAN NOT NULL DEFAULT 0",
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

	return &SQLiteStore{db: &tracedDB{DB: db}}, nil
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

// scanFeeds scans a *sql.Rows result set into a []Feed slice.
// Each row must select: id, url, title, description, last_fetched, last_error,
// etag, last_modified, enabled, created_at, consecutive_errors, next_fetch_at, status.
func scanFeeds(rows *sql.Rows) ([]Feed, error) {
	var feeds []Feed
	for rows.Next() {
		var f Feed
		var etag, lastMod sql.NullString
		if err := rows.Scan(
			&f.ID, &f.URL, &f.Title, &f.Description, &f.LastFetched, &f.LastError,
			&etag, &lastMod, &f.Enabled, &f.CreatedAt,
			&f.ConsecutiveErrors, &f.NextFetchAt, &f.Status,
		); err != nil {
			return nil, fmt.Errorf("failed to scan feed: %w", err)
		}
		f.ETag = etag.String
		f.LastModified = lastMod.String
		feeds = append(feeds, f)
	}
	return feeds, rows.Err()
}

// computeFeedBaseInterval queries the last 11 article publish dates for feedID
// and returns a fetch interval based on posting recency and frequency.
func (s *SQLiteStore) computeFeedBaseInterval(feedID int64) time.Duration {
	rows, err := s.db.Query(
		`SELECT published_date FROM articles
		 WHERE feed_id = ? AND published_date IS NOT NULL
		 ORDER BY published_date DESC LIMIT 11`, feedID)
	if err != nil {
		return 24 * time.Hour
	}
	defer rows.Close()

	var dates []time.Time
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err == nil {
			dates = append(dates, t)
		}
	}
	if len(dates) == 0 {
		return 24 * time.Hour // new or empty feed
	}

	lastPostAge := time.Since(dates[0])

	var gaps []time.Duration
	for i := 0; i < len(dates)-1; i++ {
		if gap := dates[i].Sub(dates[i+1]); gap > 0 {
			gaps = append(gaps, gap)
		}
	}
	var medianGap time.Duration
	if len(gaps) > 0 {
		sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
		medianGap = gaps[len(gaps)/2]
	}

	return pickFetchInterval(lastPostAge, medianGap)
}

// pickFetchInterval maps posting recency and frequency to a base fetch interval.
func pickFetchInterval(lastPostAge, medianPostInterval time.Duration) time.Duration {
	const (
		day   = 24 * time.Hour
		week  = 7 * day
		month = 30 * day
	)
	switch {
	case lastPostAge < week:
		switch {
		case medianPostInterval < 6*time.Hour:
			return 30 * time.Minute
		case medianPostInterval < day:
			return time.Hour
		default:
			return 4 * time.Hour
		}
	case lastPostAge < month:
		return 12 * time.Hour
	case lastPostAge < 3*month:
		return day
	case lastPostAge < 6*month:
		return 3 * day
	case lastPostAge < 365*day:
		return week
	default:
		return 30 * day
	}
}

// applyErrorBackoff returns base doubled for each consecutive error, capped at 30 days.
func applyErrorBackoff(base time.Duration, consecutiveErrors int) time.Duration {
	if consecutiveErrors <= 0 {
		return base
	}
	n := consecutiveErrors
	if n > 6 {
		n = 6 // cap multiplier at 64×
	}
	backoff := base * time.Duration(1<<n)
	if max := 30 * 24 * time.Hour; backoff > max {
		return max
	}
	return backoff
}

// Close closes the database connection
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// User represents a registered household member.
type User struct {
	ID        int64
	Name      string
	OIDCSub   *string // OIDC subject claim; nil for users created before OIDC
	Email     *string // email from JWT; may be nil
	CreatedAt time.Time
}

// CreateUser registers a new user by name. Returns the new user's ID.
func (s *SQLiteStore) CreateUser(name string) (int64, error) {
	result, err := s.db.Exec(
		"INSERT INTO users (name) VALUES (?)",
		name,
	)
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return result.LastInsertId()
}

// GetUserByName looks up a user by name (case-insensitive).
func (s *SQLiteStore) GetUserByName(name string) (*User, error) {
	var u User
	err := s.db.QueryRow(
		"SELECT id, name, oidc_sub, email, created_at FROM users WHERE name = ?",
		name,
	).Scan(&u.ID, &u.Name, &u.OIDCSub, &u.Email, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByOIDCSub looks up a user by their OIDC subject claim.
func (s *SQLiteStore) GetUserByOIDCSub(sub string) (*User, error) {
	var u User
	err := s.db.QueryRow(
		"SELECT id, name, oidc_sub, email, created_at FROM users WHERE oidc_sub = ?",
		sub,
	).Scan(&u.ID, &u.Name, &u.OIDCSub, &u.Email, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateUserWithOIDC registers a new user with OIDC identity, returning the full User.
func (s *SQLiteStore) CreateUserWithOIDC(name, email, sub string) (*User, error) {
	var emailVal *string
	if email != "" {
		emailVal = &email
	}
	result, err := s.db.Exec(
		"INSERT INTO users (name, oidc_sub, email) VALUES (?, ?, ?)",
		name, sub, emailVal,
	)
	if err != nil {
		return nil, fmt.Errorf("create OIDC user: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	u := &User{ID: id, Name: name, OIDCSub: &sub, Email: emailVal}
	return u, nil
}

// UpdateUserOIDCEmail updates the stored email for a user.
func (s *SQLiteStore) UpdateUserOIDCEmail(id int64, email string) error {
	_, err := s.db.Exec("UPDATE users SET email = ? WHERE id = ?", email, id)
	return err
}

// ListUsers returns all registered users ordered by name.
func (s *SQLiteStore) ListUsers() ([]User, error) {
	rows, err := s.db.Query("SELECT id, name, oidc_sub, email, created_at FROM users ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.OIDCSub, &u.Email, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// User prompt management

// GetUserPrompt retrieves a user's custom prompt template
func (s *SQLiteStore) GetUserPrompt(userID int64, promptType string) (string, error) {
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
func (s *SQLiteStore) GetUserPromptTemperature(userID int64, promptType string) (float64, error) {
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

// GetUserPromptModel retrieves a user's custom model for a prompt type
func (s *SQLiteStore) GetUserPromptModel(userID int64, promptType string) (string, error) {
	var model sql.NullString
	err := s.db.QueryRow(
		"SELECT model FROM user_prompts WHERE user_id = ? AND prompt_type = ?",
		userID, promptType,
	).Scan(&model)
	if err != nil {
		return "", err
	}
	return model.String, nil
}

// SetUserPrompt sets a custom prompt template for a user
func (s *SQLiteStore) SetUserPrompt(userID int64, promptType, promptTemplate string, temperature *float64, model *string) error {
	_, err := s.db.Exec(
		`INSERT INTO user_prompts (user_id, prompt_type, prompt_template, temperature, model, updated_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(user_id, prompt_type) DO UPDATE SET
		   prompt_template = excluded.prompt_template,
		   temperature = excluded.temperature,
		   model = COALESCE(excluded.model, model),
		   updated_at = CURRENT_TIMESTAMP`,
		userID, promptType, promptTemplate, temperature, model,
	)
	return err
}

// DeleteUserPrompt removes a custom prompt, reverting to config/default
func (s *SQLiteStore) DeleteUserPrompt(userID int64, promptType string) error {
	_, err := s.db.Exec(
		"DELETE FROM user_prompts WHERE user_id = ? AND prompt_type = ?",
		userID, promptType,
	)
	return err
}

// ListUserPrompts lists all custom prompts for a user
func (s *SQLiteStore) ListUserPrompts(userID int64) ([]UserPrompt, error) {
	rows, err := s.db.Query(
		`SELECT prompt_type, prompt_template, temperature, model, created_at, updated_at
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
		var model sql.NullString
		err := rows.Scan(&p.PromptType, &p.PromptTemplate, &temp, &model, &p.CreatedAt, &p.UpdatedAt)
		p.Model = model.String
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
func (s *SQLiteStore) GetUserPreference(userID int64, key string) (string, error) {
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
func (s *SQLiteStore) SetUserPreference(userID int64, key, value string) error {
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
func (s *SQLiteStore) GetAllUserPreferences(userID int64) (map[string]string, error) {
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
func (s *SQLiteStore) DeleteUserPreference(userID int64, key string) error {
	_, err := s.db.Exec(
		"DELETE FROM user_preferences WHERE user_id = ? AND key = ?",
		userID, key,
	)
	return err
}

// UpdateStarred sets the starred flag on an article's read state.
func (s *SQLiteStore) UpdateStarred(userID, articleID int64, starred bool) error {
	_, err := s.db.Exec(
		`INSERT INTO read_state (user_id, article_id, starred)
		 VALUES (?, ?, ?)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
		   starred = excluded.starred`,
		userID, articleID, starred,
	)
	if err != nil {
		return fmt.Errorf("update starred: %w", err)
	}
	return nil
}

// AddFeed adds a new feed to the database
func (s *SQLiteStore) AddFeed(url, title, description string) (int64, error) {
	result, err := s.db.Exec(
		"INSERT INTO feeds (url, title, description) VALUES (?, ?, ?)",
		url, title, description,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to add feed: %w", err)
	}
	return result.LastInsertId()
}

// GetAllFeeds returns all active enabled feeds that are due for fetching.
func (s *SQLiteStore) GetAllFeeds() ([]Feed, error) {
	rows, err := s.db.Query(`
		SELECT id, url, title, description, last_fetched, last_error, etag, last_modified,
		       enabled, created_at, consecutive_errors, next_fetch_at, status
		FROM feeds
		WHERE enabled = 1 AND status = 'active'
		  AND (next_fetch_at IS NULL OR next_fetch_at <= CURRENT_TIMESTAMP)`)
	if err != nil {
		return nil, fmt.Errorf("failed to get feeds: %w", err)
	}
	defer rows.Close()
	return scanFeeds(rows)
}

// UpdateFeedError records a fetch error, increments the consecutive error count,
// schedules the next fetch with exponential backoff, and marks the feed as dead
// when it has failed 5+ times without a successful fetch in the last 30 days.
func (s *SQLiteStore) UpdateFeedError(feedID int64, errMsg string) error {
	if _, err := s.db.Exec(
		"UPDATE feeds SET last_error = ?, consecutive_errors = consecutive_errors + 1 WHERE id = ?",
		errMsg, feedID,
	); err != nil {
		return fmt.Errorf("failed to update feed error: %w", err)
	}

	var consecutiveErrors int
	var lastFetched sql.NullTime
	if err := s.db.QueryRow(
		"SELECT consecutive_errors, last_fetched FROM feeds WHERE id = ?", feedID,
	).Scan(&consecutiveErrors, &lastFetched); err != nil {
		return nil // best-effort scheduling; don't fail the caller
	}

	// Mark dead when persistently broken: 5+ errors and no success in 30+ days.
	if consecutiveErrors >= 5 && (!lastFetched.Valid || time.Since(lastFetched.Time) > 30*24*time.Hour) {
		s.db.Exec("UPDATE feeds SET status = 'dead' WHERE id = ?", feedID) //nolint:errcheck
		return nil
	}

	base := s.computeFeedBaseInterval(feedID)
	next := time.Now().Add(applyErrorBackoff(base, consecutiveErrors))
	s.db.Exec("UPDATE feeds SET next_fetch_at = ? WHERE id = ?", next, feedID) //nolint:errcheck
	return nil
}

// ClearFeedError clears the last error and schedules the next fetch.
func (s *SQLiteStore) ClearFeedError(feedID int64) error {
	return s.UpdateFeedLastFetched(feedID)
}

// MarkFeedFetched records a successful fetch and resets error state without
// scheduling next_fetch_at. Use for initial subscriptions so the feed remains
// immediately eligible for the next regular fetch cycle (next_fetch_at = NULL
// means "due now").
func (s *SQLiteStore) MarkFeedFetched(feedID int64) error {
	_, err := s.db.Exec(
		`UPDATE feeds SET last_fetched = CURRENT_TIMESTAMP, last_error = NULL,
		 consecutive_errors = 0, status = 'active' WHERE id = ?`,
		feedID,
	)
	if err != nil {
		return fmt.Errorf("failed to mark feed fetched: %w", err)
	}
	return nil
}

// UpdateFeedCacheHeaders stores the HTTP cache headers from the last successful fetch.
func (s *SQLiteStore) UpdateFeedCacheHeaders(feedID int64, etag, lastModified string) error {
	_, err := s.db.Exec("UPDATE feeds SET etag = ?, last_modified = ? WHERE id = ?", etag, lastModified, feedID)
	if err != nil {
		return fmt.Errorf("failed to update feed cache headers: %w", err)
	}
	return nil
}

// UpdateFeedLastFetched records a successful fetch, resets error state, and
// schedules the next fetch based on the feed's posting frequency.
func (s *SQLiteStore) UpdateFeedLastFetched(feedID int64) error {
	base := s.computeFeedBaseInterval(feedID)
	next := time.Now().Add(base)
	_, err := s.db.Exec(
		`UPDATE feeds SET last_fetched = CURRENT_TIMESTAMP, last_error = NULL,
		 consecutive_errors = 0, status = 'active', next_fetch_at = ? WHERE id = ?`,
		next, feedID,
	)
	if err != nil {
		return fmt.Errorf("failed to update feed last_fetched: %w", err)
	}
	return nil
}

// FindDuplicateArticle returns the ID of an existing article with the same title
// and published date (used to suppress cross-posted duplicates from multiple feeds).
// Returns 0 if no duplicate is found.
func (s *SQLiteStore) FindDuplicateArticle(title string, publishedDate *time.Time) (int64, error) {
	if title == "" || publishedDate == nil {
		return 0, nil
	}
	var id int64
	err := s.db.QueryRow(
		"SELECT id FROM articles WHERE title = ? AND published_date = ? LIMIT 1",
		title, publishedDate,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// AddArticle adds a new article to the database
func (s *SQLiteStore) AddArticle(article *Article) (int64, error) {
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
func (s *SQLiteStore) GetUnreadArticles(limit int) ([]Article, error) {
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

// GetArticlesNeedingFullText returns the most recently fetched articles that
// have not yet been processed for full-text extraction, newest first.
func (s *SQLiteStore) GetArticlesNeedingFullText(limit int) ([]Article, error) {
	const query = `
		SELECT id, feed_id, guid, title, url, COALESCE(content,''), COALESCE(summary,''),
		       COALESCE(author,''), published_date, fetched_date
		FROM articles
		WHERE full_text_fetched = 0
		ORDER BY fetched_date DESC
		LIMIT ?`
	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("get articles needing full text: %w", err)
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

// UpdateArticleContent replaces an article's content field in the database.
func (s *SQLiteStore) UpdateArticleContent(articleID int64, content string) error {
	_, err := s.db.Exec(`UPDATE articles SET content = ? WHERE id = ?`, content, articleID)
	return err
}

// MarkArticleFullTextFetched sets full_text_fetched = 1 for the article,
// recording that we have already processed it (whether or not we updated the content).
func (s *SQLiteStore) MarkArticleFullTextFetched(articleID int64) error {
	_, err := s.db.Exec(`UPDATE articles SET full_text_fetched = 1 WHERE id = ?`, articleID)
	return err
}

// UpdateReadState updates or creates the read state for an article.
// When interestScore is non-nil this is an AI pipeline call: it sets scores
// and marks the article as AI-scored without touching the user's read flag.
// When interestScore is nil this is a user read/unread action: it updates
// only the read flag and read_date without touching scores or ai_scored.
func (s *SQLiteStore) UpdateReadState(userID, articleID int64, read bool, interestScore, securityScore *float64, securityReason *string) error {
	var err error
	if interestScore != nil {
		// AI pipeline: record scores, mark ai_scored=1, do not overwrite user's read flag.
		_, err = s.db.Exec(
			`INSERT INTO read_state (user_id, article_id, read, interest_score, security_score, security_reason, ai_scored)
			 VALUES (?, ?, 0, ?, ?, ?, 1)
			 ON CONFLICT(user_id, article_id) DO UPDATE SET
			   interest_score = excluded.interest_score,
			   security_score = excluded.security_score,
			   security_reason = excluded.security_reason,
			   ai_scored = 1`,
			userID, articleID, interestScore, securityScore, securityReason,
		)
	} else {
		// User action: update only read flag, do not touch scores or ai_scored.
		_, err = s.db.Exec(
			`INSERT INTO read_state (user_id, article_id, read, read_date)
			 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			 ON CONFLICT(user_id, article_id) DO UPDATE SET
			   read = excluded.read,
			   read_date = CURRENT_TIMESTAMP`,
			userID, articleID, read,
		)
	}
	if err != nil {
		return fmt.Errorf("failed to update read state: %w", err)
	}
	return nil
}

// GetScoreStats returns AI scoring breakdown per feed for a user.
func (s *SQLiteStore) GetScoreStats(userID int64) (*ScoreStatsResult, error) {
	rows, err := s.db.Query(`
		SELECT
			f.id,
			COALESCE(uf.user_title, f.title),
			COUNT(*) FILTER (WHERE rs.ai_scored = 1),
			COUNT(*) FILTER (WHERE rs.security_score >= 7.0),
			COUNT(*) FILTER (WHERE rs.security_score >= 4.0 AND rs.security_score < 7.0),
			COUNT(*) FILTER (WHERE rs.security_score IS NOT NULL AND rs.security_score < 4.0),
			COUNT(*) FILTER (WHERE rs.security_score >= 7.0 AND rs.interest_score >= 8.0),
			COUNT(*) FILTER (WHERE rs.security_score >= 7.0 AND rs.interest_score >= 5.0 AND rs.interest_score < 8.0),
			COUNT(*) FILTER (WHERE rs.security_score >= 7.0 AND rs.interest_score IS NOT NULL AND rs.interest_score < 5.0)
		FROM feeds f
		JOIN user_feeds uf ON uf.feed_id = f.id AND uf.user_id = ?
		JOIN articles a ON a.feed_id = f.id
		LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = ?
		GROUP BY f.id, uf.user_title, f.title
		ORDER BY COALESCE(uf.user_title, f.title)`,
		userID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("get score stats: %w", err)
	}
	defer rows.Close()

	result := &ScoreStatsResult{}
	for rows.Next() {
		var fs FeedScoreStats
		if err := rows.Scan(&fs.FeedID, &fs.FeedTitle, &fs.TotalScored,
			&fs.SecPass, &fs.SecBorderline, &fs.SecFail,
			&fs.IntHigh, &fs.IntMedium, &fs.IntLow); err != nil {
			return nil, fmt.Errorf("scan score stats: %w", err)
		}
		result.Total.TotalScored += fs.TotalScored
		result.Total.SecPass += fs.SecPass
		result.Total.SecBorderline += fs.SecBorderline
		result.Total.SecFail += fs.SecFail
		result.Total.IntHigh += fs.IntHigh
		result.Total.IntMedium += fs.IntMedium
		result.Total.IntLow += fs.IntLow
		result.Feeds = append(result.Feeds, fs)
	}
	return result, rows.Err()
}

// ResetScores clears AI scores so articles are reprocessed by the pipeline.
// If securityOnly is true, only articles that failed the security check are reset.
// belowScore filters to articles with security_score < belowScore (use 10.0 to reset all).
// Returns the number of rows affected.
func (s *SQLiteStore) ResetScores(userID int64, securityOnly bool, belowScore float64) (int64, error) {
	var result sql.Result
	var err error
	if securityOnly {
		result, err = s.db.Exec(
			`UPDATE read_state SET ai_scored = 0, interest_score = NULL, security_score = NULL, security_reason = NULL
			 WHERE user_id = ? AND security_score IS NOT NULL AND security_score < ?`,
			userID, belowScore,
		)
	} else {
		result, err = s.db.Exec(
			`UPDATE read_state SET ai_scored = 0, interest_score = NULL, security_score = NULL, security_reason = NULL
			 WHERE user_id = ?`,
			userID,
		)
	}
	if err != nil {
		return 0, fmt.Errorf("reset scores: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
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
func (s *SQLiteStore) GetArticlesByInterestScore(userID int64, threshold float64, limit, offset int, filterThreshold *int) ([]Article, []float64, error) {
	filterSQL, filterArgs := filterScoreClause(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(rs.interest_score, 0) * (1.0 / (1.0 + MAX(0, julianday('now') - julianday(COALESCE(a.published_date, a.fetched_date))) * 0.1)) AS decayed_score
		FROM articles a
		JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE rs.interest_score >= ? AND rs.read = 0
		` + filterSQL + `
		ORDER BY decayed_score DESC
		LIMIT ? OFFSET ?
	`
	args := []interface{}{userID, threshold}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
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
func (s *SQLiteStore) UpdateArticleAISummary(userID, articleID int64, aiSummary string) error {
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
func (s *SQLiteStore) GetArticleSummary(userID, articleID int64) (*ArticleSummary, error) {
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
	LastPostDate         *time.Time
}

// GetFeedStats returns article counts per feed for a user.
func (s *SQLiteStore) GetFeedStats(userID int64) ([]FeedStats, error) {
	rows, err := s.db.Query(`
		SELECT f.id, COALESCE(uf.user_title, f.title),
			COUNT(a.id),
			SUM(CASE WHEN (rs.read IS NULL OR rs.read = 0)
			         AND NOT EXISTS (
			           SELECT 1 FROM article_group_members agm
			           JOIN article_groups ag ON agm.group_id = ag.id
			           WHERE agm.article_id = a.id AND ag.user_id = uf.user_id
			         ) THEN 1 ELSE 0 END),
			COUNT(a.id) - COUNT(asumm.article_id),
			MAX(a.published_date)
		FROM feeds f
		JOIN user_feeds uf ON uf.feed_id = f.id AND uf.user_id = ?
		JOIN articles a ON a.feed_id = f.id
		LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = ?
		LEFT JOIN article_summaries asumm ON asumm.article_id = a.id AND asumm.user_id = ?
		GROUP BY f.id, uf.user_title
		ORDER BY COALESCE(uf.user_title, f.title)`,
		userID, userID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("get feed stats: %w", err)
	}
	defer rows.Close()

	// Time formats the driver may use for stored DATETIME values.
	// MAX() returns a plain string so we parse manually instead of
	// scanning into *time.Time (which the driver handles for named columns
	// but not aggregates).
	timeFormats := []string{
		"2006-01-02T15:04:05.999999999Z07:00", // RFC3339Nano
		"2006-01-02T15:04:05Z07:00",           // RFC3339
		"2006-01-02T15:04:05",                 // ISO without tz
		"2006-01-02 15:04:05",                 // SQLite native
	}

	var stats []FeedStats
	for rows.Next() {
		var fs FeedStats
		var lastPost *string
		if err := rows.Scan(&fs.FeedID, &fs.FeedTitle, &fs.TotalArticles, &fs.UnreadArticles, &fs.UnsummarizedArticles, &lastPost); err != nil {
			return nil, fmt.Errorf("scan feed stats: %w", err)
		}
		if lastPost != nil {
			for _, layout := range timeFormats {
				if t, err := time.Parse(layout, *lastPost); err == nil {
					fs.LastPostDate = &t
					break
				}
			}
		}
		stats = append(stats, fs)
	}
	return stats, rows.Err()
}

// CreateArticleGroup creates a new article group
func (s *SQLiteStore) CreateArticleGroup(userID int64, topic string) (int64, error) {
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
func (s *SQLiteStore) AddArticleToGroup(groupID, articleID int64) error {
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
func (s *SQLiteStore) GetGroupArticles(groupID int64) ([]Article, error) {
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
func (s *SQLiteStore) UpdateGroupSummary(groupID int64, summary string, articleCount int, maxInterestScore *float64) error {
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
func (s *SQLiteStore) GetGroupSummary(groupID int64) (*GroupSummary, error) {
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

// GetUserGroups returns groups for a user that contain at least 2 articles.
// Single-article groups are excluded as they represent ungrouped articles rather
// than genuine topic clusters.
func (s *SQLiteStore) GetUserGroups(userID int64) ([]ArticleGroup, error) {
	query := `SELECT ag.id, ag.user_id, ag.topic, ag.display_name, ag.muted, ag.created_at, ag.updated_at
		FROM article_groups ag
		WHERE ag.user_id = ?
		  AND (SELECT COUNT(*) FROM article_group_members WHERE group_id = ag.id) >= 2
		ORDER BY ag.updated_at DESC`
	rows, err := s.db.Query(query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user groups: %w", err)
	}
	defer rows.Close()

	var groups []ArticleGroup
	for rows.Next() {
		var g ArticleGroup
		var displayName *string
		if err := rows.Scan(&g.ID, &g.UserID, &g.Topic, &displayName, &g.Muted, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan group: %w", err)
		}
		if displayName != nil {
			g.DisplayName = *displayName
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// GetGroup returns a single article group by ID, regardless of user or member count.
func (s *SQLiteStore) GetGroup(groupID int64) (*ArticleGroup, error) {
	var g ArticleGroup
	var displayName *string
	err := s.db.QueryRow(
		"SELECT id, user_id, topic, display_name, muted, created_at, updated_at FROM article_groups WHERE id = ?",
		groupID,
	).Scan(&g.ID, &g.UserID, &g.Topic, &displayName, &g.Muted, &g.CreatedAt, &g.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get group: %w", err)
	}
	if displayName != nil {
		g.DisplayName = *displayName
	}
	return &g, nil
}

// FindArticleGroup finds the group ID for an article, if it belongs to one
func (s *SQLiteStore) FindArticleGroup(articleID, userID int64) (*int64, error) {
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

// GetUnreadGroupArticles returns unread articles belonging to a specific group.
func (s *SQLiteStore) GetUnreadGroupArticles(userID, groupID int64, limit, offset int, filterThreshold *int) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClause(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN article_group_members agm ON a.id = agm.article_id
		JOIN article_groups ag ON agm.group_id = ag.id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE agm.group_id = ? AND ag.user_id = ? AND (rs.article_id IS NULL OR rs.read = 0)
		` + filterSQL + `
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?
	`
	args := []interface{}{userID, groupID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get unread group articles: %w", err)
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

// GetGroupStats returns sidebar data for non-muted groups with 2+ articles and unread content.
func (s *SQLiteStore) GetGroupStats(userID int64) ([]GroupStats, error) {
	rows, err := s.db.Query(`
		SELECT ag.id,
		       COALESCE(ag.display_name, ag.topic),
		       SUM(CASE WHEN rs.read IS NULL OR rs.read = 0 THEN 1 ELSE 0 END)
		FROM article_groups ag
		JOIN article_group_members agm ON agm.group_id = ag.id
		LEFT JOIN read_state rs ON rs.article_id = agm.article_id AND rs.user_id = ?
		WHERE ag.user_id = ? AND ag.muted = 0
		GROUP BY ag.id
		HAVING COUNT(agm.article_id) >= 2
		   AND SUM(CASE WHEN rs.read IS NULL OR rs.read = 0 THEN 1 ELSE 0 END) > 0
		ORDER BY COALESCE(ag.display_name, ag.topic)`,
		userID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("get group stats: %w", err)
	}
	defer rows.Close()

	var stats []GroupStats
	for rows.Next() {
		var gs GroupStats
		if err := rows.Scan(&gs.GroupID, &gs.DisplayName, &gs.UnreadArticles); err != nil {
			return nil, fmt.Errorf("scan group stats: %w", err)
		}
		stats = append(stats, gs)
	}
	return stats, rows.Err()
}

// SetGroupMuted sets the muted flag on an article group.
func (s *SQLiteStore) SetGroupMuted(groupID int64, muted bool) error {
	_, err := s.db.Exec("UPDATE article_groups SET muted = ? WHERE id = ?", muted, groupID)
	if err != nil {
		return fmt.Errorf("set group muted: %w", err)
	}
	return nil
}

// IsGroupMuted returns whether a group is muted.
func (s *SQLiteStore) IsGroupMuted(groupID int64) (bool, error) {
	var muted bool
	err := s.db.QueryRow("SELECT muted FROM article_groups WHERE id = ?", groupID).Scan(&muted)
	if err != nil {
		return false, fmt.Errorf("is group muted: %w", err)
	}
	return muted, nil
}

// DisbandGroup deletes a group and its memberships (ON DELETE CASCADE).
func (s *SQLiteStore) DisbandGroup(groupID int64) error {
	_, err := s.db.Exec("DELETE FROM article_groups WHERE id = ?", groupID)
	if err != nil {
		return fmt.Errorf("disband group: %w", err)
	}
	return nil
}

// UpdateGroupDisplayName sets the display name for a group.
func (s *SQLiteStore) UpdateGroupDisplayName(groupID int64, displayName string) error {
	_, err := s.db.Exec("UPDATE article_groups SET display_name = ? WHERE id = ?", displayName, groupID)
	if err != nil {
		return fmt.Errorf("update group display name: %w", err)
	}
	return nil
}

// SubscribeUserToFeed subscribes a user to a feed
func (s *SQLiteStore) SubscribeUserToFeed(userID, feedID int64) error {
	_, err := s.db.Exec(
		"INSERT OR IGNORE INTO user_feeds (user_id, feed_id) VALUES (?, ?)",
		userID, feedID,
	)
	if err != nil {
		return fmt.Errorf("failed to subscribe user to feed: %w", err)
	}
	return nil
}

// GetUserFeeds returns all feeds a user is subscribed to.
func (s *SQLiteStore) GetUserFeeds(userID int64) ([]Feed, error) {
	rows, err := s.db.Query(`
		SELECT f.id, f.url, COALESCE(uf.user_title, f.title), f.description, f.last_fetched, f.last_error, f.etag,
		       f.last_modified, f.enabled, f.created_at,
		       f.consecutive_errors, f.next_fetch_at, f.status
		FROM feeds f
		JOIN user_feeds uf ON f.id = uf.feed_id
		WHERE uf.user_id = ? AND f.enabled = 1
		ORDER BY COALESCE(uf.user_title, f.title)`, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user feeds: %w", err)
	}
	defer rows.Close()
	return scanFeeds(rows)
}

// GetAllSubscribedFeeds returns all active enabled feeds that any user is subscribed
// to and that are due for fetching.
func (s *SQLiteStore) GetAllSubscribedFeeds() ([]Feed, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT f.id, f.url, f.title, f.description, f.last_fetched, f.last_error,
		       f.etag, f.last_modified, f.enabled, f.created_at,
		       f.consecutive_errors, f.next_fetch_at, f.status
		FROM feeds f
		JOIN user_feeds uf ON f.id = uf.feed_id
		WHERE f.enabled = 1 AND f.status = 'active'
		  AND (f.next_fetch_at IS NULL OR f.next_fetch_at <= CURRENT_TIMESTAMP)
		ORDER BY f.title`)
	if err != nil {
		return nil, fmt.Errorf("failed to get subscribed feeds: %w", err)
	}
	defer rows.Close()
	return scanFeeds(rows)
}

// GetAllActiveSubscribedFeeds returns all enabled feeds that any user is subscribed to,
// without any scheduling filter. Intended for export operations.
func (s *SQLiteStore) GetAllActiveSubscribedFeeds() ([]Feed, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT f.id, f.url, f.title, f.description, f.last_fetched, f.last_error,
		       f.etag, f.last_modified, f.enabled, f.created_at,
		       f.consecutive_errors, f.next_fetch_at, f.status
		FROM feeds f
		JOIN user_feeds uf ON f.id = uf.feed_id
		WHERE f.enabled = 1
		ORDER BY f.title`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all active subscribed feeds: %w", err)
	}
	defer rows.Close()
	return scanFeeds(rows)
}

// GetFeedSubscribers returns all user IDs subscribed to a feed
func (s *SQLiteStore) GetFeedSubscribers(feedID int64) ([]int64, error) {
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
func (s *SQLiteStore) UnsubscribeUserFromFeed(userID, feedID int64) error {
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
// Returns true if the feed was deleted.
//
// Articles are removed in batches before the feed itself is deleted. Each batch
// is its own transaction so the WAL write lock is released briefly between
// iterations, keeping the site responsive when unsubscribing from large feeds.
// FK CASCADE handles read_state, summaries, authors, and group-member cleanup
// for each batch of articles.
func (s *SQLiteStore) DeleteFeedIfOrphaned(feedID int64) (bool, error) {
	// Fast path: skip deletes if the feed still has subscribers.
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM user_feeds WHERE feed_id = ?", feedID).Scan(&n); err != nil {
		return false, fmt.Errorf("failed to check subscribers: %w", err)
	}
	if n > 0 {
		return false, nil
	}

	// Delete articles in batches. Each batch commits independently so long
	// feed removals don't starve concurrent readers/writers.
	const batchSize = 500
	for {
		res, err := s.db.Exec(
			`DELETE FROM articles WHERE id IN (SELECT id FROM articles WHERE feed_id = ? LIMIT ?)`,
			feedID, batchSize,
		)
		if err != nil {
			return false, fmt.Errorf("failed to batch-delete articles for feed %d: %w", feedID, err)
		}
		if deleted, _ := res.RowsAffected(); deleted == 0 {
			break
		}
	}

	// Delete the feed. The NOT EXISTS guard prevents deletion if another user
	// re-subscribed between the subscriber check above and this point.
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
func (s *SQLiteStore) RenameFeed(feedID int64, title string) error {
	_, err := s.db.Exec("UPDATE feeds SET title = ? WHERE id = ?", title, feedID)
	if err != nil {
		return fmt.Errorf("failed to rename feed: %w", err)
	}
	return nil
}

// RenameUserFeed sets a per-user display title for a feed subscription.
// Passing an empty title clears the override, reverting to the feed's original title.
func (s *SQLiteStore) RenameUserFeed(userID, feedID int64, title string) error {
	var err error
	if title == "" {
		_, err = s.db.Exec("UPDATE user_feeds SET user_title = NULL WHERE user_id = ? AND feed_id = ?", userID, feedID)
	} else {
		_, err = s.db.Exec("UPDATE user_feeds SET user_title = ? WHERE user_id = ? AND feed_id = ?", title, userID, feedID)
	}
	if err != nil {
		return fmt.Errorf("failed to rename user feed: %w", err)
	}
	return nil
}

// GetAllSubscribingUsers returns all user IDs that have feed subscriptions
func (s *SQLiteStore) GetAllSubscribingUsers() ([]int64, error) {
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
func (s *SQLiteStore) GetArticle(articleID int64) (*Article, error) {
	var a Article
	err := s.db.QueryRow(
		`SELECT id, feed_id, guid, title, url, content, summary,
		        author, published_date, fetched_date,
		        COALESCE(linked_url,''), COALESCE(linked_content,'')
		 FROM articles WHERE id = ?`, articleID,
	).Scan(&a.ID, &a.FeedID, &a.GUID, &a.Title, &a.URL,
		&a.Content, &a.Summary, &a.Author, &a.PublishedDate, &a.FetchedDate,
		&a.LinkedURL, &a.LinkedContent)
	if err != nil {
		return nil, fmt.Errorf("get article %d: %w", articleID, err)
	}
	return &a, nil
}

// UpdateArticleLinkedContent stores the outbound link URL and the readability
// content fetched from it for a link-blog post. The original post content is
// left unchanged; this data is displayed alongside it in the reading pane.
func (s *SQLiteStore) UpdateArticleLinkedContent(articleID int64, linkedURL, linkedContent string) error {
	_, err := s.db.Exec(
		`UPDATE articles SET linked_url = ?, linked_content = ? WHERE id = ?`,
		linkedURL, linkedContent, articleID,
	)
	return err
}

// GetUnscoredArticleCount returns the number of articles from the user's
// subscribed feeds that have no read_state entry (pending security/interest scoring).
func (s *SQLiteStore) GetUnscoredArticleCount(userID int64) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*)
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND (rs.article_id IS NULL OR rs.ai_scored = 0)`,
		userID, userID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get unscored article count: %w", err)
	}
	return count, nil
}

// GetUnsummarizedArticleCount returns the number of articles from the user's
// subscribed feeds that have no AI summary yet (pending content summarization).
func (s *SQLiteStore) GetUnsummarizedArticleCount(userID int64) (int, error) {
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
func (s *SQLiteStore) GetUnscoredArticlesForUser(userID int64, limit int) ([]Article, error) {
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND (rs.article_id IS NULL OR rs.ai_scored = 0)
		ORDER BY a.published_date DESC
		LIMIT ?
	`
	rows, err := s.db.Query(query, userID, userID, limit)
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
func (s *SQLiteStore) GetUnreadArticlesForUser(userID int64, limit, offset int, filterThreshold *int) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClause(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND (rs.article_id IS NULL OR rs.read = 0)
		AND NOT EXISTS (
			SELECT 1 FROM article_group_members agm
			JOIN article_groups ag ON agm.group_id = ag.id
			WHERE agm.article_id = a.id AND ag.user_id = ?
		)
		` + filterSQL + `
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?
	`
	args := []interface{}{userID, userID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
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

// GetUnreadArticlesByFeed returns unread articles for a user filtered to a specific feed.
func (s *SQLiteStore) GetUnreadArticlesByFeed(userID, feedID int64, limit, offset int, filterThreshold *int) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClause(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND a.feed_id = ? AND (rs.article_id IS NULL OR rs.read = 0)
		AND NOT EXISTS (
			SELECT 1 FROM article_group_members agm
			JOIN article_groups ag ON agm.group_id = ag.id
			WHERE agm.article_id = a.id AND ag.user_id = ?
		)
		` + filterSQL + `
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?
	`
	args := []interface{}{userID, userID, feedID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get unread articles by feed: %w", err)
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

// UpdateGroupEmbedding stores or updates the centroid embedding for a group.
func (s *SQLiteStore) UpdateGroupEmbedding(groupID int64, embedding []byte) error {
	_, err := s.db.Exec("UPDATE article_groups SET embedding = ? WHERE id = ?", embedding, groupID)
	if err != nil {
		return fmt.Errorf("update group embedding: %w", err)
	}
	return nil
}

// GetGroupsWithEmbeddings returns all groups for a user that have a centroid embedding.
func (s *SQLiteStore) GetGroupsWithEmbeddings(userID int64) ([]ArticleGroupWithEmbedding, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, topic, display_name, muted, embedding, created_at, updated_at
		 FROM article_groups
		 WHERE user_id = ? AND embedding IS NOT NULL`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("get groups with embeddings: %w", err)
	}
	defer rows.Close()

	var groups []ArticleGroupWithEmbedding
	for rows.Next() {
		var g ArticleGroupWithEmbedding
		var displayName *string
		if err := rows.Scan(&g.ID, &g.UserID, &g.Topic, &displayName, &g.Muted, &g.Embedding, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan group with embedding: %w", err)
		}
		if displayName != nil {
			g.DisplayName = *displayName
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// GetGroupEmbedding returns the raw centroid embedding for a single group.
// Returns nil if the group has no embedding.
func (s *SQLiteStore) GetGroupEmbedding(groupID int64) ([]byte, error) {
	var emb []byte
	err := s.db.QueryRow(
		"SELECT embedding FROM article_groups WHERE id = ?",
		groupID,
	).Scan(&emb)
	if err != nil {
		return nil, fmt.Errorf("get group embedding: %w", err)
	}
	return emb, nil
}

// GetGroupArticleCount returns the number of articles in a group.
func (s *SQLiteStore) GetGroupArticleCount(groupID int64) (int, error) {
	var count int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM article_group_members WHERE group_id = ?",
		groupID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get group article count: %w", err)
	}
	return count, nil
}

// UpdateGroupTopic updates the topic label for a group.
func (s *SQLiteStore) UpdateGroupTopic(groupID int64, topic string) error {
	_, err := s.db.Exec("UPDATE article_groups SET topic = ? WHERE id = ?", topic, groupID)
	if err != nil {
		return fmt.Errorf("update group topic: %w", err)
	}
	return nil
}

// GetStarredArticles returns starred articles for a user.
func (s *SQLiteStore) GetStarredArticles(userID int64, limit, offset int, filterThreshold *int) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClause(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND rs.starred = 1
		` + filterSQL + `
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?
	`
	args := []interface{}{userID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get starred articles: %w", err)
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

// --- Filter scoring helper ---

// filterScoreClause returns an SQL fragment and bind args that filter articles
// by additive filter rule scoring. Returns ("", nil) when threshold is nil
// (no filtering). The caller's query must alias the articles table as "a".
func filterScoreClause(userID int64, threshold *int) (string, []interface{}) {
	if threshold == nil {
		return "", nil
	}
	sql := `AND (
		NOT EXISTS (SELECT 1 FROM filter_rules WHERE user_id = ?)
		OR (
			SELECT COALESCE(SUM(fr.score), 0)
			FROM filter_rules fr
			WHERE fr.user_id = ?
			  AND (fr.feed_id IS NULL OR fr.feed_id = a.feed_id)
			  AND (
				(fr.axis = 'author' AND EXISTS (
				  SELECT 1 FROM article_authors aa
				  WHERE aa.article_id = a.id AND aa.name = fr.value
				))
				OR (fr.axis IN ('category', 'tag') AND EXISTS (
				  SELECT 1 FROM article_categories ac
				  WHERE ac.article_id = a.id AND ac.category = fr.value
				))
			  )
		) >= ?
	)`
	return sql, []interface{}{userID, userID, *threshold}
}

// --- Article metadata methods ---

// StoreArticleAuthors stores authors for an article. Uses INSERT OR IGNORE
// to handle duplicates gracefully.
func (s *SQLiteStore) StoreArticleAuthors(articleID int64, authors []ArticleAuthor) error {
	for _, a := range authors {
		_, err := s.db.Exec(
			"INSERT OR IGNORE INTO article_authors (article_id, name, email) VALUES (?, ?, ?)",
			articleID, a.Name, a.Email,
		)
		if err != nil {
			return fmt.Errorf("store article author: %w", err)
		}
	}
	return nil
}

// StoreArticleCategories stores categories for an article. Uses INSERT OR IGNORE
// to handle duplicates gracefully.
func (s *SQLiteStore) StoreArticleCategories(articleID int64, categories []string) error {
	for _, cat := range categories {
		_, err := s.db.Exec(
			"INSERT OR IGNORE INTO article_categories (article_id, category) VALUES (?, ?)",
			articleID, cat,
		)
		if err != nil {
			return fmt.Errorf("store article category: %w", err)
		}
	}
	return nil
}

// GetArticleAuthors returns all authors for an article.
func (s *SQLiteStore) GetArticleAuthors(articleID int64) ([]ArticleAuthor, error) {
	rows, err := s.db.Query(
		"SELECT name, email FROM article_authors WHERE article_id = ? ORDER BY name",
		articleID,
	)
	if err != nil {
		return nil, fmt.Errorf("get article authors: %w", err)
	}
	defer rows.Close()

	var authors []ArticleAuthor
	for rows.Next() {
		var a ArticleAuthor
		var email sql.NullString
		if err := rows.Scan(&a.Name, &email); err != nil {
			return nil, fmt.Errorf("scan article author: %w", err)
		}
		a.Email = email.String
		authors = append(authors, a)
	}
	return authors, rows.Err()
}

// GetArticleCategories returns all categories for an article.
func (s *SQLiteStore) GetArticleCategories(articleID int64) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT category FROM article_categories WHERE article_id = ? ORDER BY category",
		articleID,
	)
	if err != nil {
		return nil, fmt.Errorf("get article categories: %w", err)
	}
	defer rows.Close()

	var cats []string
	for rows.Next() {
		var cat string
		if err := rows.Scan(&cat); err != nil {
			return nil, fmt.Errorf("scan article category: %w", err)
		}
		cats = append(cats, cat)
	}
	return cats, rows.Err()
}

// --- Feed metadata discovery ---

// GetFeedAuthors returns distinct author names across all articles in a feed.
func (s *SQLiteStore) GetFeedAuthors(feedID int64) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT aa.name FROM article_authors aa
		 JOIN articles a ON a.id = aa.article_id
		 WHERE a.feed_id = ? ORDER BY aa.name`,
		feedID,
	)
	if err != nil {
		return nil, fmt.Errorf("get feed authors: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan feed author: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// GetFeedCategories returns distinct categories across all articles in a feed.
func (s *SQLiteStore) GetFeedCategories(feedID int64) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT ac.category FROM article_categories ac
		 JOIN articles a ON a.id = ac.article_id
		 WHERE a.feed_id = ? ORDER BY ac.category`,
		feedID,
	)
	if err != nil {
		return nil, fmt.Errorf("get feed categories: %w", err)
	}
	defer rows.Close()

	var cats []string
	for rows.Next() {
		var cat string
		if err := rows.Scan(&cat); err != nil {
			return nil, fmt.Errorf("scan feed category: %w", err)
		}
		cats = append(cats, cat)
	}
	return cats, rows.Err()
}

// --- Filter rules CRUD ---

// AddFilterRule inserts a new filter rule and returns its ID.
func (s *SQLiteStore) AddFilterRule(rule *FilterRule) (int64, error) {
	result, err := s.db.Exec(
		`INSERT INTO filter_rules (user_id, feed_id, axis, value, score)
		 VALUES (?, ?, ?, ?, ?)`,
		rule.UserID, rule.FeedID, rule.Axis, rule.Value, rule.Score,
	)
	if err != nil {
		return 0, fmt.Errorf("add filter rule: %w", err)
	}
	return result.LastInsertId()
}

// GetFilterRules returns filter rules for a user. If feedID is non-nil,
// returns only rules scoped to that feed plus global rules. If nil, returns all.
func (s *SQLiteStore) GetFilterRules(userID int64, feedID *int64) ([]FilterRule, error) {
	var query string
	var args []interface{}

	if feedID != nil {
		query = `SELECT id, user_id, feed_id, axis, value, score, created_at
				 FROM filter_rules WHERE user_id = ? AND (feed_id IS NULL OR feed_id = ?)
				 ORDER BY axis, value`
		args = []interface{}{userID, *feedID}
	} else {
		query = `SELECT id, user_id, feed_id, axis, value, score, created_at
				 FROM filter_rules WHERE user_id = ?
				 ORDER BY axis, value`
		args = []interface{}{userID}
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get filter rules: %w", err)
	}
	defer rows.Close()

	var rules []FilterRule
	for rows.Next() {
		var r FilterRule
		if err := rows.Scan(&r.ID, &r.UserID, &r.FeedID, &r.Axis, &r.Value, &r.Score, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan filter rule: %w", err)
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// UpdateFilterRuleScore updates the score of an existing filter rule.
func (s *SQLiteStore) UpdateFilterRuleScore(ruleID int64, score int) error {
	_, err := s.db.Exec("UPDATE filter_rules SET score = ? WHERE id = ?", score, ruleID)
	if err != nil {
		return fmt.Errorf("update filter rule score: %w", err)
	}
	return nil
}

// DeleteFilterRule deletes a filter rule by ID.
func (s *SQLiteStore) DeleteFilterRule(ruleID int64) error {
	_, err := s.db.Exec("DELETE FROM filter_rules WHERE id = ?", ruleID)
	if err != nil {
		return fmt.Errorf("delete filter rule: %w", err)
	}
	return nil
}

// HasFilterRules returns true if the user has any filter rules defined.
func (s *SQLiteStore) HasFilterRules(userID int64) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM filter_rules WHERE user_id = ?", userID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("has filter rules: %w", err)
	}
	return count > 0, nil
}

// --- Feed favicons ---

// FeedFavicon holds a cached favicon for a feed.
type FeedFavicon struct {
	FeedID    int64
	Data      []byte
	MimeType  string
	FetchedAt time.Time
}

// StoreFeedFavicon upserts a favicon for the given feed.
func (s *SQLiteStore) StoreFeedFavicon(feedID int64, data []byte, mimeType string) error {
	_, err := s.db.Exec(
		`INSERT INTO feed_favicons (feed_id, data, mime_type, fetched_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(feed_id) DO UPDATE SET data = excluded.data,
		   mime_type = excluded.mime_type, fetched_at = CURRENT_TIMESTAMP`,
		feedID, data, mimeType,
	)
	return err
}

// GetFeedFavicon returns the cached favicon for a feed, or nil if none exists.
func (s *SQLiteStore) GetFeedFavicon(feedID int64) (*FeedFavicon, error) {
	var f FeedFavicon
	err := s.db.QueryRow(
		`SELECT feed_id, data, mime_type, fetched_at FROM feed_favicons WHERE feed_id = ?`, feedID,
	).Scan(&f.FeedID, &f.Data, &f.MimeType, &f.FetchedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get feed favicon: %w", err)
	}
	return &f, nil
}

// GetAllFeedFavicons returns all cached favicons (used by Fever API &favicons).
func (s *SQLiteStore) GetAllFeedFavicons() ([]FeedFavicon, error) {
	rows, err := s.db.Query(`SELECT feed_id, data, mime_type, fetched_at FROM feed_favicons`)
	if err != nil {
		return nil, fmt.Errorf("get all feed favicons: %w", err)
	}
	defer rows.Close()

	var favicons []FeedFavicon
	for rows.Next() {
		var f FeedFavicon
		if err := rows.Scan(&f.FeedID, &f.Data, &f.MimeType, &f.FetchedAt); err != nil {
			return nil, fmt.Errorf("scan feed favicon: %w", err)
		}
		favicons = append(favicons, f)
	}
	return favicons, rows.Err()
}

// GetSubscribedFeedsWithoutFavicons returns subscribed feeds that have no
// cached favicon, ordered by ID. Used to drive background favicon fetching.
func (s *SQLiteStore) GetSubscribedFeedsWithoutFavicons() ([]Feed, error) {
	const query = `
		SELECT DISTINCT f.id, f.url, f.title, f.description,
		       f.last_fetched, f.last_error, f.etag, f.last_modified,
		       f.enabled, f.created_at, f.consecutive_errors, f.next_fetch_at, f.status
		FROM feeds f
		JOIN user_feeds uf ON f.id = uf.feed_id
		LEFT JOIN feed_favicons ff ON f.id = ff.feed_id
		WHERE ff.feed_id IS NULL
		ORDER BY f.id`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("get feeds without favicons: %w", err)
	}
	defer rows.Close()

	return scanFeeds(rows)
}

// --- Article images ---

// ArticleImage holds a cached image extracted from article content.
type ArticleImage struct {
	ID          int64
	ArticleID   int64
	OriginalURL string
	Data        []byte
	MimeType    string
	Width       int
	Height      int
	FetchedAt   time.Time
}

// StoreArticleImage upserts a cached image for an article.
// Returns the row ID of the inserted or existing image.
func (s *SQLiteStore) StoreArticleImage(articleID int64, originalURL string, data []byte, mimeType string, width, height int) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO article_images (article_id, original_url, data, mime_type, width, height)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(article_id, original_url) DO UPDATE SET
		   data = excluded.data, mime_type = excluded.mime_type,
		   width = excluded.width, height = excluded.height,
		   fetched_at = CURRENT_TIMESTAMP`,
		articleID, originalURL, data, mimeType, width, height,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// LastInsertId returns 0 on a no-op upsert in SQLite; look up the real ID.
	if id == 0 {
		err = s.db.QueryRow(
			`SELECT id FROM article_images WHERE article_id = ? AND original_url = ?`,
			articleID, originalURL,
		).Scan(&id)
	}
	return id, err
}

// GetArticleImage returns a single cached image by its ID.
func (s *SQLiteStore) GetArticleImage(imageID int64) (*ArticleImage, error) {
	var img ArticleImage
	err := s.db.QueryRow(
		`SELECT id, article_id, original_url, data, mime_type, width, height, fetched_at
		 FROM article_images WHERE id = ?`, imageID,
	).Scan(&img.ID, &img.ArticleID, &img.OriginalURL, &img.Data,
		&img.MimeType, &img.Width, &img.Height, &img.FetchedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get article image: %w", err)
	}
	return &img, nil
}

// GetArticleImageMap returns a map of original URL → image ID for all cached
// images belonging to an article. Used to rewrite HTML at serve time.
func (s *SQLiteStore) GetArticleImageMap(articleID int64) (map[string]int64, error) {
	rows, err := s.db.Query(
		`SELECT id, original_url FROM article_images WHERE article_id = ?`, articleID,
	)
	if err != nil {
		return nil, fmt.Errorf("get article image map: %w", err)
	}
	defer rows.Close()

	m := make(map[string]int64)
	for rows.Next() {
		var id int64
		var u string
		if err := rows.Scan(&id, &u); err != nil {
			return nil, fmt.Errorf("scan article image: %w", err)
		}
		m[u] = id
	}
	return m, rows.Err()
}

// GetArticlesNeedingImageCache returns the most recently fetched articles
// whose images have not yet been cached (images_cached = 0), newest first.
func (s *SQLiteStore) GetArticlesNeedingImageCache(limit int) ([]Article, error) {
	const query = `
		SELECT id, feed_id, guid, title, url, COALESCE(content,''), COALESCE(summary,''),
		       COALESCE(author,''), published_date, fetched_date
		FROM articles
		WHERE images_cached = 0
		ORDER BY fetched_date DESC
		LIMIT ?`
	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("get articles needing image cache: %w", err)
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

// MarkArticleImagesCached sets images_cached = 1 for the article.
func (s *SQLiteStore) MarkArticleImagesCached(articleID int64) error {
	_, err := s.db.Exec(`UPDATE articles SET images_cached = 1 WHERE id = ?`, articleID)
	return err
}

// NewStore returns a Store backed by the appropriate database driver.
// A DSN beginning with "postgres://" or "postgresql://" selects PostgreSQL;
// all other values are treated as a SQLite file path.
func NewStore(dsn string) (Store, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return NewPostgresStore(dsn)
	}
	return NewSQLiteStore(dsn)
}
