package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
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
	SiteURL           string // blog homepage URL, populated from feed metadata
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
	ID              int64
	FeedID          int64
	GUID            string
	Title           string
	URL             string
	Content         string
	Summary         string
	Author          string
	PublishedDate   *time.Time
	FetchedDate     time.Time
	LinkedURL       string // outbound link extracted from a link-blog post
	LinkedContent   string // readability content fetched from LinkedURL
	SecurityFlagged bool   // true when article passed the medium security threshold but not the full threshold
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

// ArticleEmbeddingRow holds a single article's embedding for cosine similarity search.
type ArticleEmbeddingRow struct {
	ArticleID int64
	Embedding []byte // raw little-endian float32 blob, caller decodes
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
	Headline         string
	Summary          string
	ArticleCount     int
	MaxInterestScore *float64
	GeneratedAt      time.Time
}

// NewsletterConfig holds the filtering criteria for a newsletter definition.
type NewsletterConfig struct {
	MinInterestScore  float64  `json:"min_interest_score"`
	MinSecurityScore  float64  `json:"min_security_score"`
	IncludeFeeds      []int64  `json:"include_feeds,omitempty"`
	ExcludeFeeds      []int64  `json:"exclude_feeds,omitempty"`
	IncludeCategories []string `json:"include_categories,omitempty"`
	ExcludeCategories []string `json:"exclude_categories,omitempty"`
	// IncludeTags names feed tags this digest follows: at generation time they
	// resolve to whatever feeds currently carry the tag, unioned with IncludeFeeds.
	IncludeTags []string `json:"include_tags,omitempty"`
	MaxArticles int      `json:"max_articles"`
}

