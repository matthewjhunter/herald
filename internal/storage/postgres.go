package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/matthewjhunter/herald/internal/storage/db"
)

// PostgresStore implements the Store interface using PostgreSQL.
//
// The query layer is mid-migration from hand-written database/sql (via the
// legacy *tracedDB) to sqlc-generated code on a pgx pool (#185). Both handles
// are live during the port: methods move from db to pool/q one file at a time,
// suite green at each step, and the legacy handle is removed once every query
// is ported.
type PostgresStore struct {
	db   *tracedDB     // legacy database/sql handle for not-yet-ported queries
	pool *pgxpool.Pool // pgx pool backing the sqlc query layer
	q    *db.Queries   // sqlc-generated queries, bound to pool
}

// Compile-time check that PostgresStore implements Store.
var _ Store = (*PostgresStore)(nil)

// Connection pool limits. Go's database/sql defaults (unlimited MaxOpen,
// MaxIdle=2) churn connections under concurrent load: idle conns over 2 are
// destroyed after each use, and the resulting TIME_WAIT sockets can exhaust
// ephemeral ports during a failure storm. Holding more idle conns warm
// eliminates the churn.
const (
	pgMaxOpenConns    = 25
	pgMaxIdleConns    = 25
	pgConnMaxLifetime = 30 * time.Minute
	pgConnMaxIdleTime = 5 * time.Minute
)

// NewPostgresStore opens a PostgreSQL connection, verifies it, and initializes
// the schema (all DDL statements are idempotent).
func NewPostgresStore(dsn string) (*PostgresStore, error) {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open postgres: %w", err)
	}
	sqlDB.SetMaxOpenConns(pgMaxOpenConns)
	sqlDB.SetMaxIdleConns(pgMaxIdleConns)
	sqlDB.SetConnMaxLifetime(pgConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(pgConnMaxIdleTime)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("failed to connect to postgres: %w", err)
	}
	// goose runs against the database/sql handle (goose has no pgx-pool API).
	if err := runMigrations(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}

	// The pgx pool backs the sqlc query layer; goose has already brought the
	// schema current above, so the pool only serves application queries.
	pool, err := newPgxPool(context.Background(), dsn)
	if err != nil {
		sqlDB.Close()
		return nil, err
	}

	return &PostgresStore{
		db:   &tracedDB{DB: sqlDB, useRebind: true},
		pool: pool,
		q:    db.New(pool),
	}, nil
}

// Close releases both query handles. They are independent connection pools to
// the same database during the sqlc migration (#185).
func (s *PostgresStore) Close() error {
	s.pool.Close()
	return s.db.Close()
}

// --- Internal helpers ---

