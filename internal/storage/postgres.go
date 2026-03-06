package storage

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresStore implements the Store interface using PostgreSQL.
type PostgresStore struct {
	db *tracedDB
}

// Compile-time check that PostgresStore implements Store.
var _ Store = (*PostgresStore)(nil)

// NewPostgresStore opens a PostgreSQL connection, verifies it, and initializes
// the schema (all DDL statements are idempotent).
func NewPostgresStore(dsn string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to connect to postgres: %w", err)
	}
	if _, err := db.Exec(SchemaPostgres); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize postgres schema: %w", err)
	}
	return &PostgresStore{db: &tracedDB{DB: db, useRebind: true}}, nil
}

func (s *PostgresStore) Close() error { return s.db.Close() }

// --- Internal helpers ---

// computeFeedBaseInterval queries the last 11 article publish dates for feedID
// and returns a fetch interval based on posting recency and frequency.
func (s *PostgresStore) computeFeedBaseInterval(feedID int64) time.Duration {
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
		return 24 * time.Hour
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
	return pickFetchIntervalPG(lastPostAge, medianGap)
}

// pickFetchIntervalPG mirrors the SQLite version but lives in postgres.go to
// avoid a duplicate-function collision.
func pickFetchIntervalPG(lastPostAge, medianPostInterval time.Duration) time.Duration {
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

// --- Users ---

func (s *PostgresStore) CreateUser(name string) (int64, error) {
	var id int64
	err := s.db.QueryRow("INSERT INTO users (name) VALUES (?) RETURNING id", name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

func (s *PostgresStore) GetUserByName(name string) (*User, error) {
	var u User
	var email sql.NullString
	var oidcSub sql.NullString
	err := s.db.QueryRow(
		"SELECT id, name, oidc_sub, email, created_at FROM users WHERE name = ?", name,
	).Scan(&u.ID, &u.Name, &oidcSub, &email, &u.CreatedAt)
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

func (s *PostgresStore) GetUserByOIDCSub(sub string) (*User, error) {
	var u User
	var email sql.NullString
	var oidcSub sql.NullString
	err := s.db.QueryRow(
		"SELECT id, name, oidc_sub, email, created_at FROM users WHERE oidc_sub = ?", sub,
	).Scan(&u.ID, &u.Name, &oidcSub, &email, &u.CreatedAt)
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

func (s *PostgresStore) CreateUserWithOIDC(name, email, sub string) (*User, error) {
	var emailVal *string
	if email != "" {
		emailVal = &email
	}
	var id int64
	err := s.db.QueryRow(
		"INSERT INTO users (name, oidc_sub, email) VALUES (?, ?, ?) RETURNING id",
		name, sub, emailVal,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("create OIDC user: %w", err)
	}
	u := &User{ID: id, Name: name, OIDCSub: &sub, Email: emailVal}
	return u, nil
}

func (s *PostgresStore) UpdateUserOIDCEmail(id int64, email string) error {
	_, err := s.db.Exec("UPDATE users SET email = ? WHERE id = ?", email, id)
	return err
}

func (s *PostgresStore) ListUsers() ([]User, error) {
	rows, err := s.db.Query("SELECT id, name, oidc_sub, email, created_at FROM users ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		var email sql.NullString
		var oidcSub sql.NullString
		if err := rows.Scan(&u.ID, &u.Name, &oidcSub, &email, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		if email.Valid {
			u.Email = &email.String
		}
		if oidcSub.Valid {
			u.OIDCSub = &oidcSub.String
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// --- User prompts ---

func (s *PostgresStore) GetUserPrompt(userID int64, promptType string) (string, error) {
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

func (s *PostgresStore) GetUserPromptTemperature(userID int64, promptType string) (float64, error) {
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

func (s *PostgresStore) GetUserPromptModel(userID int64, promptType string) (string, error) {
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

func (s *PostgresStore) SetUserPrompt(userID int64, promptType, promptTemplate string, temperature *float64, model *string) error {
	_, err := s.db.Exec(
		`INSERT INTO user_prompts (user_id, prompt_type, prompt_template, temperature, model, updated_at)
		 VALUES (?, ?, ?, ?, ?, NOW())
		 ON CONFLICT(user_id, prompt_type) DO UPDATE SET
		   prompt_template = excluded.prompt_template,
		   temperature = excluded.temperature,
		   model = COALESCE(excluded.model, user_prompts.model),
		   updated_at = NOW()`,
		userID, promptType, promptTemplate, temperature, model,
	)
	return err
}

func (s *PostgresStore) DeleteUserPrompt(userID int64, promptType string) error {
	_, err := s.db.Exec(
		"DELETE FROM user_prompts WHERE user_id = ? AND prompt_type = ?",
		userID, promptType,
	)
	return err
}

func (s *PostgresStore) ListUserPrompts(userID int64) ([]UserPrompt, error) {
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
		if err := rows.Scan(&p.PromptType, &p.PromptTemplate, &temp, &model, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.UserID = userID
		p.Model = model.String
		if temp.Valid {
			tempVal := temp.Float64
			p.Temperature = &tempVal
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
}

// --- User preferences ---

func (s *PostgresStore) GetUserPreference(userID int64, key string) (string, error) {
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

func (s *PostgresStore) SetUserPreference(userID int64, key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO user_preferences (user_id, key, value)
		 VALUES (?, ?, ?)
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`,
		userID, key, value,
	)
	return err
}

func (s *PostgresStore) GetAllUserPreferences(userID int64) (map[string]string, error) {
	rows, err := s.db.Query(
		"SELECT key, value FROM user_preferences WHERE user_id = ?", userID,
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

func (s *PostgresStore) DeleteUserPreference(userID int64, key string) error {
	_, err := s.db.Exec(
		"DELETE FROM user_preferences WHERE user_id = ? AND key = ?",
		userID, key,
	)
	return err
}

// --- Read state ---

func (s *PostgresStore) UpdateStarred(userID, articleID int64, starred bool) error {
	_, err := s.db.Exec(
		`INSERT INTO read_state (user_id, article_id, starred)
		 VALUES (?, ?, ?)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET starred = excluded.starred`,
		userID, articleID, starred,
	)
	if err != nil {
		return fmt.Errorf("update starred: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateReadState(userID, articleID int64, read bool, interestScore, securityScore *float64) error {
	var err error
	if interestScore != nil {
		_, err = s.db.Exec(
			`INSERT INTO read_state (user_id, article_id, read, interest_score, security_score, ai_scored)
			 VALUES (?, ?, FALSE, ?, ?, TRUE)
			 ON CONFLICT(user_id, article_id) DO UPDATE SET
			   interest_score = excluded.interest_score,
			   security_score = excluded.security_score,
			   ai_scored = TRUE`,
			userID, articleID, interestScore, securityScore,
		)
	} else {
		_, err = s.db.Exec(
			`INSERT INTO read_state (user_id, article_id, read, read_date)
			 VALUES (?, ?, ?, NOW())
			 ON CONFLICT(user_id, article_id) DO UPDATE SET
			   read = excluded.read,
			   read_date = NOW()`,
			userID, articleID, read,
		)
	}
	if err != nil {
		return fmt.Errorf("failed to update read state: %w", err)
	}
	return nil
}

// --- Feeds ---

func (s *PostgresStore) AddFeed(url, title, description string) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		"INSERT INTO feeds (url, title, description) VALUES (?, ?, ?) RETURNING id",
		url, title, description,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("failed to add feed: %w", err)
	}
	return id, nil
}

func (s *PostgresStore) GetAllFeeds() ([]Feed, error) {
	rows, err := s.db.Query(`
		SELECT id, url, title, description, last_fetched, last_error, etag, last_modified,
		       enabled, created_at, consecutive_errors, next_fetch_at, status
		FROM feeds
		WHERE enabled = TRUE AND status = 'active'
		  AND (next_fetch_at IS NULL OR next_fetch_at <= NOW())`)
	if err != nil {
		return nil, fmt.Errorf("failed to get feeds: %w", err)
	}
	defer rows.Close()
	return scanFeeds(rows)
}

func (s *PostgresStore) UpdateFeedError(feedID int64, errMsg string) error {
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
		return nil
	}

	if consecutiveErrors >= 5 && (!lastFetched.Valid || time.Since(lastFetched.Time) > 30*24*time.Hour) {
		s.db.Exec("UPDATE feeds SET status = 'dead' WHERE id = ?", feedID) //nolint:errcheck
		return nil
	}

	base := s.computeFeedBaseInterval(feedID)
	next := time.Now().Add(applyErrorBackoff(base, consecutiveErrors))
	s.db.Exec("UPDATE feeds SET next_fetch_at = ? WHERE id = ?", next, feedID) //nolint:errcheck
	return nil
}

func (s *PostgresStore) ClearFeedError(feedID int64) error {
	return s.UpdateFeedLastFetched(feedID)
}

func (s *PostgresStore) MarkFeedFetched(feedID int64) error {
	_, err := s.db.Exec(
		`UPDATE feeds SET last_fetched = NOW(), last_error = NULL,
		 consecutive_errors = 0, status = 'active' WHERE id = ?`,
		feedID,
	)
	if err != nil {
		return fmt.Errorf("failed to mark feed fetched: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateFeedCacheHeaders(feedID int64, etag, lastModified string) error {
	_, err := s.db.Exec(
		"UPDATE feeds SET etag = ?, last_modified = ? WHERE id = ?",
		etag, lastModified, feedID,
	)
	if err != nil {
		return fmt.Errorf("failed to update feed cache headers: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateFeedLastFetched(feedID int64) error {
	base := s.computeFeedBaseInterval(feedID)
	next := time.Now().Add(base)
	_, err := s.db.Exec(
		`UPDATE feeds SET last_fetched = NOW(), last_error = NULL,
		 consecutive_errors = 0, status = 'active', next_fetch_at = ? WHERE id = ?`,
		next, feedID,
	)
	if err != nil {
		return fmt.Errorf("failed to update feed last_fetched: %w", err)
	}
	return nil
}

func (s *PostgresStore) RenameFeed(feedID int64, title string) error {
	_, err := s.db.Exec("UPDATE feeds SET title = ? WHERE id = ?", title, feedID)
	if err != nil {
		return fmt.Errorf("failed to rename feed: %w", err)
	}
	return nil
}

// --- Articles ---

func (s *PostgresStore) FindDuplicateArticle(title string, publishedDate *time.Time) (int64, error) {
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

func (s *PostgresStore) AddArticle(article *Article) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO articles (feed_id, guid, title, url, content, summary, author, published_date)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(feed_id, guid) DO NOTHING
		 RETURNING id`,
		article.FeedID, article.GUID, article.Title, article.URL,
		article.Content, article.Summary, article.Author, article.PublishedDate,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil // duplicate
	}
	if err != nil {
		return 0, fmt.Errorf("failed to add article: %w", err)
	}
	return id, nil
}

func (s *PostgresStore) GetUnreadArticles(limit int) ([]Article, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		LEFT JOIN read_state rs ON a.id = rs.article_id
		WHERE rs.article_id IS NULL OR rs.read = FALSE
		ORDER BY a.published_date DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get unread articles: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

func (s *PostgresStore) GetArticle(articleID int64) (*Article, error) {
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

func (s *PostgresStore) GetArticlesByInterestScore(userID int64, threshold float64, limit, offset int, filterThreshold *int) ([]Article, []float64, error) {
	filterSQL, filterArgs := filterScoreClausePG(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(rs.interest_score, 0) * (1.0 / (1.0 + GREATEST(0, EXTRACT(epoch FROM (NOW() - COALESCE(a.published_date, a.fetched_date))) / 86400.0) * 0.1)) AS decayed_score
		FROM articles a
		JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE rs.interest_score >= ? AND rs.read = FALSE
		` + filterSQL + `
		ORDER BY decayed_score DESC
		LIMIT ? OFFSET ?`
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

func (s *PostgresStore) GetUnreadArticlesForUser(userID int64, limit, offset int, filterThreshold *int) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClausePG(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND (rs.article_id IS NULL OR rs.read = FALSE)
		` + filterSQL + `
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?`
	args := []interface{}{userID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get unread articles for user: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

func (s *PostgresStore) GetUnreadArticlesByFeed(userID, feedID int64, limit, offset int, filterThreshold *int) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClausePG(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND a.feed_id = ? AND (rs.article_id IS NULL OR rs.read = FALSE)
		` + filterSQL + `
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?`
	args := []interface{}{userID, userID, feedID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get unread articles by feed: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

func (s *PostgresStore) GetUnscoredArticlesForUser(userID int64, limit int) ([]Article, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND (rs.article_id IS NULL OR rs.ai_scored = FALSE)
		ORDER BY a.published_date DESC
		LIMIT ?`, userID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("get unscored articles for user: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

func (s *PostgresStore) GetUnscoredArticleCount(userID int64) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*)
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND (rs.article_id IS NULL OR rs.ai_scored = FALSE)`,
		userID, userID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get unscored article count: %w", err)
	}
	return count, nil
}

func (s *PostgresStore) GetUnsummarizedArticleCount(userID int64) (int, error) {
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

func (s *PostgresStore) GetArticlesNeedingFullText(limit int) ([]Article, error) {
	rows, err := s.db.Query(`
		SELECT id, feed_id, guid, title, url, COALESCE(content,''), COALESCE(summary,''),
		       COALESCE(author,''), published_date, fetched_date
		FROM articles
		WHERE full_text_fetched = FALSE
		ORDER BY fetched_date DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("get articles needing full text: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

func (s *PostgresStore) UpdateArticleContent(articleID int64, content string) error {
	_, err := s.db.Exec(`UPDATE articles SET content = ? WHERE id = ?`, content, articleID)
	return err
}

func (s *PostgresStore) UpdateArticleLinkedContent(articleID int64, linkedURL, linkedContent string) error {
	_, err := s.db.Exec(
		`UPDATE articles SET linked_url = ?, linked_content = ? WHERE id = ?`,
		linkedURL, linkedContent, articleID,
	)
	return err
}

func (s *PostgresStore) MarkArticleFullTextFetched(articleID int64) error {
	_, err := s.db.Exec(`UPDATE articles SET full_text_fetched = TRUE WHERE id = ?`, articleID)
	return err
}

func (s *PostgresStore) GetStarredArticles(userID int64, limit, offset int, filterThreshold *int) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClausePG(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND rs.starred = TRUE
		` + filterSQL + `
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?`
	args := []interface{}{userID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get starred articles: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

// --- Article images ---

func (s *PostgresStore) StoreArticleImage(articleID int64, originalURL string, data []byte, mimeType string, width, height int) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO article_images (article_id, original_url, data, mime_type, width, height)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(article_id, original_url) DO UPDATE SET
		   data = excluded.data, mime_type = excluded.mime_type,
		   width = excluded.width, height = excluded.height,
		   fetched_at = NOW()
		 RETURNING id`,
		articleID, originalURL, data, mimeType, width, height,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *PostgresStore) GetArticleImage(imageID int64) (*ArticleImage, error) {
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

func (s *PostgresStore) GetArticleImageMap(articleID int64) (map[string]int64, error) {
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

func (s *PostgresStore) GetArticlesNeedingImageCache(limit int) ([]Article, error) {
	rows, err := s.db.Query(`
		SELECT id, feed_id, guid, title, url, COALESCE(content,''), COALESCE(summary,''),
		       COALESCE(author,''), published_date, fetched_date
		FROM articles
		WHERE images_cached = FALSE
		ORDER BY fetched_date DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("get articles needing image cache: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

func (s *PostgresStore) MarkArticleImagesCached(articleID int64) error {
	_, err := s.db.Exec(`UPDATE articles SET images_cached = TRUE WHERE id = ?`, articleID)
	return err
}

// --- Article metadata ---

func (s *PostgresStore) StoreArticleAuthors(articleID int64, authors []ArticleAuthor) error {
	for _, a := range authors {
		_, err := s.db.Exec(
			"INSERT INTO article_authors (article_id, name, email) VALUES (?, ?, ?) ON CONFLICT DO NOTHING",
			articleID, a.Name, a.Email,
		)
		if err != nil {
			return fmt.Errorf("store article author: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) StoreArticleCategories(articleID int64, categories []string) error {
	for _, cat := range categories {
		_, err := s.db.Exec(
			"INSERT INTO article_categories (article_id, category) VALUES (?, ?) ON CONFLICT DO NOTHING",
			articleID, cat,
		)
		if err != nil {
			return fmt.Errorf("store article category: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) GetArticleAuthors(articleID int64) ([]ArticleAuthor, error) {
	rows, err := s.db.Query(
		"SELECT name, email FROM article_authors WHERE article_id = ? ORDER BY name", articleID,
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

func (s *PostgresStore) GetArticleCategories(articleID int64) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT category FROM article_categories WHERE article_id = ? ORDER BY category", articleID,
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

func (s *PostgresStore) GetFeedAuthors(feedID int64) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT aa.name FROM article_authors aa
		 JOIN articles a ON a.id = aa.article_id
		 WHERE a.feed_id = ? ORDER BY aa.name`, feedID,
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

func (s *PostgresStore) GetFeedCategories(feedID int64) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT ac.category FROM article_categories ac
		 JOIN articles a ON a.id = ac.article_id
		 WHERE a.feed_id = ? ORDER BY ac.category`, feedID,
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

// --- Filter rules ---

func (s *PostgresStore) AddFilterRule(rule *FilterRule) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO filter_rules (user_id, feed_id, axis, value, score)
		 VALUES (?, ?, ?, ?, ?) RETURNING id`,
		rule.UserID, rule.FeedID, rule.Axis, rule.Value, rule.Score,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("add filter rule: %w", err)
	}
	return id, nil
}

func (s *PostgresStore) GetFilterRules(userID int64, feedID *int64) ([]FilterRule, error) {
	var query string
	var args []interface{}
	if feedID != nil {
		query = `SELECT id, user_id, feed_id, axis, value, score, created_at
				 FROM filter_rules WHERE user_id = ? AND (feed_id IS NULL OR feed_id = ?)
				 ORDER BY axis, value`
		args = []interface{}{userID, *feedID}
	} else {
		query = `SELECT id, user_id, feed_id, axis, value, score, created_at
				 FROM filter_rules WHERE user_id = ? ORDER BY axis, value`
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

func (s *PostgresStore) UpdateFilterRuleScore(ruleID int64, score int) error {
	_, err := s.db.Exec("UPDATE filter_rules SET score = ? WHERE id = ?", score, ruleID)
	if err != nil {
		return fmt.Errorf("update filter rule score: %w", err)
	}
	return nil
}

func (s *PostgresStore) DeleteFilterRule(ruleID int64) error {
	_, err := s.db.Exec("DELETE FROM filter_rules WHERE id = ?", ruleID)
	if err != nil {
		return fmt.Errorf("delete filter rule: %w", err)
	}
	return nil
}

func (s *PostgresStore) HasFilterRules(userID int64) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM filter_rules WHERE user_id = ?", userID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("has filter rules: %w", err)
	}
	return count > 0, nil
}

// --- Article summaries ---

func (s *PostgresStore) UpdateArticleAISummary(userID, articleID int64, aiSummary string) error {
	_, err := s.db.Exec(
		`INSERT INTO article_summaries (user_id, article_id, ai_summary)
		 VALUES (?, ?, ?)
		 ON CONFLICT(user_id, article_id) DO UPDATE SET
		   ai_summary = excluded.ai_summary,
		   generated_at = NOW()`,
		userID, articleID, aiSummary,
	)
	if err != nil {
		return fmt.Errorf("failed to update AI summary: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetArticleSummary(userID, articleID int64) (*ArticleSummary, error) {
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

// --- Feed stats ---

func (s *PostgresStore) GetFeedStats(userID int64) ([]FeedStats, error) {
	rows, err := s.db.Query(`
		SELECT f.id, f.title,
			COUNT(a.id),
			COUNT(a.id) - COALESCE(SUM(CASE WHEN rs.read IS TRUE THEN 1 ELSE 0 END), 0),
			COUNT(a.id) - COUNT(asumm.article_id),
			MAX(a.published_date)
		FROM feeds f
		JOIN user_feeds uf ON uf.feed_id = f.id AND uf.user_id = ?
		JOIN articles a ON a.feed_id = f.id
		LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = ?
		LEFT JOIN article_summaries asumm ON asumm.article_id = a.id AND asumm.user_id = ?
		GROUP BY f.id, f.title
		ORDER BY f.title`,
		userID, userID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("get feed stats: %w", err)
	}
	defer rows.Close()

	var stats []FeedStats
	for rows.Next() {
		var fs FeedStats
		if err := rows.Scan(&fs.FeedID, &fs.FeedTitle, &fs.TotalArticles, &fs.UnreadArticles, &fs.UnsummarizedArticles, &fs.LastPostDate); err != nil {
			return nil, fmt.Errorf("scan feed stats: %w", err)
		}
		stats = append(stats, fs)
	}
	return stats, rows.Err()
}

// --- Article groups ---

func (s *PostgresStore) CreateArticleGroup(userID int64, topic string) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		"INSERT INTO article_groups (user_id, topic) VALUES (?, ?) RETURNING id",
		userID, topic,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("failed to create article group: %w", err)
	}
	return id, nil
}

func (s *PostgresStore) AddArticleToGroup(groupID, articleID int64) error {
	_, err := s.db.Exec(
		"INSERT INTO article_group_members (group_id, article_id) VALUES (?, ?) ON CONFLICT DO NOTHING",
		groupID, articleID,
	)
	if err != nil {
		return fmt.Errorf("failed to add article to group: %w", err)
	}
	_, err = s.db.Exec("UPDATE article_groups SET updated_at = NOW() WHERE id = ?", groupID)
	return err
}

func (s *PostgresStore) GetGroupArticles(groupID int64) ([]Article, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date
		FROM articles a
		JOIN article_group_members agm ON a.id = agm.article_id
		WHERE agm.group_id = ?
		ORDER BY a.published_date DESC`, groupID)
	if err != nil {
		return nil, fmt.Errorf("failed to get group articles: %w", err)
	}
	defer rows.Close()
	return scanArticles(rows)
}

func (s *PostgresStore) UpdateGroupSummary(groupID int64, summary string, articleCount int, maxInterestScore *float64) error {
	_, err := s.db.Exec(
		`INSERT INTO group_summaries (group_id, summary, article_count, max_interest_score, generated_at)
		 VALUES (?, ?, ?, ?, NOW())
		 ON CONFLICT(group_id) DO UPDATE SET
		   summary = excluded.summary,
		   article_count = excluded.article_count,
		   max_interest_score = excluded.max_interest_score,
		   generated_at = NOW()`,
		groupID, summary, articleCount, maxInterestScore,
	)
	if err != nil {
		return fmt.Errorf("failed to update group summary: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetGroupSummary(groupID int64) (*GroupSummary, error) {
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

func (s *PostgresStore) GetUserGroups(userID int64) ([]ArticleGroup, error) {
	rows, err := s.db.Query(`
		SELECT ag.id, ag.user_id, ag.topic, ag.created_at, ag.updated_at
		FROM article_groups ag
		WHERE ag.user_id = ?
		  AND (SELECT COUNT(*) FROM article_group_members WHERE group_id = ag.id) >= 2
		ORDER BY ag.updated_at DESC`, userID)
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

func (s *PostgresStore) GetGroup(groupID int64) (*ArticleGroup, error) {
	var g ArticleGroup
	err := s.db.QueryRow(
		"SELECT id, user_id, topic, created_at, updated_at FROM article_groups WHERE id = ?", groupID,
	).Scan(&g.ID, &g.UserID, &g.Topic, &g.CreatedAt, &g.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get group: %w", err)
	}
	return &g, nil
}

func (s *PostgresStore) FindArticleGroup(articleID, userID int64) (*int64, error) {
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

func (s *PostgresStore) UpdateGroupEmbedding(groupID int64, embedding []byte) error {
	_, err := s.db.Exec("UPDATE article_groups SET embedding = ? WHERE id = ?", embedding, groupID)
	if err != nil {
		return fmt.Errorf("update group embedding: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetGroupsWithEmbeddings(userID int64) ([]ArticleGroupWithEmbedding, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, topic, embedding, created_at, updated_at
		 FROM article_groups
		 WHERE user_id = ? AND embedding IS NOT NULL`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("get groups with embeddings: %w", err)
	}
	defer rows.Close()

	var groups []ArticleGroupWithEmbedding
	for rows.Next() {
		var g ArticleGroupWithEmbedding
		if err := rows.Scan(&g.ID, &g.UserID, &g.Topic, &g.Embedding, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan group with embedding: %w", err)
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (s *PostgresStore) GetGroupEmbedding(groupID int64) ([]byte, error) {
	var emb []byte
	err := s.db.QueryRow(
		"SELECT embedding FROM article_groups WHERE id = ?", groupID,
	).Scan(&emb)
	if err != nil {
		return nil, fmt.Errorf("get group embedding: %w", err)
	}
	return emb, nil
}

func (s *PostgresStore) GetGroupArticleCount(groupID int64) (int, error) {
	var count int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM article_group_members WHERE group_id = ?", groupID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get group article count: %w", err)
	}
	return count, nil
}

func (s *PostgresStore) UpdateGroupTopic(groupID int64, topic string) error {
	_, err := s.db.Exec("UPDATE article_groups SET topic = ? WHERE id = ?", topic, groupID)
	if err != nil {
		return fmt.Errorf("update group topic: %w", err)
	}
	return nil
}

// --- Feed favicons ---

func (s *PostgresStore) StoreFeedFavicon(feedID int64, data []byte, mimeType string) error {
	_, err := s.db.Exec(
		`INSERT INTO feed_favicons (feed_id, data, mime_type, fetched_at)
		 VALUES (?, ?, ?, NOW())
		 ON CONFLICT(feed_id) DO UPDATE SET
		   data = excluded.data, mime_type = excluded.mime_type, fetched_at = NOW()`,
		feedID, data, mimeType,
	)
	return err
}

func (s *PostgresStore) GetFeedFavicon(feedID int64) (*FeedFavicon, error) {
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

func (s *PostgresStore) GetAllFeedFavicons() ([]FeedFavicon, error) {
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

func (s *PostgresStore) GetSubscribedFeedsWithoutFavicons() ([]Feed, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT f.id, f.url, f.title, f.description,
		       f.last_fetched, f.last_error, f.etag, f.last_modified,
		       f.enabled, f.created_at, f.consecutive_errors, f.next_fetch_at, f.status
		FROM feeds f
		JOIN user_feeds uf ON f.id = uf.feed_id
		LEFT JOIN feed_favicons ff ON f.id = ff.feed_id
		WHERE ff.feed_id IS NULL
		ORDER BY f.id`)
	if err != nil {
		return nil, fmt.Errorf("get feeds without favicons: %w", err)
	}
	defer rows.Close()
	return scanFeeds(rows)
}

// --- Subscriptions ---

func (s *PostgresStore) SubscribeUserToFeed(userID, feedID int64) error {
	_, err := s.db.Exec(
		"INSERT INTO user_feeds (user_id, feed_id) VALUES (?, ?) ON CONFLICT DO NOTHING",
		userID, feedID,
	)
	if err != nil {
		return fmt.Errorf("failed to subscribe user to feed: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetUserFeeds(userID int64) ([]Feed, error) {
	rows, err := s.db.Query(`
		SELECT f.id, f.url, f.title, f.description, f.last_fetched, f.last_error, f.etag,
		       f.last_modified, f.enabled, f.created_at,
		       f.consecutive_errors, f.next_fetch_at, f.status
		FROM feeds f
		JOIN user_feeds uf ON f.id = uf.feed_id
		WHERE uf.user_id = ? AND f.enabled = TRUE
		ORDER BY f.title`, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user feeds: %w", err)
	}
	defer rows.Close()
	return scanFeeds(rows)
}

func (s *PostgresStore) GetAllSubscribedFeeds() ([]Feed, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT f.id, f.url, f.title, f.description, f.last_fetched, f.last_error,
		       f.etag, f.last_modified, f.enabled, f.created_at,
		       f.consecutive_errors, f.next_fetch_at, f.status
		FROM feeds f
		JOIN user_feeds uf ON f.id = uf.feed_id
		WHERE f.enabled = TRUE AND f.status = 'active'
		  AND (f.next_fetch_at IS NULL OR f.next_fetch_at <= NOW())
		ORDER BY f.title`)
	if err != nil {
		return nil, fmt.Errorf("failed to get subscribed feeds: %w", err)
	}
	defer rows.Close()
	return scanFeeds(rows)
}

func (s *PostgresStore) GetAllActiveSubscribedFeeds() ([]Feed, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT f.id, f.url, f.title, f.description, f.last_fetched, f.last_error,
		       f.etag, f.last_modified, f.enabled, f.created_at,
		       f.consecutive_errors, f.next_fetch_at, f.status
		FROM feeds f
		JOIN user_feeds uf ON f.id = uf.feed_id
		WHERE f.enabled = TRUE
		ORDER BY f.title`)
	if err != nil {
		return nil, fmt.Errorf("failed to get all active subscribed feeds: %w", err)
	}
	defer rows.Close()
	return scanFeeds(rows)
}

func (s *PostgresStore) GetFeedSubscribers(feedID int64) ([]int64, error) {
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

func (s *PostgresStore) UnsubscribeUserFromFeed(userID, feedID int64) error {
	_, err := s.db.Exec(
		"DELETE FROM user_feeds WHERE user_id = ? AND feed_id = ?", userID, feedID,
	)
	if err != nil {
		return fmt.Errorf("failed to unsubscribe user from feed: %w", err)
	}
	return nil
}

func (s *PostgresStore) DeleteFeedIfOrphaned(feedID int64) (bool, error) {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM user_feeds WHERE feed_id = ?", feedID).Scan(&n); err != nil {
		return false, fmt.Errorf("failed to check subscribers: %w", err)
	}
	if n > 0 {
		return false, nil
	}

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

func (s *PostgresStore) GetAllSubscribingUsers() ([]int64, error) {
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

// --- Admin stats ---

func (s *PostgresStore) GetDBStats() (DBStats, error) {
	var stats DBStats

	rows, err := s.db.Query(`
		SELECT
			f.id, f.title, f.url, f.status,
			COUNT(DISTINCT a.id)       AS articles,
			COUNT(DISTINCT uf.user_id) AS subscribers
		FROM feeds f
		LEFT JOIN articles   a  ON a.feed_id  = f.id
		LEFT JOIN user_feeds uf ON uf.feed_id = f.id
		GROUP BY f.id
		ORDER BY articles DESC
	`)
	if err != nil {
		return stats, fmt.Errorf("failed to query feed stats: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var fs FeedStat
		if err := rows.Scan(&fs.ID, &fs.Title, &fs.URL, &fs.Status, &fs.Articles, &fs.Subscribers); err != nil {
			return stats, fmt.Errorf("failed to scan feed stat: %w", err)
		}
		stats.Feeds = append(stats.Feeds, fs)
		stats.TotalArticles += fs.Articles
		stats.TotalFeeds++
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}

	if err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&stats.TotalUsers); err != nil {
		return stats, fmt.Errorf("failed to count users: %w", err)
	}
	return stats, nil
}

// --- Fever API ---

func (s *PostgresStore) SetFeverCredential(userID int64, apiKey string) error {
	_, err := s.db.Exec(`
		INSERT INTO fever_credentials (user_id, api_key) VALUES (?, ?)
		ON CONFLICT(user_id) DO UPDATE SET api_key = excluded.api_key`,
		userID, apiKey)
	return err
}

func (s *PostgresStore) GetUserByFeverAPIKey(apiKey string) (*User, error) {
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

func (s *PostgresStore) GetFeverAPIKey(userID int64) (string, error) {
	var apiKey string
	err := s.db.QueryRow(`SELECT api_key FROM fever_credentials WHERE user_id = ?`, userID).Scan(&apiKey)
	return apiKey, err
}

func (s *PostgresStore) DeleteFeverCredential(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM fever_credentials WHERE user_id = ?`, userID)
	return err
}

func (s *PostgresStore) GetFeverItems(userID int64, sinceID, maxID int64, withIDs []int64, limit int) ([]FeverItemRow, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}

	args := []any{userID, userID}
	q := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(rs.read, FALSE)    AS is_read,
		       COALESCE(rs.starred, FALSE) AS is_starred
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

func (s *PostgresStore) GetFeverItemCount(userID int64) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*)
		FROM articles a
		JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = ?`, userID).Scan(&count)
	return count, err
}

func (s *PostgresStore) GetUnreadArticleIDsForUser(userID int64) ([]int64, error) {
	rows, err := s.db.Query(`
		SELECT a.id
		FROM articles a
		JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = ?
		LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = ?
		WHERE NOT COALESCE(rs.read, FALSE)
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

func (s *PostgresStore) GetStarredArticleIDsForUser(userID int64) ([]int64, error) {
	rows, err := s.db.Query(`
		SELECT article_id FROM read_state
		WHERE user_id = ? AND starred = TRUE
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

func (s *PostgresStore) MarkFeedArticlesRead(userID, feedID int64, before int64) error {
	var beforeCond string
	args := []any{userID, feedID}
	if before > 0 {
		beforeCond = `AND (a.published_date IS NULL OR a.published_date <= ?)`
		args = append(args, time.Unix(before, 0))
	}
	_, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO read_state (user_id, article_id, read, read_date)
		SELECT ?, a.id, TRUE, NOW()
		FROM articles a
		WHERE a.feed_id = ? %s
		ON CONFLICT(user_id, article_id) DO UPDATE SET read = TRUE, read_date = NOW()`,
		beforeCond), args...)
	return err
}

func (s *PostgresStore) MarkGroupArticlesRead(userID, groupID int64, before int64) error {
	var beforeCond string
	args := []any{userID, groupID}
	if before > 0 {
		beforeCond = `AND (a.published_date IS NULL OR a.published_date <= ?)`
		args = append(args, time.Unix(before, 0))
	}
	_, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO read_state (user_id, article_id, read, read_date)
		SELECT ?, a.id, TRUE, NOW()
		FROM articles a
		JOIN article_group_members agm ON agm.article_id = a.id
		WHERE agm.group_id = ? %s
		ON CONFLICT(user_id, article_id) DO UPDATE SET read = TRUE, read_date = NOW()`,
		beforeCond), args...)
	return err
}

func (s *PostgresStore) MarkAllArticlesRead(userID int64, before int64) error {
	var beforeCond string
	args := []any{userID, userID}
	if before > 0 {
		beforeCond = `AND (a.published_date IS NULL OR a.published_date <= ?)`
		args = append(args, time.Unix(before, 0))
	}
	_, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO read_state (user_id, article_id, read, read_date)
		SELECT ?, a.id, TRUE, NOW()
		FROM articles a
		JOIN user_feeds uf ON uf.feed_id = a.feed_id AND uf.user_id = ?
		WHERE 1=1 %s
		ON CONFLICT(user_id, article_id) DO UPDATE SET read = TRUE, read_date = NOW()`,
		beforeCond), args...)
	return err
}

func (s *PostgresStore) GetFeverLinks(userID int64) ([]FeverLink, error) {
	rows, err := s.db.Query(`
		SELECT
			ag.id,
			agm.article_id,
			a.feed_id,
			a.title,
			a.url,
			CASE WHEN COALESCE(rs.starred, FALSE) THEN 1 ELSE 0 END,
			COALESCE(gs.max_interest_score, 0)
		FROM article_groups ag
		JOIN article_group_members agm ON agm.group_id = ag.id
		JOIN articles a ON a.id = agm.article_id
		LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = ?
		LEFT JOIN group_summaries gs ON gs.group_id = ag.id
		WHERE ag.user_id = ?
		ORDER BY ag.updated_at DESC, agm.added_at ASC`,
		userID, userID)
	if err != nil {
		return nil, fmt.Errorf("fever links: %w", err)
	}
	defer rows.Close()

	type memberRow struct {
		articleID int64
		feedID    int64
		title     string
		url       string
		isSaved   int
		score     float64
	}

	var order []int64
	byGroup := map[int64][]memberRow{}

	for rows.Next() {
		var groupID int64
		var m memberRow
		if err := rows.Scan(&groupID, &m.articleID, &m.feedID, &m.title, &m.url, &m.isSaved, &m.score); err != nil {
			return nil, fmt.Errorf("fever links scan: %w", err)
		}
		if _, seen := byGroup[groupID]; !seen {
			order = append(order, groupID)
		}
		byGroup[groupID] = append(byGroup[groupID], m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	links := make([]FeverLink, 0, len(order))
	for _, groupID := range order {
		members := byGroup[groupID]
		if len(members) < 2 {
			continue
		}
		primary := members[0]

		ids := make([]string, len(members))
		for i, m := range members {
			ids[i] = fmt.Sprintf("%d", m.articleID)
		}

		temp := 0
		if primary.score > 0 {
			temp = int(primary.score * 10)
		} else {
			temp = len(members) * 25
		}
		if temp > 100 {
			temp = 100
		}

		links = append(links, FeverLink{
			GroupID:     groupID,
			FeedID:      primary.feedID,
			ItemID:      primary.articleID,
			IsSaved:     primary.isSaved,
			Temperature: temp,
			Title:       primary.title,
			URL:         primary.url,
			ItemIDs:     strings.Join(ids, ","),
		})
	}
	return links, nil
}

func (s *PostgresStore) GetFeedGroupMemberships(userID int64) (map[int64][]int64, error) {
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

// --- Internal scan helpers ---

// scanArticles scans a standard 10-column article result set.
func scanArticles(rows *sql.Rows) ([]Article, error) {
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

// filterScoreClausePG is identical in logic to filterScoreClause but named
// distinctly to avoid a duplicate-declaration collision with the SQLite version.
func filterScoreClausePG(userID int64, threshold *int) (string, []interface{}) {
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