// Newsletter represents a user-defined newsletter/digest configuration.
type Newsletter struct {
	ID              int64
	UserID          int64
	Name            string
	Schedule        string // "manual", "hourly", "daily"
	Config          NewsletterConfig
	PromptTemplate  string
	EmailRecipient  string
	Enabled         bool
	LastGeneratedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NewsletterIssue represents a single generated newsletter edition.
type NewsletterIssue struct {
	ID           int64
	NewsletterID int64
	Headline     string
	ContentHTML  string
	ContentText  string
	ArticleIDs   []int64
	GeneratedAt  time.Time
	SentAt       *time.Time
}

// NewsletterStats holds sidebar display data for a newsletter.
type NewsletterStats struct {
	NewsletterID int64
	Name         string
	IssueCount   int
}

// AISummary is a single on-demand digest of a user's high-interest unread
// backlog, generated in one long-context cloud-model call. Status moves
// generating → done | failed. ArticleIDs records exactly which articles the
// digest covered, so "mark all as read" can mark precisely those.
type AISummary struct {
	ID           int64
	UserID       int64
	NewsletterID *int64 // config that produced it; nil = ad-hoc
	Status       string // "generating", "done", "failed"
	Model        string
	Prompt       string
	Headline     string
	ContentHTML  string
	ArticleIDs   []int64
	ArticleCount int
	InputTokens  int
	OutputTokens int
	Error        string
	CreatedAt    time.Time
	GeneratedAt  *time.Time
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
		// Blog homepage URL, populated from feed metadata on each fetch.
		"ALTER TABLE feeds ADD COLUMN site_url TEXT NOT NULL DEFAULT ''",
		// Group summary headline for display above narrative.
		"ALTER TABLE group_summaries ADD COLUMN headline TEXT NOT NULL DEFAULT ''",
		// Track which embedding model produced each group centroid.
		"ALTER TABLE article_groups ADD COLUMN embedding_model TEXT NOT NULL DEFAULT ''",
		// Retry counter for AI pipeline failures (prevents infinite retry loops).
		"ALTER TABLE read_state ADD COLUMN ai_retries INTEGER NOT NULL DEFAULT 0",
		// Sentinel marker for summarization rejections that shouldn't be retried
		// (summary longer than content, summary exceeds maxLen+15%). Non-null
		// reason + empty ai_summary = "we tried, it didn't fit, don't retry."
		"ALTER TABLE article_summaries ADD COLUMN skip_reason TEXT",
		// Embedding lifecycle state. Replaces the legacy single-byte
		// sentinel encoding that conflated transient errors with permanent
		// skips. See EmbedStatus* constants.
		"ALTER TABLE article_embeddings ADD COLUMN status SMALLINT NOT NULL DEFAULT 0",
		"ALTER TABLE article_embeddings ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE article_embeddings ADD COLUMN error_message TEXT",
		// Time-based retry eligibility — see EmbedRetryCooldown. Existing
		// rows have NULL here, which the GetArticlesWithoutEmbeddings query
		// treats as "no recorded attempt, eligible immediately."
		"ALTER TABLE article_embeddings ADD COLUMN last_attempted_at DATETIME",
		// Reclassify legacy 1-byte sentinels:
		//   articles whose body is genuinely too short → status=1 (no retry)
		//   everything else with a 1-byte sentinel    → status=2 (retryable)
		// Idempotent: each UPDATE narrows by status=0 so re-running is a no-op.
		`UPDATE article_embeddings
		 SET status = 1
		 WHERE octet_length(embedding) = 1 AND status = 0
		   AND article_id IN (
		     SELECT id FROM articles
		     WHERE octet_length(COALESCE(NULLIF(content, ''), summary, '')) < 200
		   )`,
		`UPDATE article_embeddings
		 SET status = 2,
		     error_message = 'migrated from legacy 1-byte sentinel — original error not preserved'
		 WHERE octet_length(embedding) = 1 AND status = 0`,
		// FTS5 full-text search index (external-content, synced via triggers).
		`CREATE VIRTUAL TABLE IF NOT EXISTS articles_fts USING fts5(
			title, content, summary, linked_content,
			content='articles', content_rowid='id',
			tokenize='porter unicode61'
		)`,
		`CREATE TRIGGER IF NOT EXISTS articles_fts_insert AFTER INSERT ON articles BEGIN
			INSERT INTO articles_fts(rowid, title, content, summary, linked_content)
			VALUES (new.id, new.title, new.content, new.summary, new.linked_content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS articles_fts_delete BEFORE DELETE ON articles BEGIN
			INSERT INTO articles_fts(articles_fts, rowid, title, content, summary, linked_content)
			VALUES ('delete', old.id, old.title, old.content, old.summary, old.linked_content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS articles_fts_update AFTER UPDATE OF title, content, summary, linked_content ON articles BEGIN
			INSERT INTO articles_fts(articles_fts, rowid, title, content, summary, linked_content)
			VALUES ('delete', old.id, old.title, old.content, old.summary, old.linked_content);
			INSERT INTO articles_fts(rowid, title, content, summary, linked_content)
			VALUES (new.id, new.title, new.content, new.summary, new.linked_content);
		END`,
		// Rebuild FTS index to backfill existing articles.
		"INSERT INTO articles_fts(articles_fts) VALUES('rebuild')",
		// Security medium path: flag articles that passed the lower threshold
		// but not the full threshold, for audit without AI summarization.
		"ALTER TABLE read_state ADD COLUMN security_flagged BOOLEAN NOT NULL DEFAULT 0",
		// Link a generated AI digest to the config (newsletter) that produced it;
		// NULL = ad-hoc. SQLite can't add a FK via ALTER — fresh DBs get it from
		// the CREATE TABLE; existing rows just carry the plain column.
		"ALTER TABLE ai_summaries ADD COLUMN newsletter_id INTEGER",
		// Per-user feed tags (many-to-many): group feeds so a digest can follow a
		// tag. New table for existing DBs; fresh DBs get it from the schema.
		`CREATE TABLE IF NOT EXISTS feed_tags (
			user_id INTEGER NOT NULL DEFAULT 1,
			feed_id INTEGER NOT NULL,
			tag TEXT NOT NULL COLLATE NOCASE,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, feed_id, tag),
			FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE
		)`,
		"CREATE INDEX IF NOT EXISTS idx_feed_tags_user_tag ON feed_tags(user_id, tag)",
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
// Each row must select: id, url, title, description, site_url, last_fetched,
// last_error, etag, last_modified, enabled, created_at, consecutive_errors,
// next_fetch_at, status.
func scanFeeds(rows *sql.Rows) ([]Feed, error) {
	var feeds []Feed
	for rows.Next() {
		var f Feed
		var etag, lastMod sql.NullString
		if err := rows.Scan(
			&f.ID, &f.URL, &f.Title, &f.Description, &f.SiteURL, &f.LastFetched, &f.LastError,
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
		slices.Sort(gaps)
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
	n := min(consecutiveErrors,
		// cap multiplier at 64×
		6)
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
// GetFeed returns the feed with the given ID, or an error if not found.
// Unlike GetAllFeeds, this returns the row regardless of enabled/status —
// callers using it for metadata lookup (e.g. embedding context) need the
// title even for disabled feeds.
func (s *SQLiteStore) GetFeed(feedID int64) (*Feed, error) {
	var f Feed
	var etag, lastMod sql.NullString
	err := s.db.QueryRow(
		`SELECT id, url, title, description, site_url, last_fetched, last_error,
		        etag, last_modified, enabled, created_at,
		        consecutive_errors, next_fetch_at, status
		 FROM feeds WHERE id = ?`, feedID,
	).Scan(&f.ID, &f.URL, &f.Title, &f.Description, &f.SiteURL, &f.LastFetched,
		&f.LastError, &etag, &lastMod, &f.Enabled, &f.CreatedAt,
		&f.ConsecutiveErrors, &f.NextFetchAt, &f.Status)
	if err != nil {
		return nil, fmt.Errorf("get feed %d: %w", feedID, err)
	}
	f.ETag = etag.String
	f.LastModified = lastMod.String
	return &f, nil
}

func (s *SQLiteStore) GetAllFeeds() ([]Feed, error) {
	rows, err := s.db.Query(`
		SELECT id, url, title, description, site_url, last_fetched, last_error, etag, last_modified,
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
func (s *SQLiteStore) UpdateReadState(userID, articleID int64, read bool, interestScore, securityScore *float64, securityReason *string, securityFlagged *bool) error {
	var err error
	if interestScore != nil {
		// AI pipeline: record scores, mark ai_scored=1, do not overwrite user's read flag.
		// security_flagged uses COALESCE so a nil caller arg preserves the existing value.
		var flagVal any
		if securityFlagged != nil {
			flagVal = *securityFlagged
		}
		_, err = s.db.Exec(
			`INSERT INTO read_state (user_id, article_id, read, interest_score, security_score, security_reason, security_flagged, ai_scored)
			 VALUES (?, ?, 0, ?, ?, ?, COALESCE(?, 0), 1)
			 ON CONFLICT(user_id, article_id) DO UPDATE SET
			   interest_score = excluded.interest_score,
			   security_score = excluded.security_score,
			   security_reason = excluded.security_reason,
			   security_flagged = COALESCE(excluded.security_flagged, read_state.security_flagged),
			   ai_scored = 1`,
			userID, articleID, interestScore, securityScore, securityReason, flagVal,
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

// MarkSecurityScored records the security verdict for an article and marks it
// ai_scored, WITHOUT writing an interest score. The staged pipeline runs the
// security screen and interest scoring as separate passes: a passing article is
// marked here (interest_score stays NULL) so the curation stage can pick it up
// via GetUnscoredCurationArticles. interest_score is never touched on conflict,
// so re-running the security stage cannot clobber a score the curator already
// wrote. (Hard-blocked and medium-flagged articles are terminal and still go
// through UpdateReadState with interest_score=0.)
func (s *SQLiteStore) MarkSecurityScored(userID, articleID int64, securityScore float64, securityReason string, securityFlagged bool) error {
	_, err := s.db.Exec(
		`INSERT INTO read_state (user_id, article_id, read, security_score, security_reason, security_flagged, ai_scored)
		 VALUES (?, ?, 0, ?, ?, ?, 1)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
		   security_score = excluded.security_score,
		   security_reason = excluded.security_reason,
		   security_flagged = excluded.security_flagged,
		   ai_scored = 1`,
		userID, articleID, securityScore, securityReason, securityFlagged,
	)
	if err != nil {
		return fmt.Errorf("mark security scored: %w", err)
	}
	return nil
}

// SetInterestScore records the interest score from the curation stage WITHOUT
// touching the security verdict the security stage already wrote. The staged
// pipeline runs the two as separate passes, so security_score / security_reason
// / security_flagged must be left untouched here — using UpdateReadState would
// overwrite them with NULL. ai_scored stays set.
func (s *SQLiteStore) SetInterestScore(userID, articleID int64, interestScore float64) error {
	_, err := s.db.Exec(
		`INSERT INTO read_state (user_id, article_id, read, interest_score, ai_scored)
		 VALUES (?, ?, 0, ?, 1)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
		   interest_score = excluded.interest_score,
		   ai_scored = 1`,
		userID, articleID, interestScore,
	)
	if err != nil {
		return fmt.Errorf("set interest score: %w", err)
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

// IncrementAIRetries bumps the retry counter for an article that failed AI processing.
// Creates a read_state row if one doesn't exist yet.
func (s *SQLiteStore) IncrementAIRetries(userID, articleID int64) error {
	_, err := s.db.Exec(
		`INSERT INTO read_state (user_id, article_id, ai_retries)
		 VALUES (?, ?, 1)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
		   ai_retries = read_state.ai_retries + 1`,
		userID, articleID,
	)
	if err != nil {
		return fmt.Errorf("increment ai retries: %w", err)
	}
	return nil
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
			`UPDATE read_state SET ai_scored = 0, ai_retries = 0, interest_score = NULL, security_score = NULL, security_reason = NULL
			 WHERE user_id = ? AND security_score IS NOT NULL AND security_score < ?`,
			userID, belowScore,
		)
	} else {
		result, err = s.db.Exec(
			`UPDATE read_state SET ai_scored = 0, ai_retries = 0, interest_score = NULL, security_score = NULL, security_reason = NULL
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
	args := []any{userID, threshold}
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

// UpdateArticleAISummary stores the AI-generated summary for an article (per-user).
// Clears any prior skip_reason so a previously-skipped article that later
// summarizes successfully transitions back to a normal cached row.
func (s *SQLiteStore) UpdateArticleAISummary(userID, articleID int64, aiSummary string) error {
	_, err := s.db.Exec(
		`INSERT INTO article_summaries (user_id, article_id, ai_summary, skip_reason)
		 VALUES (?, ?, ?, NULL)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
		   ai_summary = excluded.ai_summary,
		   skip_reason = NULL,
		   generated_at = CURRENT_TIMESTAMP`,
		userID, articleID, aiSummary,
	)
	if err != nil {
		return fmt.Errorf("failed to update AI summary: %w", err)
	}
	return nil
}

// MarkSummarizationSkipped records a deterministic summarization rejection
// (e.g., the model can't compress content shorter than the input). Stores an
// empty ai_summary plus a non-null skip_reason so the article drops out of the
// backfill set and isn't retried each cycle.
func (s *SQLiteStore) MarkSummarizationSkipped(userID, articleID int64, reason string) error {
	_, err := s.db.Exec(
		`INSERT INTO article_summaries (user_id, article_id, ai_summary, skip_reason)
		 VALUES (?, ?, '', ?)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
		   skip_reason = excluded.skip_reason,
		   generated_at = CURRENT_TIMESTAMP`,
		userID, articleID, reason,
	)
	if err != nil {
		return fmt.Errorf("failed to mark summarization skipped: %w", err)
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

// GetProcessingStats returns an aggregate snapshot of the AI pipeline state for a
// user's articles (not broken down by feed). Counts mirror the daemon's own
// work-selection predicates: "pending" uses ai_retries < 3 (the retry budget the
// pipeline honours) and "stuck" is everything that has exhausted it.
func (s *SQLiteStore) GetProcessingStats(userID int64) (*ProcessingStats, error) {
	var p ProcessingStats
	err := s.db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN rs.ai_scored = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN (rs.article_id IS NULL OR rs.ai_scored = 0) AND COALESCE(rs.ai_retries, 0) < 3 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN (rs.article_id IS NULL OR rs.ai_scored = 0) AND COALESCE(rs.ai_retries, 0) >= 3 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN rs.security_score >= 7 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN rs.security_score IS NOT NULL AND rs.security_score < 7 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN rs.ai_scored = 1 AND rs.security_score IS NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN rs.interest_score IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM articles a
		JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = ?
		LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = ?`,
		userID, userID,
	).Scan(&p.TotalArticles, &p.Scored, &p.Pending, &p.Stuck,
		&p.SecurityPassed, &p.SecurityRejected, &p.SecuritySkipped, &p.Curated)
	if err != nil {
		return nil, fmt.Errorf("get processing stats (funnel): %w", err)
	}

	err = s.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM article_summaries WHERE user_id = ? AND ai_summary <> ''),
			(SELECT COUNT(*) FROM article_summaries WHERE user_id = ? AND COALESCE(skip_reason, '') <> '')`,
		userID, userID,
	).Scan(&p.Summarized, &p.SummarizeSkipped)
	if err != nil {
		return nil, fmt.Errorf("get processing stats (summaries): %w", err)
	}

	err = s.db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM user_feeds WHERE user_id = ?),
			(SELECT COUNT(*) FROM feeds f JOIN user_feeds uf ON uf.feed_id = f.id
			 WHERE uf.user_id = ? AND f.consecutive_errors > 0)`,
		userID, userID,
	).Scan(&p.FeedsTotal, &p.FeedsErroring)
	if err != nil {
		return nil, fmt.Errorf("get processing stats (feeds): %w", err)
	}
	return &p, nil
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

// GetArticleInterestScores returns a map of article ID → interest score for the
// given user. Articles with no read_state row or a NULL interest_score are omitted.
func (s *SQLiteStore) GetArticleInterestScores(userID int64, articleIDs []int64) (map[int64]float64, error) {
	if len(articleIDs) == 0 {
		return map[int64]float64{}, nil
	}
	placeholders := make([]string, len(articleIDs))
	args := make([]any, 0, len(articleIDs)+1)
	args = append(args, userID)
	for i, id := range articleIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `SELECT article_id, interest_score FROM read_state
		WHERE user_id = ? AND article_id IN (` + strings.Join(placeholders, ",") + `)
		AND interest_score IS NOT NULL`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get article interest scores: %w", err)
	}
	defer rows.Close()
	scores := make(map[int64]float64, len(articleIDs))
	for rows.Next() {
		var id int64
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, fmt.Errorf("scan interest score: %w", err)
		}
		scores[id] = score
	}
	return scores, rows.Err()
}

// GetSecurityScores returns the persisted security_score for each of the given
// articles (user-scoped), skipping any with a NULL score. The curate stage uses
// it to report the verdict the security stage wrote, since that score is not
// carried on the in-memory Article (#119).
func (s *SQLiteStore) GetSecurityScores(userID int64, articleIDs []int64) (map[int64]float64, error) {
	if len(articleIDs) == 0 {
		return map[int64]float64{}, nil
	}
	placeholders := make([]string, len(articleIDs))
	args := make([]any, 0, len(articleIDs)+1)
	args = append(args, userID)
	for i, id := range articleIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `SELECT article_id, security_score FROM read_state
		WHERE user_id = ? AND article_id IN (` + strings.Join(placeholders, ",") + `)
		AND security_score IS NOT NULL`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get security scores: %w", err)
	}
	defer rows.Close()
	scores := make(map[int64]float64, len(articleIDs))
	for rows.Next() {
		var id int64
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, fmt.Errorf("scan security score: %w", err)
		}
		scores[id] = score
	}
	return scores, rows.Err()
}

// UpdateGroupSummary stores or updates the summary for a group
func (s *SQLiteStore) UpdateGroupSummary(groupID int64, headline, summary string, articleCount int, maxInterestScore *float64) error {
	_, err := s.db.Exec(
		`INSERT INTO group_summaries (group_id, headline, summary, article_count, max_interest_score, generated_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(group_id) DO UPDATE SET
		   headline = excluded.headline,
		   summary = excluded.summary,
		   article_count = excluded.article_count,
		   max_interest_score = excluded.max_interest_score,
		   generated_at = CURRENT_TIMESTAMP`,
		groupID, headline, summary, articleCount, maxInterestScore,
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
		"SELECT group_id, headline, summary, article_count, max_interest_score, generated_at FROM group_summaries WHERE group_id = ?",
		groupID,
	).Scan(&gs.GroupID, &gs.Headline, &gs.Summary, &gs.ArticleCount, &gs.MaxInterestScore, &gs.GeneratedAt)
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
	args := []any{userID, groupID, userID}
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

// maxTagLen caps a feed tag's length (runes) to keep the UI and storage sane.
const maxTagLen = 50

// normalizeTag trims surrounding space, collapses internal whitespace runs to a
// single space, and caps the length. Returns "" for an effectively empty tag,
// which callers reject. Case is preserved for display; uniqueness is enforced
// case-insensitively by the schema (COLLATE NOCASE / lower(tag) index).
func normalizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return ""
	}
	tag = strings.Join(strings.Fields(tag), " ")
	if len([]rune(tag)) > maxTagLen {
		tag = string([]rune(tag)[:maxTagLen])
	}
	return tag
}

// AddFeedTag tags a feed for a user (idempotent; case-insensitive duplicate is a no-op).
func (s *SQLiteStore) AddFeedTag(userID, feedID int64, tag string) error {
	tag = normalizeTag(tag)
	if tag == "" {
		return fmt.Errorf("empty tag")
	}
	_, err := s.db.Exec(
		"INSERT OR IGNORE INTO feed_tags (user_id, feed_id, tag) VALUES (?, ?, ?)",
		userID, feedID, tag,
	)
	if err != nil {
		return fmt.Errorf("add feed tag: %w", err)
	}
	return nil
}

// RemoveFeedTag removes a tag from a feed for a user (case-insensitive).
func (s *SQLiteStore) RemoveFeedTag(userID, feedID int64, tag string) error {
	tag = normalizeTag(tag)
	_, err := s.db.Exec(
		"DELETE FROM feed_tags WHERE user_id = ? AND feed_id = ? AND tag = ?",
		userID, feedID, tag,
	)
	if err != nil {
		return fmt.Errorf("remove feed tag: %w", err)
	}
	return nil
}

// GetFeedTags returns one feed's tags for a user, sorted.
func (s *SQLiteStore) GetFeedTags(userID, feedID int64) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT tag FROM feed_tags WHERE user_id = ? AND feed_id = ? ORDER BY tag",
		userID, feedID,
	)
	if err != nil {
		return nil, fmt.Errorf("get feed tags: %w", err)
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan feed tag: %w", err)
		}
		tags = append(tags, t)
	}
	return tags, rows.Err()
}

// GetAllFeedTags returns every tagged feed's tags for a user in one query.
func (s *SQLiteStore) GetAllFeedTags(userID int64) (map[int64][]string, error) {
	rows, err := s.db.Query(
		"SELECT feed_id, tag FROM feed_tags WHERE user_id = ? ORDER BY feed_id, tag",
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("get all feed tags: %w", err)
	}
	defer rows.Close()
	out := make(map[int64][]string)
	for rows.Next() {
		var fid int64
		var t string
		if err := rows.Scan(&fid, &t); err != nil {
			return nil, fmt.Errorf("scan feed tag: %w", err)
		}
		out[fid] = append(out[fid], t)
	}
	return out, rows.Err()
}

// GetUserTags returns the distinct tags a user has applied, sorted.
func (s *SQLiteStore) GetUserTags(userID int64) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT DISTINCT tag FROM feed_tags WHERE user_id = ? ORDER BY tag",
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("get user tags: %w", err)
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan user tag: %w", err)
		}
		tags = append(tags, t)
	}
	return tags, rows.Err()
}

// GetFeedsByTags resolves a set of tags to the distinct feed IDs carrying any of
// them (case-insensitive). Empty input returns nil.
func (s *SQLiteStore) GetFeedsByTags(userID int64, tags []string) ([]int64, error) {
	norm := normalizeTags(tags)
	if len(norm) == 0 {
		return nil, nil
	}
	ph := make([]string, len(norm))
	args := make([]any, 0, len(norm)+1)
	args = append(args, userID)
	for i, t := range norm {
		ph[i] = "?"
		args = append(args, t)
	}
	rows, err := s.db.Query(
		"SELECT DISTINCT feed_id FROM feed_tags WHERE user_id = ? AND tag IN ("+strings.Join(ph, ",")+") ORDER BY feed_id",
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("get feeds by tags: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan feed id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// normalizeTags normalizes a slice of tags, dropping empties and case-insensitive
// duplicates while preserving order.
func normalizeTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		n := normalizeTag(t)
		if n == "" {
			continue
		}
		k := strings.ToLower(n)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
	}
	return out
}

// GetUserFeeds returns all feeds a user is subscribed to.
func (s *SQLiteStore) GetUserFeeds(userID int64) ([]Feed, error) {
	rows, err := s.db.Query(`
		SELECT f.id, f.url, COALESCE(uf.user_title, f.title), f.description, f.site_url, f.last_fetched, f.last_error, f.etag,
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
		SELECT DISTINCT f.id, f.url, f.title, f.description, f.site_url, f.last_fetched, f.last_error,
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
		SELECT DISTINCT f.id, f.url, f.title, f.description, f.site_url, f.last_fetched, f.last_error,
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

// UpdateFeedSiteURL stores the blog homepage URL for a feed.
func (s *SQLiteStore) UpdateFeedSiteURL(feedID int64, siteURL string) error {
	_, err := s.db.Exec("UPDATE feeds SET site_url = ? WHERE id = ?", siteURL, feedID)
	if err != nil {
		return fmt.Errorf("update feed site url: %w", err)
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
		WHERE uf.user_id = ? AND (rs.article_id IS NULL OR rs.ai_scored = 0)
		  AND COALESCE(rs.ai_retries, 0) < 3`,
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
		  AND COALESCE(rs.ai_retries, 0) < 3
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

// GetUnsummarizedScoredArticles returns articles that have been AI-scored with
// a passing security score but lack a cached AI summary. Used by the daemon's
// summary backfill pass to re-attempt summarization that failed transiently
// (e.g., Ollama timeout, garbled output) during the original scoring cycle.
func (s *SQLiteStore) GetUnsummarizedScoredArticles(userID int64, securityThreshold float64, limit int) ([]Article, error) {
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = uf.user_id
		LEFT JOIN article_summaries asumm ON asumm.article_id = a.id AND asumm.user_id = uf.user_id
		WHERE uf.user_id = ?
		  AND rs.ai_scored = 1
		  AND rs.security_score >= ?
		  AND asumm.article_id IS NULL
		ORDER BY a.published_date DESC
		LIMIT ?
	`
	rows, err := s.db.Query(query, userID, securityThreshold, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsummarized scored articles: %w", err)
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

// GetUnscoredCurationArticles returns articles that passed the security screen
// (ai_scored, security_score >= threshold) but have not yet been interest-scored
// (interest_score IS NULL). The staged pipeline runs security and curation as
// separate passes, so this is the backfill input for the curation stage —
// articles stranded between the two when a prior cycle stopped early.
func (s *SQLiteStore) GetUnscoredCurationArticles(userID int64, securityThreshold float64, limit int) ([]Article, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = uf.user_id
		WHERE uf.user_id = ?
		  AND rs.ai_scored = 1
		  AND rs.security_score >= ?
		  AND rs.interest_score IS NULL
		ORDER BY a.published_date DESC
		LIMIT ?`, userID, securityThreshold, limit)
	if err != nil {
		return nil, fmt.Errorf("get unscored curation articles: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

// GetUngroupedEmbeddedArticles returns articles that passed the security screen
// (ai_scored, security_score >= threshold), have a usable embedding (status OK)
// for the given model, belong to no group owned by the user, and were
// published/fetched since the cutoff. It is the cluster stage's recency window:
// breaking news spans fetch cycles, so a story that arrived with a lone
// (then-ungrouped) article last cycle can still be pulled into a group as
// siblings arrive now. The security filter keeps blocked content — which may
// still get embedded — out of clusters. Newest-first, limited.
func (s *SQLiteStore) GetUngroupedEmbeddedArticles(userID int64, model string, securityThreshold float64, since time.Time, limit int) ([]Article, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = uf.user_id
		JOIN article_embeddings ae ON ae.article_id = a.id
		    AND ae.embedding_model = ? AND ae.status = ?
		WHERE uf.user_id = ?
		  AND rs.ai_scored = 1
		  AND rs.security_score >= ?
		  AND COALESCE(a.published_date, a.fetched_date) >= ?
		  AND NOT EXISTS (
		      SELECT 1 FROM article_group_members agm
		      JOIN article_groups ag ON agm.group_id = ag.id
		      WHERE agm.article_id = a.id AND ag.user_id = uf.user_id
		  )
		ORDER BY COALESCE(a.published_date, a.fetched_date) DESC
		LIMIT ?`, model, EmbedStatusOK, userID, securityThreshold, since, limit)
	if err != nil {
		return nil, fmt.Errorf("get ungrouped embedded articles: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

// GetUnreadArticlesForUser returns unread articles from feeds the user subscribes to
func (s *SQLiteStore) GetUnreadArticlesForUser(userID int64, limit, offset int, filterThreshold *int) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClause(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(rs.security_flagged, 0) AS security_flagged
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
	args := []any{userID, userID, userID}
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
			&a.Content, &a.Summary, &a.Author, &a.PublishedDate, &a.FetchedDate,
			&a.SecurityFlagged); err != nil {
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
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(rs.security_flagged, 0) AS security_flagged
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
	args := []any{userID, userID, feedID, userID}
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
			&a.Content, &a.Summary, &a.Author, &a.PublishedDate, &a.FetchedDate,
			&a.SecurityFlagged); err != nil {
			return nil, fmt.Errorf("failed to scan article: %w", err)
		}
		articles = append(articles, a)
	}
	return articles, rows.Err()
}

// UpdateGroupEmbedding stores or updates the centroid embedding for a group.
func (s *SQLiteStore) UpdateGroupEmbedding(groupID int64, embedding []byte, model string) error {
	_, err := s.db.Exec("UPDATE article_groups SET embedding = ?, embedding_model = ? WHERE id = ?", embedding, model, groupID)
	if err != nil {
		return fmt.Errorf("update group embedding: %w", err)
	}
	return nil
}

// GetGroupsWithEmbeddings returns groups for a user that have a centroid embedding
// produced by the specified model. Embeddings from other models are ignored.
func (s *SQLiteStore) GetGroupsWithEmbeddings(userID int64, model string) ([]ArticleGroupWithEmbedding, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, topic, display_name, muted, embedding, created_at, updated_at
		 FROM article_groups
		 WHERE user_id = ? AND embedding IS NOT NULL AND embedding_model = ?`,
		userID, model,
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
	args := []any{userID, userID}
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
func filterScoreClause(userID int64, threshold *int) (string, []any) {
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
	return sql, []any{userID, userID, *threshold}
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
	var args []any

	if feedID != nil {
		query = `SELECT id, user_id, feed_id, axis, value, score, created_at
				 FROM filter_rules WHERE user_id = ? AND (feed_id IS NULL OR feed_id = ?)
				 ORDER BY axis, value`
		args = []any{userID, *feedID}
	} else {
		query = `SELECT id, user_id, feed_id, axis, value, score, created_at
				 FROM filter_rules WHERE user_id = ?
				 ORDER BY axis, value`
		args = []any{userID}
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
		SELECT DISTINCT f.id, f.url, f.title, f.description, f.site_url,
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

// --- Search methods ---

// SearchArticlesFTS performs a full-text search using FTS5 MATCH, scoped to feeds
// the user is subscribed to. Results are ordered by BM25 rank.
func (s *SQLiteStore) SearchArticlesFTS(userID int64, query string, limit, offset int) ([]Article, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN articles_fts fts ON fts.rowid = a.id
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		WHERE uf.user_id = ? AND articles_fts MATCH ?
		ORDER BY rank
		LIMIT ? OFFSET ?`,
		userID, query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

// StoreArticleEmbedding upserts a successful embedding vector. Resets
// status to ok, attempts to 0, and clears any prior error_message and
// last_attempted_at — the success "wins" over any prior error/skip
// state for this article.
func (s *SQLiteStore) StoreArticleEmbedding(articleID int64, embedding []byte, model string) error {
	_, err := s.db.Exec(`
		INSERT INTO article_embeddings (article_id, embedding, embedding_model, status, attempts, error_message, last_attempted_at)
		VALUES (?, ?, ?, ?, 0, NULL, NULL)
		ON CONFLICT(article_id) DO UPDATE SET
			embedding = excluded.embedding,
			embedding_model = excluded.embedding_model,
			status = ?,
			attempts = 0,
			error_message = NULL,
			last_attempted_at = NULL,
			created_at = CURRENT_TIMESTAMP`,
		articleID, embedding, model, EmbedStatusOK, EmbedStatusOK)
	return err
}

// embedSentinelBytes is the placeholder written to the embedding column
// for non-ok status rows. The schema declares embedding NOT NULL, so we
// need some bytes to satisfy the constraint; one byte distinguishes
// these rows from real vectors (≥ 4 bytes for any single float32) and
// matches the legacy sentinel encoding for backward compatibility.
var embedSentinelBytes = []byte{0}

// MarkArticleEmbeddingSkipped records a deterministic skip (e.g. body
// too short to embed). status=too_short, attempts=0, error_message=NULL.
// Never returned by GetArticlesWithoutEmbeddings — permanent skip.
func (s *SQLiteStore) MarkArticleEmbeddingSkipped(articleID int64, model string) error {
	_, err := s.db.Exec(`
		INSERT INTO article_embeddings (article_id, embedding, embedding_model, status, attempts, error_message, last_attempted_at)
		VALUES (?, ?, ?, ?, 0, NULL, NULL)
		ON CONFLICT(article_id) DO UPDATE SET
			embedding_model = excluded.embedding_model,
			status = ?,
			attempts = 0,
			error_message = NULL,
			last_attempted_at = NULL,
			created_at = CURRENT_TIMESTAMP`,
		articleID, embedSentinelBytes, model, EmbedStatusTooShort, EmbedStatusTooShort)
	return err
}

// MarkArticleEmbeddingFailed records a transient failure. status=error,
// attempts=attempts+1 on update (1 on first insert), error_message captures
// the failure text, last_attempted_at = now() to gate the next retry by
// EmbedRetryCooldown. Eligible for retry by GetArticlesWithoutEmbeddings
// while attempts < EmbedMaxAttempts AND the cooldown has elapsed.
//
// last_attempted_at is bound as a Go time.Time (not CURRENT_TIMESTAMP)
// so writes and the cutoff comparison in GetArticlesWithoutEmbeddings
// share a single string format under the SQLite driver — mixing
// CURRENT_TIMESTAMP ("YYYY-MM-DD HH:MM:SS") with driver-formatted
// time.Time values broke the comparison subtly during cooldown checks.
func (s *SQLiteStore) MarkArticleEmbeddingFailed(articleID int64, model, errMsg string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`
		INSERT INTO article_embeddings (article_id, embedding, embedding_model, status, attempts, error_message, last_attempted_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(article_id) DO UPDATE SET
			embedding_model = excluded.embedding_model,
			status = ?,
			attempts = article_embeddings.attempts + 1,
			error_message = excluded.error_message,
			last_attempted_at = excluded.last_attempted_at,
			created_at = excluded.last_attempted_at`,
		articleID, embedSentinelBytes, model, EmbedStatusError, errMsg, now, EmbedStatusError)
	return err
}

// GetArticleEmbeddings returns all article embeddings for a user's subscribed feeds,
// filtered by the specified embedding model.
func (s *SQLiteStore) GetArticleEmbeddings(userID int64, model string) ([]ArticleEmbeddingRow, error) {
	rows, err := s.db.Query(`
		SELECT ae.article_id, ae.embedding
		FROM article_embeddings ae
		JOIN articles a ON a.id = ae.article_id
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		WHERE uf.user_id = ? AND ae.embedding_model = ?`,
		userID, model)
	if err != nil {
		return nil, fmt.Errorf("get article embeddings: %w", err)
	}
	defer rows.Close()
	var result []ArticleEmbeddingRow
	for rows.Next() {
		var r ArticleEmbeddingRow
		if err := rows.Scan(&r.ArticleID, &r.Embedding); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetArticleEmbeddingsByIDs returns the usable (status OK) embeddings for the
// given article IDs and model, as raw blobs for the caller to decode. The
// cluster stage uses it to load the cohort's vectors in one query rather than
// every embedding the user has. Returns nil for an empty ID set.
func (s *SQLiteStore) GetArticleEmbeddingsByIDs(articleIDs []int64, model string) ([]ArticleEmbeddingRow, error) {
	if len(articleIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(articleIDs))
	args := make([]any, 0, len(articleIDs)+2)
	for i, id := range articleIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, model, EmbedStatusOK)
	rows, err := s.db.Query(
		`SELECT article_id, embedding FROM article_embeddings
		 WHERE article_id IN (`+strings.Join(placeholders, ",")+`)
		   AND embedding_model = ? AND status = ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("get article embeddings by ids: %w", err)
	}
	defer rows.Close()
	var result []ArticleEmbeddingRow
	for rows.Next() {
		var r ArticleEmbeddingRow
		if err := rows.Scan(&r.ArticleID, &r.Embedding); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ResetAllArticleEmbeddings deletes every row in article_embeddings.
// Returns the number of rows deleted. The daemon's per-cycle backfill
// repopulates from articles missing an embedding for the current model.
//
// Used after an embed-format change (e.g. switching the input from
// title+content to a metadata-enriched record) when every existing
// vector becomes incompatible with new ones.
func (s *SQLiteStore) ResetAllArticleEmbeddings() (int64, error) {
	r, err := s.db.Exec(`DELETE FROM article_embeddings`)
	if err != nil {
		return 0, fmt.Errorf("reset article embeddings: %w", err)
	}
	return r.RowsAffected()
}

// ResetAllGroupEmbeddings clears the embedding centroid and stored model
// name on every row in article_groups. Returns the number of rows
// updated. Group memberships are preserved; centroids will repopulate
// as articles re-embed and rejoin groups via the scoring pipeline.
//
// Pairs with ResetAllArticleEmbeddings: stale group centroids built from
// the old vector format are silent-garbage hazards if left behind, so
// any reset of article embeddings should also clear centroids.
func (s *SQLiteStore) ResetAllGroupEmbeddings() (int64, error) {
	r, err := s.db.Exec(`UPDATE article_groups SET embedding = NULL, embedding_model = ''`)
	if err != nil {
		return 0, fmt.Errorf("reset group embeddings: %w", err)
	}
	return r.RowsAffected()
}

// ResetStuckEmbeddings clears the retry budget on rows that exhausted
// EmbedMaxAttempts. Sets attempts=0, last_attempted_at=NULL,
// error_message=NULL on rows where status=error AND attempts >=
// EmbedMaxAttempts AND embedding_model = model. The next backfill cycle
// then picks them up again with a fresh budget.
//
// errorPattern is an optional SQL LIKE pattern (use "" for unfiltered).
// Useful for narrowing the reset to a specific cause — e.g. resetting
// only "%HTTP 403%" rows after fixing a rate-limit misconfiguration,
// without touching rows stuck on a different error class.
//
// Status stays at EmbedStatusError so reset rows stay distinguishable
// from never-attempted rows; the (status, attempts<5, last_attempted_at
// IS NULL) shape is what GetArticlesWithoutEmbeddings looks for. Returns
// the number of rows updated.
func (s *SQLiteStore) ResetStuckEmbeddings(model, errorPattern string) (int64, error) {
	var (
		r   sql.Result
		err error
	)
	if errorPattern == "" {
		r, err = s.db.Exec(`
			UPDATE article_embeddings
			SET attempts = 0, last_attempted_at = NULL, error_message = NULL
			WHERE embedding_model = ? AND status = ? AND attempts >= ?`,
			model, EmbedStatusError, EmbedMaxAttempts)
	} else {
		r, err = s.db.Exec(`
			UPDATE article_embeddings
			SET attempts = 0, last_attempted_at = NULL, error_message = NULL
			WHERE embedding_model = ? AND status = ? AND attempts >= ?
			  AND error_message LIKE ?`,
			model, EmbedStatusError, EmbedMaxAttempts, errorPattern)
	}
	if err != nil {
		return 0, fmt.Errorf("reset stuck embeddings: %w", err)
	}
	return r.RowsAffected()
}

// GetArticlesWithoutEmbeddings returns articles eligible for an embedding
// pass under the given model: either no row exists yet for this model,
// or the row is in error state with retries remaining AND the
// EmbedRetryCooldown has elapsed since the last attempt. status=too_short
// rows are NEVER returned (permanent skip). status=error rows are
// retried until attempts reaches EmbedMaxAttempts, at which point they
// stay sentineled but stop consuming retry budget.
//
// Rows migrated from before the cooldown column existed have
// last_attempted_at IS NULL; those are eligible immediately, which is
// the right behavior — we have no record of when they last failed.
func (s *SQLiteStore) GetArticlesWithoutEmbeddings(model string, limit int) ([]Article, error) {
	// UTC matches the format used by MarkArticleEmbeddingFailed when
	// writing last_attempted_at. Mixing local-time and UTC produces
	// driver-formatted strings ("...-05:00" vs "...Z") that don't
	// compare correctly under SQLite's string-based DATETIME compare.
	cutoff := time.Now().UTC().Add(-EmbedRetryCooldown)
	rows, err := s.db.Query(`
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		LEFT JOIN article_embeddings ae ON a.id = ae.article_id AND ae.embedding_model = ?
		WHERE ae.article_id IS NULL
		   OR (ae.status = ? AND ae.attempts < ?
		       AND (ae.last_attempted_at IS NULL OR ae.last_attempted_at < ?))
		ORDER BY a.published_date DESC
		LIMIT ?`, model, EmbedStatusError, EmbedMaxAttempts, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("get articles without embeddings: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

// --- Newsletter methods ---

func (s *SQLiteStore) CreateNewsletter(n *Newsletter) (int64, error) {
	configJSON, err := json.Marshal(n.Config)
	if err != nil {
		return 0, fmt.Errorf("marshal newsletter config: %w", err)
	}
	result, err := s.db.Exec(`
		INSERT INTO newsletters (user_id, name, schedule, config_json, prompt_template, email_recipient, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		n.UserID, n.Name, n.Schedule, string(configJSON), n.PromptTemplate, n.EmailRecipient, n.Enabled)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *SQLiteStore) UpdateNewsletter(n *Newsletter) error {
	configJSON, err := json.Marshal(n.Config)
	if err != nil {
		return fmt.Errorf("marshal newsletter config: %w", err)
	}
	_, err = s.db.Exec(`
		UPDATE newsletters
		SET name = ?, schedule = ?, config_json = ?, prompt_template = ?,
		    email_recipient = ?, enabled = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		n.Name, n.Schedule, string(configJSON), n.PromptTemplate, n.EmailRecipient, n.Enabled, n.ID)
	return err
}

func (s *SQLiteStore) DeleteNewsletter(newsletterID int64) error {
	_, err := s.db.Exec(`DELETE FROM newsletters WHERE id = ?`, newsletterID)
	return err
}

func (s *SQLiteStore) GetNewsletter(newsletterID int64) (*Newsletter, error) {
	var n Newsletter
	var configJSON string
	err := s.db.QueryRow(`
		SELECT id, user_id, name, schedule, config_json, prompt_template,
		       email_recipient, enabled, last_generated_at, created_at, updated_at
		FROM newsletters WHERE id = ?`, newsletterID).Scan(
		&n.ID, &n.UserID, &n.Name, &n.Schedule, &configJSON, &n.PromptTemplate,
		&n.EmailRecipient, &n.Enabled, &n.LastGeneratedAt, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(configJSON), &n.Config) //nolint:errcheck
	return &n, nil
}

func (s *SQLiteStore) GetUserNewsletters(userID int64) ([]Newsletter, error) {
	rows, err := s.db.Query(`
		SELECT id, user_id, name, schedule, config_json, prompt_template,
		       email_recipient, enabled, last_generated_at, created_at, updated_at
		FROM newsletters WHERE user_id = ? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var newsletters []Newsletter
	for rows.Next() {
		var n Newsletter
		var configJSON string
		if err := rows.Scan(&n.ID, &n.UserID, &n.Name, &n.Schedule, &configJSON, &n.PromptTemplate,
			&n.EmailRecipient, &n.Enabled, &n.LastGeneratedAt, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(configJSON), &n.Config) //nolint:errcheck
		newsletters = append(newsletters, n)
	}
	return newsletters, rows.Err()
}

func (s *SQLiteStore) GetDueNewsletters(schedule string) ([]Newsletter, error) {
	var interval string
	switch schedule {
	case "hourly":
		interval = "-1 hour"
	case "daily":
		interval = "-1 day"
	default:
		return nil, nil // manual newsletters are never "due"
	}
	rows, err := s.db.Query(`
		SELECT id, user_id, name, schedule, config_json, prompt_template,
		       email_recipient, enabled, last_generated_at, created_at, updated_at
		FROM newsletters
		WHERE enabled = 1 AND schedule = ?
		  AND (last_generated_at IS NULL OR last_generated_at < datetime('now', ?))`,
		schedule, interval)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var newsletters []Newsletter
	for rows.Next() {
		var n Newsletter
		var configJSON string
		if err := rows.Scan(&n.ID, &n.UserID, &n.Name, &n.Schedule, &configJSON, &n.PromptTemplate,
			&n.EmailRecipient, &n.Enabled, &n.LastGeneratedAt, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(configJSON), &n.Config) //nolint:errcheck
		newsletters = append(newsletters, n)
	}
	return newsletters, rows.Err()
}

func (s *SQLiteStore) CreateNewsletterIssue(issue *NewsletterIssue) (int64, error) {
	articleIDsJSON, _ := json.Marshal(issue.ArticleIDs) //nolint:errcheck
	result, err := s.db.Exec(`
		INSERT INTO newsletter_issues (newsletter_id, headline, content_html, content_text, article_ids_json)
		VALUES (?, ?, ?, ?, ?)`,
		issue.NewsletterID, issue.Headline, issue.ContentHTML, issue.ContentText, string(articleIDsJSON))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *SQLiteStore) GetNewsletterIssue(issueID int64) (*NewsletterIssue, error) {
	var issue NewsletterIssue
	var articleIDsJSON string
	err := s.db.QueryRow(`
		SELECT id, newsletter_id, headline, content_html, content_text, article_ids_json, generated_at, sent_at
		FROM newsletter_issues WHERE id = ?`, issueID).Scan(
		&issue.ID, &issue.NewsletterID, &issue.Headline, &issue.ContentHTML, &issue.ContentText,
		&articleIDsJSON, &issue.GeneratedAt, &issue.SentAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(articleIDsJSON), &issue.ArticleIDs) //nolint:errcheck
	return &issue, nil
}

func (s *SQLiteStore) GetLatestNewsletterIssue(newsletterID int64) (*NewsletterIssue, error) {
	var issue NewsletterIssue
	var articleIDsJSON string
	err := s.db.QueryRow(`
		SELECT id, newsletter_id, headline, content_html, content_text, article_ids_json, generated_at, sent_at
		FROM newsletter_issues WHERE newsletter_id = ?
		ORDER BY generated_at DESC LIMIT 1`, newsletterID).Scan(
		&issue.ID, &issue.NewsletterID, &issue.Headline, &issue.ContentHTML, &issue.ContentText,
		&articleIDsJSON, &issue.GeneratedAt, &issue.SentAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(articleIDsJSON), &issue.ArticleIDs) //nolint:errcheck
	return &issue, nil
}

func (s *SQLiteStore) GetNewsletterIssues(newsletterID int64, limit, offset int) ([]NewsletterIssue, error) {
	rows, err := s.db.Query(`
		SELECT id, newsletter_id, headline, content_text, generated_at, sent_at
		FROM newsletter_issues WHERE newsletter_id = ?
		ORDER BY generated_at DESC LIMIT ? OFFSET ?`,
		newsletterID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var issues []NewsletterIssue
	for rows.Next() {
		var issue NewsletterIssue
		if err := rows.Scan(&issue.ID, &issue.NewsletterID, &issue.Headline,
			&issue.ContentText, &issue.GeneratedAt, &issue.SentAt); err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

func (s *SQLiteStore) MarkNewsletterIssueSent(issueID int64) error {
	_, err := s.db.Exec(`UPDATE newsletter_issues SET sent_at = CURRENT_TIMESTAMP WHERE id = ?`, issueID)
	return err
}

func (s *SQLiteStore) UpdateNewsletterLastGenerated(newsletterID int64) error {
	_, err := s.db.Exec(`UPDATE newsletters SET last_generated_at = CURRENT_TIMESTAMP WHERE id = ?`, newsletterID)
	return err
}

func (s *SQLiteStore) GetNewsletterStats(userID int64) ([]NewsletterStats, error) {
	rows, err := s.db.Query(`
		SELECT n.id, n.name, COUNT(ni.id) AS issue_count
		FROM newsletters n
		LEFT JOIN newsletter_issues ni ON ni.newsletter_id = n.id
		WHERE n.user_id = ? AND n.enabled = 1
		GROUP BY n.id
		ORDER BY n.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stats []NewsletterStats
	for rows.Next() {
		var s NewsletterStats
		if err := rows.Scan(&s.NewsletterID, &s.Name, &s.IssueCount); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

func (s *SQLiteStore) GetNewsletterArticles(userID int64, config *NewsletterConfig, since *time.Time, limit int) ([]Article, []float64, error) {
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date, rs.interest_score
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = uf.user_id
		WHERE uf.user_id = ?`
	args := []any{userID}

	if config.MinInterestScore > 0 {
		query += ` AND rs.interest_score >= ?`
		args = append(args, config.MinInterestScore)
	}
	if config.MinSecurityScore > 0 {
		query += ` AND rs.security_score >= ?`
		args = append(args, config.MinSecurityScore)
	}
	if len(config.IncludeFeeds) > 0 {
		placeholders := make([]string, len(config.IncludeFeeds))
		for i, fid := range config.IncludeFeeds {
			placeholders[i] = "?"
			args = append(args, fid)
		}
		query += ` AND a.feed_id IN (` + strings.Join(placeholders, ",") + `)`
	}
	if len(config.ExcludeFeeds) > 0 {
		placeholders := make([]string, len(config.ExcludeFeeds))
		for i, fid := range config.ExcludeFeeds {
			placeholders[i] = "?"
			args = append(args, fid)
		}
		query += ` AND a.feed_id NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	if len(config.IncludeCategories) > 0 {
		placeholders := make([]string, len(config.IncludeCategories))
		for i, cat := range config.IncludeCategories {
			placeholders[i] = "?"
			args = append(args, cat)
		}
		query += ` AND EXISTS (SELECT 1 FROM article_categories ac WHERE ac.article_id = a.id AND ac.category IN (` + strings.Join(placeholders, ",") + `))`
	}
	if len(config.ExcludeCategories) > 0 {
		placeholders := make([]string, len(config.ExcludeCategories))
		for i, cat := range config.ExcludeCategories {
			placeholders[i] = "?"
			args = append(args, cat)
		}
		query += ` AND NOT EXISTS (SELECT 1 FROM article_categories ac WHERE ac.article_id = a.id AND ac.category IN (` + strings.Join(placeholders, ",") + `))`
	}
	if since != nil {
		query += ` AND a.published_date > ?`
		args = append(args, since)
	}

	query += ` ORDER BY rs.interest_score DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("get newsletter articles: %w", err)
	}
	defer rows.Close()

	var articles []Article
	var scores []float64
	for rows.Next() {
		var a Article
		var score float64
		if err := rows.Scan(&a.ID, &a.FeedID, &a.GUID, &a.Title, &a.URL,
			&a.Content, &a.Summary, &a.Author, &a.PublishedDate, &a.FetchedDate, &score); err != nil {
			return nil, nil, err
		}
		articles = append(articles, a)
		scores = append(scores, score)
	}
	return articles, scores, rows.Err()
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