// computeFeedBaseInterval queries the last 11 article publish dates for feedID
// and returns a fetch interval based on posting recency and frequency.
func (s *PostgresStore) computeFeedBaseInterval(feedID int64) time.Duration {
	rows, err := s.q.GetFeedRecentPublishDates(context.Background(), feedID)
	if err != nil {
		return 24 * time.Hour
	}
	dates := make([]time.Time, 0, len(rows))
	for _, t := range rows {
		if t != nil {
			dates = append(dates, *t)
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
		slices.Sort(gaps)
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

func userFromRow(r db.User) User {
	return User{
		ID:        r.ID,
		Name:      r.Name,
		OIDCSub:   r.OidcSub,
		Email:     r.Email,
		CreatedAt: r.CreatedAt,
	}
}

func (s *PostgresStore) CreateUser(name string) (int64, error) {
	id, err := s.q.CreateUser(context.Background(), name)
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

func (s *PostgresStore) GetUserByName(name string) (*User, error) {
	r, err := s.q.GetUserByName(context.Background(), name)
	if err != nil {
		return nil, mapErr(err)
	}
	u := userFromRow(r)
	return &u, nil
}

func (s *PostgresStore) GetUserByOIDCSub(sub string) (*User, error) {
	r, err := s.q.GetUserByOIDCSub(context.Background(), &sub)
	if err != nil {
		return nil, mapErr(err)
	}
	u := userFromRow(r)
	return &u, nil
}

func (s *PostgresStore) CreateUserWithOIDC(name, email, sub string) (*User, error) {
	var emailVal *string
	if email != "" {
		emailVal = &email
	}
	r, err := s.q.CreateUserWithOIDC(context.Background(), db.CreateUserWithOIDCParams{
		Name:    name,
		OidcSub: &sub,
		Email:   emailVal,
	})
	if err != nil {
		return nil, fmt.Errorf("create OIDC user: %w", err)
	}
	u := userFromRow(r)
	return &u, nil
}

func (s *PostgresStore) UpdateUserOIDCEmail(id int64, email string) error {
	return s.q.UpdateUserOIDCEmail(context.Background(), db.UpdateUserOIDCEmailParams{
		Email: &email,
		ID:    id,
	})
}

func (s *PostgresStore) ListUsers() ([]User, error) {
	rows, err := s.q.ListUsers(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	users := make([]User, len(rows))
	for i, r := range rows {
		users[i] = userFromRow(r)
	}
	return users, nil
}

// DeleteUser removes a user and everything they own, atomically. Tables that
// lack a users FK are deleted explicitly; tables with ON DELETE CASCADE are
// handled automatically when the users row is removed.
func (s *PostgresStore) DeleteUser(userID int64) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete user: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit
	qtx := s.q.WithTx(tx)
	// Order matters only in that the explicit deletes precede DeleteUserRow,
	// which cascades fever_credentials, newsletters+issues, and ai_summaries.
	// DeleteUserArticleGroups cascades article_group_members + group_summaries.
	steps := []func(context.Context, int64) error{
		qtx.DeleteUserReadState,
		qtx.DeleteUserPreferences,
		qtx.DeleteUserFeeds,
		qtx.DeleteUserFeedTags,
		qtx.DeleteUserPrompts,
		qtx.DeleteUserFilterRules,
		qtx.DeleteUserArticleGroups,
		qtx.DeleteUserRow,
	}
	for _, step := range steps {
		if err := step(ctx, userID); err != nil {
			return fmt.Errorf("delete user: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// --- User prompts ---

func (s *PostgresStore) GetUserPrompt(userID int64, promptType string) (string, error) {
	tmpl, err := s.q.GetUserPrompt(context.Background(), db.GetUserPromptParams{
		UserID:     userID,
		PromptType: promptType,
	})
	return tmpl, mapErr(err)
}

func (s *PostgresStore) GetUserPromptTemperature(userID int64, promptType string) (float64, error) {
	temp, err := s.q.GetUserPromptTemperature(context.Background(), db.GetUserPromptTemperatureParams{
		UserID:     userID,
		PromptType: promptType,
	})
	if err != nil {
		return 0, mapErr(err)
	}
	if temp == nil {
		return 0, nil
	}
	return *temp, nil
}

func (s *PostgresStore) GetUserPromptModel(userID int64, promptType string) (string, error) {
	model, err := s.q.GetUserPromptModel(context.Background(), db.GetUserPromptModelParams{
		UserID:     userID,
		PromptType: promptType,
	})
	if err != nil {
		return "", mapErr(err)
	}
	return derefString(model), nil
}

func (s *PostgresStore) SetUserPrompt(userID int64, promptType, promptTemplate string, temperature *float64, model *string) error {
	return s.q.SetUserPrompt(context.Background(), db.SetUserPromptParams{
		UserID:         userID,
		PromptType:     promptType,
		PromptTemplate: promptTemplate,
		Temperature:    temperature,
		Model:          model,
	})
}

func (s *PostgresStore) DeleteUserPrompt(userID int64, promptType string) error {
	return s.q.DeleteUserPrompt(context.Background(), db.DeleteUserPromptParams{
		UserID:     userID,
		PromptType: promptType,
	})
}

func (s *PostgresStore) ListUserPrompts(userID int64) ([]UserPrompt, error) {
	rows, err := s.q.ListUserPrompts(context.Background(), userID)
	if err != nil {
		return nil, err
	}
	prompts := make([]UserPrompt, len(rows))
	for i, r := range rows {
		prompts[i] = UserPrompt{
			UserID:         userID,
			PromptType:     r.PromptType,
			PromptTemplate: r.PromptTemplate,
			Temperature:    r.Temperature,
			Model:          derefString(r.Model),
			CreatedAt:      r.CreatedAt,
			UpdatedAt:      r.UpdatedAt,
		}
	}
	return prompts, nil
}

// --- User preferences ---

func (s *PostgresStore) GetUserPreference(userID int64, key string) (string, error) {
	value, err := s.q.GetUserPreference(context.Background(), db.GetUserPreferenceParams{
		UserID: userID,
		Key:    key,
	})
	return value, mapErr(err)
}

func (s *PostgresStore) SetUserPreference(userID int64, key, value string) error {
	return s.q.SetUserPreference(context.Background(), db.SetUserPreferenceParams{
		UserID: userID,
		Key:    key,
		Value:  value,
	})
}

func (s *PostgresStore) GetAllUserPreferences(userID int64) (map[string]string, error) {
	rows, err := s.q.GetAllUserPreferences(context.Background(), userID)
	if err != nil {
		return nil, fmt.Errorf("get user preferences: %w", err)
	}
	prefs := make(map[string]string, len(rows))
	for _, r := range rows {
		prefs[r.Key] = r.Value
	}
	return prefs, nil
}

func (s *PostgresStore) DeleteUserPreference(userID int64, key string) error {
	return s.q.DeleteUserPreference(context.Background(), db.DeleteUserPreferenceParams{
		UserID: userID,
		Key:    key,
	})
}

// --- Read state ---

// UserSubscribedToArticleFeed reports whether the user is subscribed to the
// feed that owns the article. Unknown article IDs return false, nil.
func (s *PostgresStore) UserSubscribedToArticleFeed(userID, articleID int64) (bool, error) {
	subscribed, err := s.q.UserSubscribedToArticleFeed(context.Background(), db.UserSubscribedToArticleFeedParams{
		UserID:    userID,
		ArticleID: articleID,
	})
	if err != nil {
		return false, fmt.Errorf("check article subscription: %w", err)
	}
	return subscribed, nil
}

func (s *PostgresStore) UpdateStarred(userID, articleID int64, starred bool) error {
	if err := s.q.UpdateStarred(context.Background(), db.UpdateStarredParams{
		UserID:    userID,
		ArticleID: articleID,
		Starred:   starred,
	}); err != nil {
		return fmt.Errorf("update starred: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateReadState(userID, articleID int64, read bool, interestScore, securityScore *float64, securityReason *string, securityFlagged *bool) error {
	var err error
	if interestScore != nil {
		// security_flagged is typed interface{} (COALESCE(?, FALSE) is untyped):
		// pass the bool value or nil, matching the hand-written behavior.
		var flagVal any
		if securityFlagged != nil {
			flagVal = *securityFlagged
		}
		err = s.q.UpsertReadStateScores(context.Background(), db.UpsertReadStateScoresParams{
			UserID:          userID,
			ArticleID:       articleID,
			InterestScore:   interestScore,
			SecurityScore:   securityScore,
			SecurityReason:  securityReason,
			SecurityFlagged: flagVal,
		})
	} else {
		err = s.q.UpsertReadStateRead(context.Background(), db.UpsertReadStateReadParams{
			UserID:    userID,
			ArticleID: articleID,
			Read:      read,
		})
	}
	if err != nil {
		return fmt.Errorf("failed to update read state: %w", err)
	}
	return nil
}

// GetScoreStats returns AI scoring breakdown per feed for a user.
func (s *PostgresStore) GetScoreStats(userID int64) (*ScoreStatsResult, error) {
	rows, err := s.q.GetScoreStats(context.Background(), userID)
	if err != nil {
		return nil, fmt.Errorf("get score stats: %w", err)
	}
	result := &ScoreStatsResult{}
	for _, r := range rows {
		fs := FeedScoreStats{
			FeedID:        r.FeedID,
			FeedTitle:     r.FeedTitle,
			TotalScored:   r.TotalScored,
			SecPass:       r.SecPass,
			SecBorderline: r.SecBorderline,
			SecFail:       r.SecFail,
			IntHigh:       r.IntHigh,
			IntMedium:     r.IntMedium,
			IntLow:        r.IntLow,
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
	return result, nil
}

// IncrementAIRetries bumps the retry counter for an article that failed AI processing.
// Creates a read_state row if one doesn't exist yet.
func (s *PostgresStore) IncrementAIRetries(userID, articleID int64) error {
	if err := s.q.IncrementAIRetries(context.Background(), db.IncrementAIRetriesParams{
		UserID:    userID,
		ArticleID: articleID,
	}); err != nil {
		return fmt.Errorf("increment ai retries: %w", err)
	}
	return nil
}

// ResetScores clears the security verdict on the user's subscribed articles so
// the pipeline re-screens (and re-curates) them. The verdict is article-level
// now (#141): this resets the shared article rows for the user's feeds, plus the
// user's own interest scores. If securityOnly, only articles below belowScore are
// reset. Returns the number of article rows reset.
func (s *PostgresStore) ResetScores(userID int64, securityOnly bool, belowScore float64) (int64, error) {
	ctx := context.Background()
	if securityOnly {
		n, err := s.q.ResetArticleScoresBelow(ctx, db.ResetArticleScoresBelowParams{
			UserID:     userID,
			BelowScore: belowScore,
		})
		if err != nil {
			return 0, fmt.Errorf("reset scores: %w", err)
		}
		return n, nil
	}
	if err := s.q.ResetReadStateScores(ctx, userID); err != nil {
		return 0, fmt.Errorf("reset scores (read_state): %w", err)
	}
	n, err := s.q.ResetArticleScores(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("reset scores: %w", err)
	}
	return n, nil
}

// --- Feeds ---

func feedFromRow(r db.Feed) Feed {
	return Feed{
		ID:                r.ID,
		URL:               r.Url,
		Title:             r.Title,
		Description:       derefString(r.Description),
		SiteURL:           r.SiteUrl,
		LastFetched:       r.LastFetched,
		LastError:         r.LastError,
		ETag:              derefString(r.Etag),
		LastModified:      derefString(r.LastModified),
		Enabled:           r.Enabled,
		CreatedAt:         r.CreatedAt,
		ConsecutiveErrors: int(r.ConsecutiveErrors),
		NextFetchAt:       r.NextFetchAt,
		Status:            r.Status,
	}
}

func (s *PostgresStore) AddFeed(url, title, description string) (int64, error) {
	id, err := s.q.AddFeed(context.Background(), db.AddFeedParams{
		Url:         url,
		Title:       title,
		Description: description,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to add feed: %w", err)
	}
	return id, nil
}

// GetFeed returns the feed with the given ID, or an error if not found.
// Unlike GetAllFeeds, this returns the row regardless of enabled/status —
// callers using it for metadata lookup (e.g. embedding context) need the
// title even for disabled feeds.
func (s *PostgresStore) GetFeed(feedID int64) (*Feed, error) {
	r, err := s.q.GetFeed(context.Background(), feedID)
	if err != nil {
		return nil, fmt.Errorf("get feed %d: %w", feedID, mapErr(err))
	}
	f := feedFromRow(r)
	return &f, nil
}

func (s *PostgresStore) GetAllFeeds() ([]Feed, error) {
	rows, err := s.q.GetActiveFeedsToFetch(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get feeds: %w", err)
	}
	feeds := make([]Feed, len(rows))
	for i, r := range rows {
		feeds[i] = feedFromRow(r)
	}
	return feeds, nil
}

func (s *PostgresStore) UpdateFeedError(feedID int64, errMsg string) error {
	ctx := context.Background()
	if err := s.q.IncrementFeedError(ctx, db.IncrementFeedErrorParams{
		LastError: errMsg,
		ID:        feedID,
	}); err != nil {
		return fmt.Errorf("failed to update feed error: %w", err)
	}

	state, err := s.q.GetFeedErrorState(ctx, feedID)
	if err != nil {
		return nil
	}

	if state.ConsecutiveErrors >= 5 && (state.LastFetched == nil || time.Since(*state.LastFetched) > 30*24*time.Hour) {
		s.q.MarkFeedDead(ctx, feedID) //nolint:errcheck
		return nil
	}

	base := s.computeFeedBaseInterval(feedID)
	next := time.Now().Add(applyErrorBackoff(base, int(state.ConsecutiveErrors)))
	s.q.SetFeedNextFetch(ctx, db.SetFeedNextFetchParams{NextFetchAt: &next, ID: feedID}) //nolint:errcheck
	return nil
}

func (s *PostgresStore) ClearFeedError(feedID int64) error {
	return s.UpdateFeedLastFetched(feedID)
}

func (s *PostgresStore) MarkFeedFetched(feedID int64) error {
	if err := s.q.MarkFeedFetched(context.Background(), feedID); err != nil {
		return fmt.Errorf("failed to mark feed fetched: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateFeedCacheHeaders(feedID int64, etag, lastModified string) error {
	if err := s.q.UpdateFeedCacheHeaders(context.Background(), db.UpdateFeedCacheHeadersParams{
		Etag:         etag,
		LastModified: lastModified,
		ID:           feedID,
	}); err != nil {
		return fmt.Errorf("failed to update feed cache headers: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateFeedLastFetched(feedID int64) error {
	base := s.computeFeedBaseInterval(feedID)
	next := time.Now().Add(base)
	if err := s.q.MarkFeedFetchedWithNext(context.Background(), db.MarkFeedFetchedWithNextParams{
		NextFetchAt: &next,
		ID:          feedID,
	}); err != nil {
		return fmt.Errorf("failed to update feed last_fetched: %w", err)
	}
	return nil
}

func (s *PostgresStore) RenameFeed(feedID int64, title string) error {
	if err := s.q.RenameFeed(context.Background(), db.RenameFeedParams{Title: title, ID: feedID}); err != nil {
		return fmt.Errorf("failed to rename feed: %w", err)
	}
	return nil
}

func (s *PostgresStore) RenameUserFeed(userID, feedID int64, title string) error {
	var userTitle *string
	if title != "" {
		userTitle = &title
	}
	if err := s.q.RenameUserFeed(context.Background(), db.RenameUserFeedParams{
		UserTitle: userTitle,
		UserID:    userID,
		FeedID:    feedID,
	}); err != nil {
		return fmt.Errorf("failed to rename user feed: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateFeedSiteURL(feedID int64, siteURL string) error {
	if err := s.q.UpdateFeedSiteURL(context.Background(), db.UpdateFeedSiteURLParams{SiteUrl: siteURL, ID: feedID}); err != nil {
		return fmt.Errorf("update feed site url: %w", err)
	}
	return nil
}

// --- Articles ---

// coreArticle assembles an Article from the standard 10-column projection
// (id, feed_id, guid, title, url, content, summary, author, published_date,
// fetched_date) shared by the article fetch queries. The nullable text columns
// are dereferenced to "" to match the non-pointer Article fields.
func coreArticle(id, feedID int64, guid, title, url string, content, summary, author *string, published *time.Time, fetched time.Time) Article {
	return Article{
		ID:            id,
		FeedID:        feedID,
		GUID:          guid,
		Title:         title,
		URL:           url,
		Content:       derefString(content),
		Summary:       derefString(summary),
		Author:        derefString(author),
		PublishedDate: published,
		FetchedDate:   fetched,
	}
}

func (s *PostgresStore) FindDuplicateArticle(title string, publishedDate *time.Time) (int64, error) {
	if title == "" || publishedDate == nil {
		return 0, nil
	}
	id, err := s.q.FindDuplicateArticle(context.Background(), db.FindDuplicateArticleParams{
		Title:         title,
		PublishedDate: publishedDate,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func (s *PostgresStore) AddArticle(article *Article) (int64, error) {
	id, err := s.q.AddArticle(context.Background(), db.AddArticleParams{
		FeedID:        article.FeedID,
		Guid:          article.GUID,
		Title:         article.Title,
		Url:           article.URL,
		Content:       article.Content,
		Summary:       article.Summary,
		Author:        article.Author,
		PublishedDate: article.PublishedDate,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // duplicate
	}
	if err != nil {
		return 0, fmt.Errorf("failed to add article: %w", err)
	}
	return id, nil
}

func (s *PostgresStore) GetUnreadArticles(limit int) ([]Article, error) {
	rows, err := s.q.GetUnreadArticles(context.Background(), int32(limit))
	if err != nil {
		return nil, fmt.Errorf("failed to get unread articles: %w", err)
	}
	articles := make([]Article, len(rows))
	for i, r := range rows {
		articles[i] = coreArticle(r.ID, r.FeedID, r.Guid, r.Title, r.Url, r.Content, r.Summary, r.Author, r.PublishedDate, r.FetchedDate)
	}
	return articles, nil
}

func (s *PostgresStore) GetArticle(articleID int64) (*Article, error) {
	r, err := s.q.GetArticle(context.Background(), articleID)
	if err != nil {
		return nil, fmt.Errorf("get article %d: %w", articleID, mapErr(err))
	}
	a := coreArticle(r.ID, r.FeedID, r.Guid, r.Title, r.Url, r.Content, r.Summary, r.Author, r.PublishedDate, r.FetchedDate)
	a.LinkedURL = r.LinkedUrl
	a.LinkedContent = r.LinkedContent
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
	args := []any{userID, threshold}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.pool.Query(context.Background(), rebindNumeric(query), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get articles by interest score: %w", err)
	}
	defer rows.Close()

	var articles []Article
	var scores []float64
	for rows.Next() {
		var (
			id, feedID               int64
			guid, title, url         string
			content, summary, author *string
			published                *time.Time
			fetched                  time.Time
			score                    float64
		)
		if err := rows.Scan(&id, &feedID, &guid, &title, &url,
			&content, &summary, &author, &published, &fetched, &score); err != nil {
			return nil, nil, fmt.Errorf("failed to scan article: %w", err)
		}
		articles = append(articles, coreArticle(id, feedID, guid, title, url, content, summary, author, published, fetched))
		scores = append(scores, score)
	}
	return articles, scores, rows.Err()
}

func (s *PostgresStore) GetUnreadArticlesForUser(userID int64, limit, offset int, filterThreshold *int, includeRead bool) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClausePG(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(a.security_flagged, FALSE) AS security_flagged,
		       COALESCE(rs.read, FALSE) AS is_read, COALESCE(rs.starred, FALSE) AS is_starred
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ?` + readFilterClausePG(includeRead) + `
		AND NOT EXISTS (
			SELECT 1 FROM article_group_members agm
			JOIN article_groups ag ON agm.group_id = ag.id
			WHERE agm.article_id = a.id AND ag.user_id = ?
		)
		` + filterSQL + `
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?`
	args := []any{userID, userID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.pool.Query(context.Background(), rebindNumeric(query), args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get unread articles for user: %w", err)
	}
	return scanArticlesWithReadStatePgx(rows)
}

func (s *PostgresStore) GetUnreadArticlesByFeed(userID, feedID int64, limit, offset int, filterThreshold *int, includeRead bool) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClausePG(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(a.security_flagged, FALSE) AS security_flagged,
		       COALESCE(rs.read, FALSE) AS is_read, COALESCE(rs.starred, FALSE) AS is_starred
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND a.feed_id = ?` + readFilterClausePG(includeRead) + `
		AND NOT EXISTS (
			SELECT 1 FROM article_group_members agm
			JOIN article_groups ag ON agm.group_id = ag.id
			WHERE agm.article_id = a.id AND ag.user_id = ?
		)
		` + filterSQL + `
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?`
	args := []any{userID, userID, feedID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.pool.Query(context.Background(), rebindNumeric(query), args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get unread articles by feed: %w", err)
	}
	return scanArticlesWithReadStatePgx(rows)
}

// GetUnscoredArticleCount counts the user's articles still in the AI funnel:
// not yet security-screened (within budget) or screened-pass but not yet
// interest-scored for this user. See the SQLite implementation (#141).
func (s *PostgresStore) GetUnscoredArticleCount(userID int64) (int, error) {
	count, err := s.q.GetUnscoredArticleCount(context.Background(), userID)
	if err != nil {
		return 0, fmt.Errorf("get unscored article count: %w", err)
	}
	return count, nil
}

func (s *PostgresStore) GetUnsummarizedScoredArticles(securityThreshold float64, limit int) ([]Article, error) {
	rows, err := s.q.GetUnsummarizedScoredArticles(context.Background(), db.GetUnsummarizedScoredArticlesParams{
		SecurityThreshold: securityThreshold,
		Lim:               int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("get unsummarized scored articles: %w", err)
	}
	articles := make([]Article, len(rows))
	for i, r := range rows {
		articles[i] = coreArticle(r.ID, r.FeedID, r.Guid, r.Title, r.Url, r.Content, r.Summary, r.Author, r.PublishedDate, r.FetchedDate)
	}
	return articles, nil
}

// SetInterestScore records the curation interest score without touching the
// security verdict. See the SQLite implementation.
func (s *PostgresStore) SetInterestScore(userID, articleID int64, interestScore float64) error {
	if err := s.q.SetInterestScore(context.Background(), db.SetInterestScoreParams{
		UserID:        userID,
		ArticleID:     articleID,
		InterestScore: interestScore,
	}); err != nil {
		return fmt.Errorf("set interest score: %w", err)
	}
	return nil
}

// ScreenArticleSecurity records the security verdict on the article itself
// (#141). See the SQLite implementation.
func (s *PostgresStore) ScreenArticleSecurity(articleID int64, securityScore float64, securityReason string, securityFlagged bool) error {
	if err := s.q.ScreenArticleSecurity(context.Background(), db.ScreenArticleSecurityParams{
		SecurityScore:   securityScore,
		SecurityReason:  securityReason,
		SecurityFlagged: securityFlagged,
		ID:              articleID,
	}); err != nil {
		return fmt.Errorf("screen article security: %w", err)
	}
	return nil
}

// SkipArticleSecurity marks an article screened without a score (no content /
// too short). See the SQLite implementation.
func (s *PostgresStore) SkipArticleSecurity(articleID int64, reason string) error {
	if err := s.q.SkipArticleSecurity(context.Background(), db.SkipArticleSecurityParams{
		SecurityReason: reason,
		ID:             articleID,
	}); err != nil {
		return fmt.Errorf("skip article security: %w", err)
	}
	return nil
}

// IncrementArticleSecurityAttempts bumps the per-article security retry counter.
// See the SQLite implementation.
func (s *PostgresStore) IncrementArticleSecurityAttempts(articleID int64) error {
	if err := s.q.IncrementArticleSecurityAttempts(context.Background(), articleID); err != nil {
		return fmt.Errorf("increment article security attempts: %w", err)
	}
	return nil
}

// GetUnscreenedArticles returns articles not yet security-screened, within the
// retry budget, newest first. Global (not user-scoped). See the SQLite version.
func (s *PostgresStore) GetUnscreenedArticles(limit int) ([]Article, error) {
	rows, err := s.q.GetUnscreenedArticles(context.Background(), int32(limit))
	if err != nil {
		return nil, fmt.Errorf("get unscreened articles: %w", err)
	}
	articles := make([]Article, len(rows))
	for i, r := range rows {
		articles[i] = coreArticle(r.ID, r.FeedID, r.Guid, r.Title, r.Url, r.Content, r.Summary, r.Author, r.PublishedDate, r.FetchedDate)
	}
	return articles, nil
}

// GetUnscoredCurationArticles returns articles that passed the security screen
// but have not yet been interest-scored (interest_score IS NULL). Backfill input
// for the staged pipeline's curation stage. See the SQLite implementation.
func (s *PostgresStore) GetUnscoredCurationArticles(userID int64, securityThreshold float64, limit int) ([]Article, error) {
	rows, err := s.q.GetUnscoredCurationArticles(context.Background(), db.GetUnscoredCurationArticlesParams{
		UserID:            userID,
		SecurityThreshold: securityThreshold,
		Lim:               int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("get unscored curation articles: %w", err)
	}
	articles := make([]Article, len(rows))
	for i, r := range rows {
		articles[i] = coreArticle(r.ID, r.FeedID, r.Guid, r.Title, r.Url, r.Content, r.Summary, r.Author, r.PublishedDate, r.FetchedDate)
	}
	return articles, nil
}

// GetUngroupedEmbeddedArticles returns security-passed, embedded (status OK),
// still-ungrouped articles published/fetched since the cutoff. The cluster
// stage's recency window. See the SQLite implementation.
func (s *PostgresStore) GetUngroupedEmbeddedArticles(userID int64, model string, securityThreshold float64, since time.Time, limit int) ([]Article, error) {
	rows, err := s.q.GetUngroupedEmbeddedArticles(context.Background(), db.GetUngroupedEmbeddedArticlesParams{
		Model:             model,
		Status:            int16(EmbedStatusOK),
		UserID:            userID,
		SecurityThreshold: securityThreshold,
		Since:             since,
		Lim:               int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("get ungrouped embedded articles: %w", err)
	}
	articles := make([]Article, len(rows))
	for i, r := range rows {
		articles[i] = coreArticle(r.ID, r.FeedID, r.Guid, r.Title, r.Url, r.Content, r.Summary, r.Author, r.PublishedDate, r.FetchedDate)
	}
	return articles, nil
}

func (s *PostgresStore) GetUnsummarizedArticleCount() (int, error) {
	count, err := s.q.GetUnsummarizedArticleCount(context.Background())
	if err != nil {
		return 0, fmt.Errorf("get unsummarized article count: %w", err)
	}
	return count, nil
}

func (s *PostgresStore) GetArticlesNeedingFullText(limit int) ([]Article, error) {
	rows, err := s.q.GetArticlesNeedingFullText(context.Background(), int32(limit))
	if err != nil {
		return nil, fmt.Errorf("get articles needing full text: %w", err)
	}
	articles := make([]Article, len(rows))
	for i, r := range rows {
		articles[i] = coreArticle(r.ID, r.FeedID, r.Guid, r.Title, r.Url, r.Content, r.Summary, r.Author, r.PublishedDate, r.FetchedDate)
	}
	return articles, nil
}

func (s *PostgresStore) UpdateArticleContent(articleID int64, content string) error {
	return s.q.UpdateArticleContent(context.Background(), db.UpdateArticleContentParams{
		Content: content,
		ID:      articleID,
	})
}

func (s *PostgresStore) UpdateArticleLinkedContent(articleID int64, linkedURL, linkedContent string) error {
	return s.q.UpdateArticleLinkedContent(context.Background(), db.UpdateArticleLinkedContentParams{
		LinkedUrl:     linkedURL,
		LinkedContent: linkedContent,
		ID:            articleID,
	})
}

func (s *PostgresStore) MarkArticleFullTextFetched(articleID int64) error {
	return s.q.MarkArticleFullTextFetched(context.Background(), articleID)
}

func (s *PostgresStore) GetStarredArticles(userID int64, limit, offset int, filterThreshold *int) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClausePG(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(a.security_flagged, FALSE) AS security_flagged,
		       COALESCE(rs.read, FALSE) AS is_read, COALESCE(rs.starred, FALSE) AS is_starred
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE uf.user_id = ? AND rs.starred = TRUE
		` + filterSQL + `
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?`
	args := []any{userID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.pool.Query(context.Background(), rebindNumeric(query), args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get starred articles: %w", err)
	}
	return scanArticlesWithReadStatePgx(rows)
}

// --- Article images ---

func (s *PostgresStore) StoreArticleImage(articleID int64, originalURL string, data []byte, mimeType string, width, height int) (int64, error) {
	return s.q.StoreArticleImage(context.Background(), db.StoreArticleImageParams{
		ArticleID:   articleID,
		OriginalUrl: originalURL,
		Data:        data,
		MimeType:    mimeType,
		Width:       int64(width),
		Height:      int64(height),
	})
}

func (s *PostgresStore) GetArticleImage(imageID int64) (*ArticleImage, error) {
	r, err := s.q.GetArticleImage(context.Background(), imageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get article image: %w", err)
	}
	return &ArticleImage{
		ID:          r.ID,
		ArticleID:   r.ArticleID,
		OriginalURL: r.OriginalUrl,
		Data:        r.Data,
		MimeType:    r.MimeType,
		Width:       int(r.Width),
		Height:      int(r.Height),
		FetchedAt:   r.FetchedAt,
	}, nil
}

func (s *PostgresStore) GetArticleImageMap(articleID int64) (map[string]int64, error) {
	rows, err := s.q.GetArticleImageMap(context.Background(), articleID)
	if err != nil {
		return nil, fmt.Errorf("get article image map: %w", err)
	}
	m := make(map[string]int64, len(rows))
	for _, r := range rows {
		m[r.OriginalUrl] = r.ID
	}
	return m, nil
}

func (s *PostgresStore) GetArticlesNeedingImageCache(limit int) ([]Article, error) {
	rows, err := s.q.GetArticlesNeedingImageCache(context.Background(), int32(limit))
	if err != nil {
		return nil, fmt.Errorf("get articles needing image cache: %w", err)
	}
	out := make([]Article, len(rows))
	for i, r := range rows {
		out[i] = Article{
			ID:            r.ID,
			FeedID:        r.FeedID,
			GUID:          r.Guid,
			Title:         r.Title,
			URL:           r.Url,
			Content:       derefString(r.Content),
			Summary:       derefString(r.Summary),
			Author:        derefString(r.Author),
			PublishedDate: r.PublishedDate,
			FetchedDate:   r.FetchedDate,
		}
	}
	return out, nil
}

func (s *PostgresStore) MarkArticleImagesCached(articleID int64) error {
	return s.q.MarkArticleImagesCached(context.Background(), articleID)
}

// --- Article metadata ---

func (s *PostgresStore) StoreArticleAuthors(articleID int64, authors []ArticleAuthor) error {
	ctx := context.Background()
	for _, a := range authors {
		if err := s.q.InsertArticleAuthor(ctx, db.InsertArticleAuthorParams{
			ArticleID: articleID,
			Name:      a.Name,
			Email:     a.Email,
		}); err != nil {
			return fmt.Errorf("store article author: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) StoreArticleCategories(articleID int64, categories []string) error {
	ctx := context.Background()
	for _, cat := range categories {
		if err := s.q.InsertArticleCategory(ctx, db.InsertArticleCategoryParams{
			ArticleID: articleID,
			Category:  cat,
		}); err != nil {
			return fmt.Errorf("store article category: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) GetArticleAuthors(articleID int64) ([]ArticleAuthor, error) {
	rows, err := s.q.GetArticleAuthors(context.Background(), articleID)
	if err != nil {
		return nil, fmt.Errorf("get article authors: %w", err)
	}
	authors := make([]ArticleAuthor, len(rows))
	for i, r := range rows {
		authors[i] = ArticleAuthor{Name: r.Name, Email: derefString(r.Email)}
	}
	return authors, nil
}

func (s *PostgresStore) GetArticleCategories(articleID int64) ([]string, error) {
	cats, err := s.q.GetArticleCategories(context.Background(), articleID)
	if err != nil {
		return nil, fmt.Errorf("get article categories: %w", err)
	}
	return cats, nil
}

// --- Feed metadata discovery ---

func (s *PostgresStore) GetFeedAuthors(feedID int64) ([]string, error) {
	names, err := s.q.GetFeedAuthors(context.Background(), feedID)
	if err != nil {
		return nil, fmt.Errorf("get feed authors: %w", err)
	}
	return names, nil
}

func (s *PostgresStore) GetFeedCategories(feedID int64) ([]string, error) {
	cats, err := s.q.GetFeedCategories(context.Background(), feedID)
	if err != nil {
		return nil, fmt.Errorf("get feed categories: %w", err)
	}
	return cats, nil
}

// --- Filter rules ---

func (s *PostgresStore) AddFilterRule(rule *FilterRule) (int64, error) {
	id, err := s.q.AddFilterRule(context.Background(), db.AddFilterRuleParams{
		UserID: rule.UserID,
		FeedID: rule.FeedID,
		Axis:   rule.Axis,
		Value:  rule.Value,
		Score:  int64(rule.Score),
	})
	if err != nil {
		return 0, fmt.Errorf("add filter rule: %w", err)
	}
	return id, nil
}

func (s *PostgresStore) GetFilterRules(userID int64, feedID *int64) ([]FilterRule, error) {
	rows, err := s.q.GetFilterRules(context.Background(), db.GetFilterRulesParams{
		UserID: userID,
		FeedID: feedID,
	})
	if err != nil {
		return nil, fmt.Errorf("get filter rules: %w", err)
	}
	rules := make([]FilterRule, len(rows))
	for i, r := range rows {
		rules[i] = FilterRule{
			ID:        r.ID,
			UserID:    r.UserID,
			FeedID:    r.FeedID,
			Axis:      r.Axis,
			Value:     r.Value,
			Score:     int(r.Score),
			CreatedAt: r.CreatedAt,
		}
	}
	return rules, nil
}

func (s *PostgresStore) UpdateFilterRuleScore(userID, ruleID int64, score int) error {
	n, err := s.q.UpdateFilterRuleScore(context.Background(), db.UpdateFilterRuleScoreParams{
		Score:  int64(score),
		ID:     ruleID,
		UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("update filter rule score: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("filter rule %d not found for user %d", ruleID, userID)
	}
	return nil
}

func (s *PostgresStore) DeleteFilterRule(userID, ruleID int64) error {
	n, err := s.q.DeleteFilterRule(context.Background(), db.DeleteFilterRuleParams{
		ID:     ruleID,
		UserID: userID,
	})
	if err != nil {
		return fmt.Errorf("delete filter rule: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("filter rule %d not found for user %d", ruleID, userID)
	}
	return nil
}

func (s *PostgresStore) HasFilterRules(userID int64) (bool, error) {
	count, err := s.q.HasFilterRules(context.Background(), userID)
	if err != nil {
		return false, fmt.Errorf("has filter rules: %w", err)
	}
	return count > 0, nil
}

// --- Article summaries ---

func (s *PostgresStore) UpdateArticleAISummary(articleID int64, aiSummary string) error {
	if err := s.q.UpdateArticleAISummary(context.Background(), db.UpdateArticleAISummaryParams{
		ArticleID: articleID,
		AiSummary: aiSummary,
	}); err != nil {
		return fmt.Errorf("failed to update AI summary: %w", err)
	}
	return nil
}

func (s *PostgresStore) MarkSummarizationSkipped(articleID int64, reason string) error {
	if err := s.q.MarkSummarizationSkipped(context.Background(), db.MarkSummarizationSkippedParams{
		ArticleID:  articleID,
		SkipReason: reason,
	}); err != nil {
		return fmt.Errorf("failed to mark summarization skipped: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetArticleSummary(articleID int64) (*ArticleSummary, error) {
	r, err := s.q.GetArticleSummary(context.Background(), articleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get article summary: %w", err)
	}
	return &ArticleSummary{
		ArticleID:   r.ArticleID,
		AISummary:   r.AiSummary,
		GeneratedAt: r.GeneratedAt,
	}, nil
}

// --- Feed stats ---

// GetProcessingStats returns an aggregate snapshot of the AI pipeline state for a
// user's articles (not broken down by feed). "pending" uses ai_retries < 3 (the
// retry budget the pipeline honours); "stuck" is everything that has exhausted it.
func (s *PostgresStore) GetProcessingStats(userID int64) (*ProcessingStats, error) {
	ctx := context.Background()
	funnel, err := s.q.GetProcessingFunnel(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get processing stats (funnel): %w", err)
	}
	summaries, err := s.q.GetProcessingSummaryCounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("get processing stats (summaries): %w", err)
	}
	feeds, err := s.q.GetProcessingFeedCounts(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get processing stats (feeds): %w", err)
	}
	return &ProcessingStats{
		TotalArticles:    funnel.TotalArticles,
		Scored:           funnel.Scored,
		Pending:          funnel.Pending,
		Stuck:            funnel.Stuck,
		SecurityPassed:   funnel.SecurityPassed,
		SecurityRejected: funnel.SecurityRejected,
		SecuritySkipped:  funnel.SecuritySkipped,
		Curated:          funnel.Curated,
		Summarized:       summaries.Summarized,
		SummarizeSkipped: summaries.SummarizeSkipped,
		FeedsTotal:       feeds.FeedsTotal,
		FeedsErroring:    feeds.FeedsErroring,
	}, nil
}

// RecordCycleStats persists one completed daemon cycle, then prunes to a bounded
// history so the table can't grow without limit on a long-running daemon.
func (s *PostgresStore) RecordCycleStats(cs CycleStats) error {
	ctx := context.Background()
	if err := s.q.RecordCycleStats(ctx, db.RecordCycleStatsParams{
		CompletedAt:        cs.CompletedAt,
		DurationMs:         cs.DurationMs,
		FeedsTotal:         cs.FeedsTotal,
		FeedsDownloaded:    cs.FeedsDownloaded,
		FeedsNotModified:   cs.FeedsNotModified,
		FeedsErrored:       cs.FeedsErrored,
		NewArticles:        cs.NewArticles,
		Processed:          cs.Processed,
		HighInterest:       cs.HighInterest,
		AiBackendAvailable: cs.AIBackendAvailable,
	}); err != nil {
		return fmt.Errorf("record cycle stats: %w", err)
	}
	if err := s.q.PruneCycleStats(ctx); err != nil {
		return fmt.Errorf("prune cycle stats: %w", err)
	}
	return nil
}

func cycleStatsFromRow(r db.CycleStat) CycleStats {
	return CycleStats{
		ID:                 r.ID,
		CompletedAt:        r.CompletedAt,
		DurationMs:         r.DurationMs,
		FeedsTotal:         r.FeedsTotal,
		FeedsDownloaded:    r.FeedsDownloaded,
		FeedsNotModified:   r.FeedsNotModified,
		FeedsErrored:       r.FeedsErrored,
		NewArticles:        r.NewArticles,
		Processed:          r.Processed,
		HighInterest:       r.HighInterest,
		AIBackendAvailable: r.AiBackendAvailable,
	}
}

// GetRecentCycleStats returns the most recent completed cycles, newest first.
func (s *PostgresStore) GetRecentCycleStats(limit int) ([]CycleStats, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.q.GetRecentCycleStats(context.Background(), int32(limit))
	if err != nil {
		return nil, fmt.Errorf("get recent cycle stats: %w", err)
	}
	out := make([]CycleStats, len(rows))
	for i, r := range rows {
		out[i] = cycleStatsFromRow(r)
	}
	return out, nil
}

func (s *PostgresStore) GetFeedStats(userID int64) ([]FeedStats, error) {
	rows, err := s.q.GetFeedStats(context.Background(), userID)
	if err != nil {
		return nil, fmt.Errorf("get feed stats: %w", err)
	}
	stats := make([]FeedStats, len(rows))
	for i, r := range rows {
		fs := FeedStats{
			FeedID:               r.FeedID,
			FeedTitle:            r.FeedTitle,
			TotalArticles:        r.TotalArticles,
			UnreadArticles:       r.UnreadArticles,
			UnsummarizedArticles: r.UnsummarizedArticles,
		}
		if t, ok := r.LastPostDate.(time.Time); ok {
			fs.LastPostDate = &t
		}
		stats[i] = fs
	}
	return stats, nil
}

// --- Article groups ---

// groupFromCols builds an ArticleGroup from the standard 7-column projection
// shared by GetGroup, GetUserGroups, and GetGroupsWithEmbeddings.
func groupFromCols(id, userID int64, topic string, displayName *string, muted bool, created, updated time.Time) ArticleGroup {
	return ArticleGroup{
		ID:          id,
		UserID:      userID,
		Topic:       topic,
		DisplayName: derefString(displayName),
		Muted:       muted,
		CreatedAt:   created,
		UpdatedAt:   updated,
	}
}

func (s *PostgresStore) CreateArticleGroup(userID int64, topic string) (int64, error) {
	id, err := s.q.CreateArticleGroup(context.Background(), db.CreateArticleGroupParams{
		UserID: userID,
		Topic:  topic,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to create article group: %w", err)
	}
	return id, nil
}

func (s *PostgresStore) AddArticleToGroup(groupID, articleID int64) error {
	ctx := context.Background()
	if err := s.q.AddArticleGroupMember(ctx, db.AddArticleGroupMemberParams{
		GroupID:   groupID,
		ArticleID: articleID,
	}); err != nil {
		return fmt.Errorf("failed to add article to group: %w", err)
	}
	return s.q.TouchArticleGroup(ctx, groupID)
}

func (s *PostgresStore) GetGroupArticles(groupID int64) ([]Article, error) {
	rows, err := s.q.GetGroupArticles(context.Background(), groupID)
	if err != nil {
		return nil, fmt.Errorf("failed to get group articles: %w", err)
	}
	articles := make([]Article, len(rows))
	for i, r := range rows {
		articles[i] = coreArticle(r.ID, r.FeedID, r.Guid, r.Title, r.Url, r.Content, r.Summary, r.Author, r.PublishedDate, r.FetchedDate)
	}
	return articles, nil
}

func (s *PostgresStore) GetArticleInterestScores(userID int64, articleIDs []int64) (map[int64]float64, error) {
	if len(articleIDs) == 0 {
		return map[int64]float64{}, nil
	}
	rows, err := s.q.GetArticleInterestScores(context.Background(), db.GetArticleInterestScoresParams{
		UserID:     userID,
		ArticleIds: articleIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("get article interest scores: %w", err)
	}
	scores := make(map[int64]float64, len(rows))
	for _, r := range rows {
		scores[r.ArticleID] = derefFloat(r.InterestScore)
	}
	return scores, nil
}

// GetArticleSecurityScores returns the persisted security_score for each of the
// given articles, skipping any with a NULL score. Not user-scoped — the verdict
// lives on the article (#141). The curate stage uses it to report the verdict
// alongside the interest score (#119).
func (s *PostgresStore) GetArticleSecurityScores(articleIDs []int64) (map[int64]float64, error) {
	if len(articleIDs) == 0 {
		return map[int64]float64{}, nil
	}
	rows, err := s.q.GetArticleSecurityScores(context.Background(), articleIDs)
	if err != nil {
		return nil, fmt.Errorf("get security scores: %w", err)
	}
	scores := make(map[int64]float64, len(rows))
	for _, r := range rows {
		scores[r.ID] = derefFloat(r.SecurityScore)
	}
	return scores, nil
}

func (s *PostgresStore) UpdateGroupSummary(groupID int64, headline, summary string, articleCount int, maxInterestScore *float64) error {
	if err := s.q.UpdateGroupSummary(context.Background(), db.UpdateGroupSummaryParams{
		GroupID:          groupID,
		Headline:         headline,
		Summary:          summary,
		ArticleCount:     int64(articleCount),
		MaxInterestScore: maxInterestScore,
	}); err != nil {
		return fmt.Errorf("failed to update group summary: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetGroupSummary(groupID int64) (*GroupSummary, error) {
	r, err := s.q.GetGroupSummary(context.Background(), groupID)
	if err != nil {
		return nil, fmt.Errorf("failed to get group summary: %w", mapErr(err))
	}
	return &GroupSummary{
		GroupID:          r.GroupID,
		Headline:         r.Headline,
		Summary:          r.Summary,
		ArticleCount:     int(r.ArticleCount),
		MaxInterestScore: r.MaxInterestScore,
		GeneratedAt:      r.GeneratedAt,
	}, nil
}

func (s *PostgresStore) GetUserGroups(userID int64) ([]ArticleGroup, error) {
	rows, err := s.q.GetUserGroups(context.Background(), userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user groups: %w", err)
	}
	groups := make([]ArticleGroup, len(rows))
	for i, r := range rows {
		groups[i] = groupFromCols(r.ID, r.UserID, r.Topic, r.DisplayName, r.Muted, r.CreatedAt, r.UpdatedAt)
	}
	return groups, nil
}

func (s *PostgresStore) GetGroup(groupID int64) (*ArticleGroup, error) {
	r, err := s.q.GetGroup(context.Background(), groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get group: %w", err)
	}
	g := groupFromCols(r.ID, r.UserID, r.Topic, r.DisplayName, r.Muted, r.CreatedAt, r.UpdatedAt)
	return &g, nil
}

func (s *PostgresStore) FindArticleGroup(articleID, userID int64) (*int64, error) {
	groupID, err := s.q.FindArticleGroup(context.Background(), db.FindArticleGroupParams{
		ArticleID: articleID,
		UserID:    userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to find article group: %w", err)
	}
	return &groupID, nil
}

func (s *PostgresStore) GetUnreadGroupArticles(userID, groupID int64, limit, offset int, filterThreshold *int, includeRead bool) ([]Article, error) {
	filterSQL, filterArgs := filterScoreClausePG(userID, filterThreshold)
	query := `
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(a.security_flagged, FALSE) AS security_flagged,
		       COALESCE(rs.read, FALSE) AS is_read, COALESCE(rs.starred, FALSE) AS is_starred
		FROM articles a
		JOIN article_group_members agm ON a.id = agm.article_id
		JOIN article_groups ag ON agm.group_id = ag.id
		LEFT JOIN read_state rs ON a.id = rs.article_id AND rs.user_id = ?
		WHERE agm.group_id = ? AND ag.user_id = ?` + readFilterClausePG(includeRead) + `
		` + filterSQL + `
		ORDER BY a.published_date DESC
		LIMIT ? OFFSET ?`
	args := []any{userID, groupID, userID}
	args = append(args, filterArgs...)
	args = append(args, limit, offset)
	rows, err := s.pool.Query(context.Background(), rebindNumeric(query), args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get unread group articles: %w", err)
	}
	return scanArticlesWithReadStatePgx(rows)
}

func (s *PostgresStore) GetGroupStats(userID int64) ([]GroupStats, error) {
	rows, err := s.q.GetGroupStats(context.Background(), userID)
	if err != nil {
		return nil, fmt.Errorf("get group stats: %w", err)
	}
	stats := make([]GroupStats, len(rows))
	for i, r := range rows {
		stats[i] = GroupStats{
			GroupID:        r.GroupID,
			DisplayName:    r.DisplayName,
			UnreadArticles: r.UnreadArticles,
		}
	}
	return stats, nil
}

func (s *PostgresStore) SetGroupMuted(groupID int64, muted bool) error {
	if err := s.q.SetGroupMuted(context.Background(), db.SetGroupMutedParams{
		Muted: muted,
		ID:    groupID,
	}); err != nil {
		return fmt.Errorf("set group muted: %w", err)
	}
	return nil
}

func (s *PostgresStore) IsGroupMuted(groupID int64) (bool, error) {
	muted, err := s.q.IsGroupMuted(context.Background(), groupID)
	if err != nil {
		return false, fmt.Errorf("is group muted: %w", err)
	}
	return muted, nil
}

func (s *PostgresStore) DisbandGroup(groupID int64) error {
	if err := s.q.DisbandGroup(context.Background(), groupID); err != nil {
		return fmt.Errorf("disband group: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateGroupDisplayName(groupID int64, displayName string) error {
	if err := s.q.UpdateGroupDisplayName(context.Background(), db.UpdateGroupDisplayNameParams{
		DisplayName: &displayName,
		ID:          groupID,
	}); err != nil {
		return fmt.Errorf("update group display name: %w", err)
	}
	return nil
}

func (s *PostgresStore) UpdateGroupEmbedding(groupID int64, embedding []byte, model string) error {
	if err := s.q.UpdateGroupEmbedding(context.Background(), db.UpdateGroupEmbeddingParams{
		Embedding:      embedding,
		EmbeddingModel: model,
		ID:             groupID,
	}); err != nil {
		return fmt.Errorf("update group embedding: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetGroupsWithEmbeddings(userID int64, model string) ([]ArticleGroupWithEmbedding, error) {
	rows, err := s.q.GetGroupsWithEmbeddings(context.Background(), db.GetGroupsWithEmbeddingsParams{
		UserID:         userID,
		EmbeddingModel: model,
	})
	if err != nil {
		return nil, fmt.Errorf("get groups with embeddings: %w", err)
	}
	groups := make([]ArticleGroupWithEmbedding, len(rows))
	for i, r := range rows {
		groups[i] = ArticleGroupWithEmbedding{
			ArticleGroup: groupFromCols(r.ID, r.UserID, r.Topic, r.DisplayName, r.Muted, r.CreatedAt, r.UpdatedAt),
			Embedding:    r.Embedding,
		}
	}
	return groups, nil
}

func (s *PostgresStore) GetGroupEmbedding(groupID int64) ([]byte, error) {
	emb, err := s.q.GetGroupEmbedding(context.Background(), groupID)
	if err != nil {
		return nil, fmt.Errorf("get group embedding: %w", err)
	}
	return emb, nil
}

func (s *PostgresStore) GetGroupArticleCount(groupID int64) (int, error) {
	count, err := s.q.GetGroupArticleCount(context.Background(), groupID)
	if err != nil {
		return 0, fmt.Errorf("get group article count: %w", err)
	}
	return count, nil
}

func (s *PostgresStore) UpdateGroupTopic(groupID int64, topic string) error {
	if err := s.q.UpdateGroupTopic(context.Background(), db.UpdateGroupTopicParams{
		Topic: topic,
		ID:    groupID,
	}); err != nil {
		return fmt.Errorf("update group topic: %w", err)
	}
	return nil
}

// --- Feed favicons ---

func faviconFromRow(r db.FeedFavicon) FeedFavicon {
	return FeedFavicon{
		FeedID:    r.FeedID,
		Data:      r.Data,
		MimeType:  r.MimeType,
		FetchedAt: r.FetchedAt,
	}
}

func (s *PostgresStore) StoreFeedFavicon(feedID int64, data []byte, mimeType string) error {
	return s.q.StoreFeedFavicon(context.Background(), db.StoreFeedFaviconParams{
		FeedID:   feedID,
		Data:     data,
		MimeType: mimeType,
	})
}

func (s *PostgresStore) GetFeedFavicon(feedID int64) (*FeedFavicon, error) {
	r, err := s.q.GetFeedFavicon(context.Background(), feedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get feed favicon: %w", err)
	}
	f := faviconFromRow(r)
	return &f, nil
}

func (s *PostgresStore) GetAllFeedFavicons() ([]FeedFavicon, error) {
	rows, err := s.q.GetAllFeedFavicons(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get all feed favicons: %w", err)
	}
	favicons := make([]FeedFavicon, len(rows))
	for i, r := range rows {
		favicons[i] = faviconFromRow(r)
	}
	return favicons, nil
}

func (s *PostgresStore) GetSubscribedFeedsWithoutFavicons() ([]Feed, error) {
	rows, err := s.q.GetSubscribedFeedsWithoutFavicons(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get feeds without favicons: %w", err)
	}
	feeds := make([]Feed, len(rows))
	for i, r := range rows {
		feeds[i] = feedFromRow(r)
	}
	return feeds, nil
}

// --- Subscriptions ---

func (s *PostgresStore) SubscribeUserToFeed(userID, feedID int64) error {
	if err := s.q.SubscribeUserToFeed(context.Background(), db.SubscribeUserToFeedParams{
		UserID: userID,
		FeedID: feedID,
	}); err != nil {
		return fmt.Errorf("failed to subscribe user to feed: %w", err)
	}
	return nil
}

// AddFeedTag tags a feed for a user (idempotent; case-insensitive duplicate is a no-op).
func (s *PostgresStore) AddFeedTag(userID, feedID int64, tag string) error {
	tag = normalizeTag(tag)
	if tag == "" {
		return fmt.Errorf("empty tag")
	}
	if err := s.q.AddFeedTag(context.Background(), db.AddFeedTagParams{
		UserID: userID,
		FeedID: feedID,
		Tag:    tag,
	}); err != nil {
		return fmt.Errorf("add feed tag: %w", err)
	}
	return nil
}

// RemoveFeedTag removes a tag from a feed for a user (case-insensitive).
func (s *PostgresStore) RemoveFeedTag(userID, feedID int64, tag string) error {
	tag = normalizeTag(tag)
	if err := s.q.RemoveFeedTag(context.Background(), db.RemoveFeedTagParams{
		UserID: userID,
		FeedID: feedID,
		Tag:    tag,
	}); err != nil {
		return fmt.Errorf("remove feed tag: %w", err)
	}
	return nil
}

// GetFeedTags returns one feed's tags for a user, sorted.
func (s *PostgresStore) GetFeedTags(userID, feedID int64) ([]string, error) {
	tags, err := s.q.GetFeedTags(context.Background(), db.GetFeedTagsParams{
		UserID: userID,
		FeedID: feedID,
	})
	if err != nil {
		return nil, fmt.Errorf("get feed tags: %w", err)
	}
	return tags, nil
}

// GetAllFeedTags returns every tagged feed's tags for a user in one query.
func (s *PostgresStore) GetAllFeedTags(userID int64) (map[int64][]string, error) {
	rows, err := s.q.GetAllFeedTags(context.Background(), userID)
	if err != nil {
		return nil, fmt.Errorf("get all feed tags: %w", err)
	}
	out := make(map[int64][]string)
	for _, r := range rows {
		out[r.FeedID] = append(out[r.FeedID], r.Tag)
	}
	return out, nil
}

// GetUserTags returns the distinct tags a user has applied, sorted.
func (s *PostgresStore) GetUserTags(userID int64) ([]string, error) {
	tags, err := s.q.GetUserTags(context.Background(), userID)
	if err != nil {
		return nil, fmt.Errorf("get user tags: %w", err)
	}
	return tags, nil
}

// GetFeedsByTags resolves a set of tags to the distinct feed IDs carrying any of
// them (case-insensitive). Empty input returns nil.
func (s *PostgresStore) GetFeedsByTags(userID int64, tags []string) ([]int64, error) {
	norm := normalizeTags(tags)
	if len(norm) == 0 {
		return nil, nil
	}
	lowered := make([]string, len(norm))
	for i, t := range norm {
		lowered[i] = strings.ToLower(t)
	}
	ids, err := s.q.GetFeedsByTags(context.Background(), db.GetFeedsByTagsParams{
		UserID: userID,
		Tags:   lowered,
	})
	if err != nil {
		return nil, fmt.Errorf("get feeds by tags: %w", err)
	}
	return ids, nil
}

func userFeedFromRow(r db.GetUserFeedsRow) Feed {
	return Feed{
		ID:                r.ID,
		URL:               r.Url,
		Title:             r.Title,
		Description:       derefString(r.Description),
		SiteURL:           r.SiteUrl,
		LastFetched:       r.LastFetched,
		LastError:         r.LastError,
		ETag:              derefString(r.Etag),
		LastModified:      derefString(r.LastModified),
		Enabled:           r.Enabled,
		CreatedAt:         r.CreatedAt,
		ConsecutiveErrors: int(r.ConsecutiveErrors),
		NextFetchAt:       r.NextFetchAt,
		Status:            r.Status,
	}
}

func (s *PostgresStore) GetUserFeeds(userID int64) ([]Feed, error) {
	rows, err := s.q.GetUserFeeds(context.Background(), userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user feeds: %w", err)
	}
	feeds := make([]Feed, len(rows))
	for i, r := range rows {
		feeds[i] = userFeedFromRow(r)
	}
	return feeds, nil
}

func (s *PostgresStore) GetAllSubscribedFeeds() ([]Feed, error) {
	rows, err := s.q.GetAllSubscribedFeeds(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get subscribed feeds: %w", err)
	}
	feeds := make([]Feed, len(rows))
	for i, r := range rows {
		feeds[i] = feedFromRow(r)
	}
	return feeds, nil
}

func (s *PostgresStore) GetAllActiveSubscribedFeeds() ([]Feed, error) {
	rows, err := s.q.GetAllActiveSubscribedFeeds(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get all active subscribed feeds: %w", err)
	}
	feeds := make([]Feed, len(rows))
	for i, r := range rows {
		feeds[i] = feedFromRow(r)
	}
	return feeds, nil
}

func (s *PostgresStore) GetFeedSubscribers(feedID int64) ([]int64, error) {
	userIDs, err := s.q.GetFeedSubscribers(context.Background(), feedID)
	if err != nil {
		return nil, fmt.Errorf("failed to get feed subscribers: %w", err)
	}
	return userIDs, nil
}

func (s *PostgresStore) UnsubscribeUserFromFeed(userID, feedID int64) error {
	if err := s.q.UnsubscribeUserFromFeed(context.Background(), db.UnsubscribeUserFromFeedParams{
		UserID: userID,
		FeedID: feedID,
	}); err != nil {
		return fmt.Errorf("failed to unsubscribe user from feed: %w", err)
	}
	return nil
}

func (s *PostgresStore) DeleteFeedIfOrphaned(feedID int64) (bool, error) {
	ctx := context.Background()
	n, err := s.q.CountFeedSubscribers(ctx, feedID)
	if err != nil {
		return false, fmt.Errorf("failed to check subscribers: %w", err)
	}
	if n > 0 {
		return false, nil
	}

	const batchSize = 500
	for {
		deleted, err := s.q.DeleteFeedArticlesBatch(ctx, db.DeleteFeedArticlesBatchParams{
			FeedID: feedID,
			Lim:    batchSize,
		})
		if err != nil {
			return false, fmt.Errorf("failed to batch-delete articles for feed %d: %w", feedID, err)
		}
		if deleted == 0 {
			break
		}
	}

	rows, err := s.q.DeleteOrphanedFeed(ctx, feedID)
	if err != nil {
		return false, fmt.Errorf("failed to delete orphaned feed: %w", err)
	}
	return rows > 0, nil
}

func (s *PostgresStore) GetAllSubscribingUsers() ([]int64, error) {
	userIDs, err := s.q.GetAllSubscribingUsers(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get subscribing users: %w", err)
	}
	return userIDs, nil
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

// --- Search methods ---

// SearchArticlesFTS performs full-text search using PostgreSQL tsvector/tsquery,
// scoped to feeds the user is subscribed to. Uses websearch_to_tsquery for
// natural query syntax (quoted phrases, -exclusion).
func (s *PostgresStore) SearchArticlesFTS(userID int64, query string, limit, offset int) ([]Article, error) {
	rows, err := s.db.Query(s.db.prepare(`
		SELECT a.id, a.feed_id, a.guid, a.title, a.url, a.content, a.summary,
		       a.author, a.published_date, a.fetched_date,
		       COALESCE(a.security_flagged, FALSE) AS security_flagged,
		       COALESCE(rs.read, FALSE) AS is_read, COALESCE(rs.starred, FALSE) AS is_starred
		FROM articles a
		JOIN user_feeds uf ON a.feed_id = uf.feed_id
		LEFT JOIN read_state rs ON rs.article_id = a.id AND rs.user_id = ?
		WHERE uf.user_id = ? AND a.search_vector @@ websearch_to_tsquery('english', ?)
		ORDER BY ts_rank_cd(a.search_vector, websearch_to_tsquery('english', ?)) DESC
		LIMIT ? OFFSET ?`),
		userID, userID, query, query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer rows.Close()
	return scanArticlesWithReadState(rows)
}

// StoreArticleEmbedding upserts a successful embedding vector. Resets
// status, attempts, error_message, and last_attempted_at —
// see SQLiteStore for rationale.
func (s *PostgresStore) StoreArticleEmbedding(articleID int64, embedding []byte, model string) error {
	return s.q.StoreArticleEmbedding(context.Background(), db.StoreArticleEmbeddingParams{
		ArticleID:      articleID,
		Embedding:      embedding,
		EmbeddingModel: model,
		Status:         int16(EmbedStatusOK),
	})
}

// MarkArticleEmbeddingSkipped — see SQLiteStore for behaviour.
func (s *PostgresStore) MarkArticleEmbeddingSkipped(articleID int64, model string) error {
	return s.q.MarkArticleEmbeddingSkipped(context.Background(), db.MarkArticleEmbeddingSkippedParams{
		ArticleID:      articleID,
		Embedding:      embedSentinelBytes,
		EmbeddingModel: model,
		Status:         int16(EmbedStatusTooShort),
	})
}

// MarkArticleEmbeddingFailed — see SQLiteStore for behaviour.
func (s *PostgresStore) MarkArticleEmbeddingFailed(articleID int64, model, errMsg string) error {
	return s.q.MarkArticleEmbeddingFailed(context.Background(), db.MarkArticleEmbeddingFailedParams{
		ArticleID:      articleID,
		Embedding:      embedSentinelBytes,
		EmbeddingModel: model,
		Status:         int16(EmbedStatusError),
		ErrorMessage:   errMsg,
	})
}

// GetArticleEmbeddings returns all article embeddings for a user's subscribed feeds,
// filtered by the specified embedding model.
func (s *PostgresStore) GetArticleEmbeddings(userID int64, model string) ([]ArticleEmbeddingRow, error) {
	rows, err := s.q.GetArticleEmbeddings(context.Background(), db.GetArticleEmbeddingsParams{
		UserID:         userID,
		EmbeddingModel: model,
	})
	if err != nil {
		return nil, fmt.Errorf("get article embeddings: %w", err)
	}
	result := make([]ArticleEmbeddingRow, len(rows))
	for i, r := range rows {
		result[i] = ArticleEmbeddingRow{ArticleID: r.ArticleID, Embedding: r.Embedding}
	}
	return result, nil
}

// ResetAllArticleEmbeddings deletes every row in article_embeddings.
// See SQLiteStore.ResetAllArticleEmbeddings for usage notes.
func (s *PostgresStore) ResetAllArticleEmbeddings() (int64, error) {
	n, err := s.q.ResetAllArticleEmbeddings(context.Background())
	if err != nil {
		return 0, fmt.Errorf("reset article embeddings: %w", err)
	}
	return n, nil
}

// GetArticleEmbeddingsByIDs returns usable (status OK) embeddings for the given
// article IDs and model. See the SQLite implementation.
func (s *PostgresStore) GetArticleEmbeddingsByIDs(articleIDs []int64, model string) ([]ArticleEmbeddingRow, error) {
	if len(articleIDs) == 0 {
		return nil, nil
	}
	rows, err := s.q.GetArticleEmbeddingsByIDs(context.Background(), db.GetArticleEmbeddingsByIDsParams{
		ArticleIds:     articleIDs,
		EmbeddingModel: model,
		Status:         int16(EmbedStatusOK),
	})
	if err != nil {
		return nil, fmt.Errorf("get article embeddings by ids: %w", err)
	}
	result := make([]ArticleEmbeddingRow, len(rows))
	for i, r := range rows {
		result[i] = ArticleEmbeddingRow{ArticleID: r.ArticleID, Embedding: r.Embedding}
	}
	return result, nil
}

// ResetAllGroupEmbeddings clears the centroid and embedding_model on
// every article_groups row. See SQLiteStore.ResetAllGroupEmbeddings.
func (s *PostgresStore) ResetAllGroupEmbeddings() (int64, error) {
	n, err := s.q.ResetAllGroupEmbeddings(context.Background())
	if err != nil {
		return 0, fmt.Errorf("reset group embeddings: %w", err)
	}
	return n, nil
}

// ResetStuckEmbeddings — see SQLiteStore for behaviour.
func (s *PostgresStore) ResetStuckEmbeddings(model, errorPattern string) (int64, error) {
	ctx := context.Background()
	var (
		n   int64
		err error
	)
	if errorPattern == "" {
		n, err = s.q.ResetStuckEmbeddings(ctx, db.ResetStuckEmbeddingsParams{
			EmbeddingModel: model,
			Status:         int16(EmbedStatusError),
			MaxAttempts:    EmbedMaxAttempts,
		})
	} else {
		n, err = s.q.ResetStuckEmbeddingsLike(ctx, db.ResetStuckEmbeddingsLikeParams{
			EmbeddingModel: model,
			Status:         int16(EmbedStatusError),
			MaxAttempts:    EmbedMaxAttempts,
			ErrorPattern:   errorPattern,
		})
	}
	if err != nil {
		return 0, fmt.Errorf("reset stuck embeddings: %w", err)
	}
	return n, nil
}

// GetArticlesWithoutEmbeddings returns articles eligible for an embedding
// pass under the given model. See SQLiteStore for retry-eligibility rules.
func (s *PostgresStore) GetArticlesWithoutEmbeddings(model string, limit int) ([]Article, error) {
	cutoff := time.Now().UTC().Add(-EmbedRetryCooldown)
	rows, err := s.q.GetArticlesWithoutEmbeddings(context.Background(), db.GetArticlesWithoutEmbeddingsParams{
		EmbeddingModel: model,
		Status:         int16(EmbedStatusError),
		MaxAttempts:    EmbedMaxAttempts,
		Cutoff:         cutoff,
		Lim:            int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("get articles without embeddings: %w", err)
	}
	articles := make([]Article, len(rows))
	for i, r := range rows {
		articles[i] = coreArticle(r.ID, r.FeedID, r.Guid, r.Title, r.Url, r.Content, r.Summary, r.Author, r.PublishedDate, r.FetchedDate)
	}
	return articles, nil
}

// --- Newsletter methods ---

func (s *PostgresStore) CreateNewsletter(n *Newsletter) (int64, error) {
	configJSON, err := json.Marshal(n.Config)
	if err != nil {
		return 0, fmt.Errorf("marshal newsletter config: %w", err)
	}
	var id int64
	err = s.db.QueryRow(s.db.prepare(`
		INSERT INTO newsletters (user_id, name, schedule, config_json, prompt_template, email_recipient, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`),
		n.UserID, n.Name, n.Schedule, string(configJSON), n.PromptTemplate, n.EmailRecipient, n.Enabled).Scan(&id)
	return id, err
}

func (s *PostgresStore) UpdateNewsletter(n *Newsletter) error {
	configJSON, err := json.Marshal(n.Config)
	if err != nil {
		return fmt.Errorf("marshal newsletter config: %w", err)
	}
	_, err = s.db.Exec(s.db.prepare(`
		UPDATE newsletters
		SET name = ?, schedule = ?, config_json = ?, prompt_template = ?,
		    email_recipient = ?, enabled = ?, updated_at = NOW()
		WHERE id = ?`),
		n.Name, n.Schedule, string(configJSON), n.PromptTemplate, n.EmailRecipient, n.Enabled, n.ID)
	return err
}

func (s *PostgresStore) DeleteNewsletter(newsletterID int64) error {
	_, err := s.db.Exec(s.db.prepare(`DELETE FROM newsletters WHERE id = ?`), newsletterID)
	return err
}

func (s *PostgresStore) GetNewsletter(newsletterID int64) (*Newsletter, error) {
	var n Newsletter
	var configJSON string
	err := s.db.QueryRow(s.db.prepare(`
		SELECT id, user_id, name, schedule, config_json, prompt_template,
		       email_recipient, enabled, last_generated_at, created_at, updated_at
		FROM newsletters WHERE id = ?`), newsletterID).Scan(
		&n.ID, &n.UserID, &n.Name, &n.Schedule, &configJSON, &n.PromptTemplate,
		&n.EmailRecipient, &n.Enabled, &n.LastGeneratedAt, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(configJSON), &n.Config) //nolint:errcheck
	return &n, nil
}

func (s *PostgresStore) GetUserNewsletters(userID int64) ([]Newsletter, error) {
	rows, err := s.db.Query(s.db.prepare(`
		SELECT id, user_id, name, schedule, config_json, prompt_template,
		       email_recipient, enabled, last_generated_at, created_at, updated_at
		FROM newsletters WHERE user_id = ? ORDER BY name`), userID)
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

func (s *PostgresStore) GetDueNewsletters(schedule string) ([]Newsletter, error) {
	var interval string
	switch schedule {
	case "hourly":
		interval = "1 hour"
	case "daily":
		interval = "1 day"
	default:
		return nil, nil
	}
	rows, err := s.db.Query(s.db.prepare(`
		SELECT id, user_id, name, schedule, config_json, prompt_template,
		       email_recipient, enabled, last_generated_at, created_at, updated_at
		FROM newsletters
		WHERE enabled = TRUE AND schedule = ?
		  AND (last_generated_at IS NULL OR last_generated_at < NOW() - ?::interval)`),
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

func (s *PostgresStore) CreateNewsletterIssue(issue *NewsletterIssue) (int64, error) {
	articleIDsJSON, _ := json.Marshal(issue.ArticleIDs) //nolint:errcheck
	var id int64
	err := s.db.QueryRow(s.db.prepare(`
		INSERT INTO newsletter_issues (newsletter_id, headline, content_html, content_text, article_ids_json)
		VALUES (?, ?, ?, ?, ?) RETURNING id`),
		issue.NewsletterID, issue.Headline, issue.ContentHTML, issue.ContentText, string(articleIDsJSON)).Scan(&id)
	return id, err
}

func (s *PostgresStore) GetNewsletterIssue(issueID int64) (*NewsletterIssue, error) {
	var issue NewsletterIssue
	var articleIDsJSON string
	err := s.db.QueryRow(s.db.prepare(`
		SELECT id, newsletter_id, headline, content_html, content_text, article_ids_json, generated_at, sent_at
		FROM newsletter_issues WHERE id = ?`), issueID).Scan(
		&issue.ID, &issue.NewsletterID, &issue.Headline, &issue.ContentHTML, &issue.ContentText,
		&articleIDsJSON, &issue.GeneratedAt, &issue.SentAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(articleIDsJSON), &issue.ArticleIDs) //nolint:errcheck
	return &issue, nil
}

func (s *PostgresStore) GetLatestNewsletterIssue(newsletterID int64) (*NewsletterIssue, error) {
	var issue NewsletterIssue
	var articleIDsJSON string
	err := s.db.QueryRow(s.db.prepare(`
		SELECT id, newsletter_id, headline, content_html, content_text, article_ids_json, generated_at, sent_at
		FROM newsletter_issues WHERE newsletter_id = ?
		ORDER BY generated_at DESC LIMIT 1`), newsletterID).Scan(
		&issue.ID, &issue.NewsletterID, &issue.Headline, &issue.ContentHTML, &issue.ContentText,
		&articleIDsJSON, &issue.GeneratedAt, &issue.SentAt)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(articleIDsJSON), &issue.ArticleIDs) //nolint:errcheck
	return &issue, nil
}

func (s *PostgresStore) GetNewsletterIssues(newsletterID int64, limit, offset int) ([]NewsletterIssue, error) {
	rows, err := s.db.Query(s.db.prepare(`
		SELECT id, newsletter_id, headline, content_text, generated_at, sent_at
		FROM newsletter_issues WHERE newsletter_id = ?
		ORDER BY generated_at DESC LIMIT ? OFFSET ?`),
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

func (s *PostgresStore) MarkNewsletterIssueSent(issueID int64) error {
	_, err := s.db.Exec(s.db.prepare(`UPDATE newsletter_issues SET sent_at = NOW() WHERE id = ?`), issueID)
	return err
}

func (s *PostgresStore) UpdateNewsletterLastGenerated(newsletterID int64) error {
	_, err := s.db.Exec(s.db.prepare(`UPDATE newsletters SET last_generated_at = NOW() WHERE id = ?`), newsletterID)
	return err
}

func (s *PostgresStore) GetNewsletterStats(userID int64) ([]NewsletterStats, error) {
	rows, err := s.db.Query(s.db.prepare(`
		SELECT n.id, n.name, COUNT(ni.id) AS issue_count
		FROM newsletters n
		LEFT JOIN newsletter_issues ni ON ni.newsletter_id = n.id
		WHERE n.user_id = ? AND n.enabled = TRUE
		GROUP BY n.id, n.name
		ORDER BY n.name`), userID)
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

func (s *PostgresStore) GetNewsletterArticles(userID int64, config *NewsletterConfig, since *time.Time, limit int) ([]Article, []float64, error) {
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
		query += ` AND a.security_score >= ?`
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

	rows, err := s.db.Query(s.db.prepare(query), args...)
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

// --- Internal scan helpers ---

// scanArticlesWithReadState scans rows that include a security_flagged column
// plus the per-user read and starred flags, in that order, after the standard
// article fields. Used by the list queries that surface read/starred state.
func scanArticlesWithReadState(rows *sql.Rows) ([]Article, error) {
	var articles []Article
	for rows.Next() {
		var a Article
		if err := rows.Scan(&a.ID, &a.FeedID, &a.GUID, &a.Title, &a.URL,
			&a.Content, &a.Summary, &a.Author, &a.PublishedDate, &a.FetchedDate,
			&a.SecurityFlagged, &a.Read, &a.Starred); err != nil {
			return nil, fmt.Errorf("scan article: %w", err)
		}
		articles = append(articles, a)
	}
	return articles, rows.Err()
}

// scanArticlesWithReadStatePgx is the pgx counterpart to
// scanArticlesWithReadState: the same 10 article columns followed by
// security_flagged and the per-user read and starred flags.
func scanArticlesWithReadStatePgx(rows pgx.Rows) ([]Article, error) {
	defer rows.Close()
	var articles []Article
	for rows.Next() {
		var (
			id, feedID                 int64
			guid, title, url           string
			content, summary, author   *string
			published                  *time.Time
			fetched                    time.Time
			flagged, isRead, isStarred bool
		)
		if err := rows.Scan(&id, &feedID, &guid, &title, &url,
			&content, &summary, &author, &published, &fetched,
			&flagged, &isRead, &isStarred); err != nil {
			return nil, fmt.Errorf("scan article: %w", err)
		}
		a := coreArticle(id, feedID, guid, title, url, content, summary, author, published, fetched)
		a.SecurityFlagged = flagged
		a.Read = isRead
		a.Starred = isStarred
		articles = append(articles, a)
	}
	return articles, rows.Err()
}

// rebindNumeric rewrites '?' placeholders to pgx's '$1','$2',... form, left to
// right. The dynamic list queries are assembled from fragments that use '?';
// pgx (unlike the legacy database/sql handle) has no rebind, so it needs the
// numbered form. None of these queries contain a literal '?', so a plain scan
// is safe.
func rebindNumeric(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

// readFilterClausePG is the Postgres counterpart to readFilterClause: it uses
// boolean FALSE rather than the integer 0 SQLite tolerates for the read column.
func readFilterClausePG(includeRead bool) string {
	if includeRead {
		return ""
	}
	return " AND (rs.article_id IS NULL OR rs.read = FALSE)"
}

// filterScoreClausePG is identical in logic to filterScoreClause but named
// distinctly to avoid a duplicate-declaration collision with the SQLite version.
func filterScoreClausePG(userID int64, threshold *int) (string, []any) {
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
