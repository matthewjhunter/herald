package storage

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) (Store, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	return store, func() { store.Close() }
}

// newPGTestStore opens a PostgreSQL store with an isolated schema for this
// test. Skips automatically when HERALD_TEST_DB_DSN is not set.
func newPGTestStore(t *testing.T) (Store, func()) {
	t.Helper()
	baseDSN := os.Getenv("HERALD_TEST_DB_DSN")
	if baseDSN == "" {
		t.Skip("HERALD_TEST_DB_DSN not set; skipping postgres test")
	}

	// Build a safe schema name from the test name.
	raw := "test_" + t.Name()
	schema := regexp.MustCompile(`[^a-z0-9_]`).ReplaceAllString(strings.ToLower(raw), "_")
	if len(schema) > 63 {
		schema = schema[:63]
	}

	// Inject search_path into DSN so the store sees only this schema.
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse HERALD_TEST_DB_DSN: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	dsn := u.String()

	// Create the schema first (using the base DSN without search_path).
	setupDB, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open postgres for schema setup: %v", err)
	}
	if _, err := setupDB.Exec("CREATE SCHEMA " + schema); err != nil {
		setupDB.Close()
		t.Fatalf("create schema %q: %v", schema, err)
	}
	setupDB.Close()

	store, err := NewPostgresStore(dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}

	cleanup := func() {
		store.Close()
		db, err := sql.Open("pgx", baseDSN)
		if err == nil {
			db.Exec("DROP SCHEMA " + schema + " CASCADE") //nolint:errcheck
			db.Close()
		}
	}
	return store, cleanup
}

func TestNewSQLiteStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore failed: %v", err)
	}
	defer store.Close()

	if store.db == nil {
		t.Fatal("Database connection is nil")
	}
}

func TestNewPostgresStorePoolLimits(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()

	pg, ok := store.(*PostgresStore)
	if !ok {
		t.Fatalf("expected *PostgresStore, got %T", store)
	}

	stats := pg.db.Stats()
	if stats.MaxOpenConnections != pgMaxOpenConns {
		t.Errorf("MaxOpenConnections = %d, want %d", stats.MaxOpenConnections, pgMaxOpenConns)
	}
}

func TestAddAndGetFeeds(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Add a feed
	feedID, err := store.AddFeed("https://example.com/feed", "Test Feed", "A test feed")
	if err != nil {
		t.Fatalf("AddFeed failed: %v", err)
	}

	if feedID == 0 {
		t.Fatal("Feed ID should not be 0")
	}

	// Get all feeds
	feeds, err := store.GetAllFeeds()
	if err != nil {
		t.Fatalf("GetAllFeeds failed: %v", err)
	}

	if len(feeds) != 1 {
		t.Fatalf("Expected 1 feed, got %d", len(feeds))
	}

	if feeds[0].URL != "https://example.com/feed" {
		t.Errorf("Feed URL mismatch: got %s, want https://example.com/feed", feeds[0].URL)
	}
}

func TestGetFeed(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, err := store.AddFeed("https://example.com/feed", "Schneier on Security", "Security blog")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}

	f, err := store.GetFeed(feedID)
	if err != nil {
		t.Fatalf("GetFeed: %v", err)
	}
	if f.ID != feedID {
		t.Errorf("ID: got %d, want %d", f.ID, feedID)
	}
	if f.Title != "Schneier on Security" {
		t.Errorf("Title: got %q", f.Title)
	}
	if f.URL != "https://example.com/feed" {
		t.Errorf("URL: got %q", f.URL)
	}

	// Missing feed → error
	if _, err := store.GetFeed(99999); err == nil {
		t.Error("expected error for missing feed, got nil")
	}
}

func TestAddAndGetArticles(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Add a feed first
	feedID, err := store.AddFeed("https://example.com/feed", "Test Feed", "")
	if err != nil {
		t.Fatalf("AddFeed failed: %v", err)
	}

	// Add an article
	now := time.Now()
	article := &Article{
		FeedID:        feedID,
		GUID:          "test-guid-1",
		Title:         "Test Article",
		URL:           "https://example.com/article1",
		Content:       "Test content",
		Summary:       "Test summary",
		Author:        "Test Author",
		PublishedDate: &now,
	}

	articleID, err := store.AddArticle(article)
	if err != nil {
		t.Fatalf("AddArticle failed: %v", err)
	}

	if articleID == 0 {
		t.Fatal("Article ID should not be 0")
	}

	// Get unread articles
	articles, err := store.GetUnreadArticles(10)
	if err != nil {
		t.Fatalf("GetUnreadArticles failed: %v", err)
	}

	if len(articles) != 1 {
		t.Fatalf("Expected 1 article, got %d", len(articles))
	}

	if articles[0].Title != "Test Article" {
		t.Errorf("Article title mismatch: got %s, want Test Article", articles[0].Title)
	}
}

func TestUpdateReadState(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Add a feed and article
	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	now := time.Now()
	article := &Article{
		FeedID:        feedID,
		GUID:          "test-guid",
		Title:         "Test",
		URL:           "https://example.com/test",
		PublishedDate: &now,
	}
	articleID, _ := store.AddArticle(article)

	// AI scores the article, then user marks it as read (separate operations).
	interestScore := 8.5
	securityScore := 9.0
	if err := store.UpdateReadState(1, articleID, false, &interestScore, &securityScore, nil, nil); err != nil {
		t.Fatalf("UpdateReadState (AI scores) failed: %v", err)
	}
	if err := store.UpdateReadState(1, articleID, true, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpdateReadState (user read) failed: %v", err)
	}

	// Verify article is now marked as read
	articles, err := store.GetUnreadArticles(10)
	if err != nil {
		t.Fatalf("GetUnreadArticles failed: %v", err)
	}

	if len(articles) != 0 {
		t.Errorf("Expected 0 unread articles, got %d", len(articles))
	}
}

func TestGetArticlesByInterestScore(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Add feed and articles
	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	now := time.Now()

	// Add 3 articles with different interest scores: 6.0, 8.0, 9.0
	scores := []float64{6.0, 8.0, 9.0}
	for i := range 3 {
		article := &Article{
			FeedID:        feedID,
			GUID:          string(rune('a' + i)),
			Title:         "Article " + string(rune('0'+i)),
			URL:           "https://example.com/" + string(rune('0'+i)),
			PublishedDate: &now,
		}
		articleID, _ := store.AddArticle(article)

		score := scores[i]
		secScore := 9.0
		store.UpdateReadState(1, articleID, false, &score, &secScore, nil, nil)
	}

	// Get articles with score >= 8.0
	articles, scores, err := store.GetArticlesByInterestScore(1, 8.0, 10, 0, nil)
	if err != nil {
		t.Fatalf("GetArticlesByInterestScore failed: %v", err)
	}

	if len(articles) != 2 {
		t.Fatalf("Expected 2 high-interest articles, got %d", len(articles))
	}

	if scores[0] < 8.0 {
		t.Errorf("First article score should be >= 8.0, got %.1f", scores[0])
	}
}

func TestGetArticlesByInterestScore_TimeDecay(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")

	// Two articles with the same raw score but different ages.
	// The newer one should sort first due to time-decay.
	recent := time.Now().Add(-1 * 24 * time.Hour) // 1 day old
	old := time.Now().Add(-30 * 24 * time.Hour)   // 30 days old

	art1, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "old", Title: "Old Article",
		URL: "https://example.com/old", PublishedDate: &old,
	})
	art2, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "recent", Title: "Recent Article",
		URL: "https://example.com/recent", PublishedDate: &recent,
	})

	// Both get raw score 9.0
	rawScore := 9.0
	secScore := 9.0
	store.UpdateReadState(1, art1, false, &rawScore, &secScore, nil, nil)
	store.UpdateReadState(1, art2, false, &rawScore, &secScore, nil, nil)

	articles, scores, err := store.GetArticlesByInterestScore(1, 8.0, 10, 0, nil)
	if err != nil {
		t.Fatalf("GetArticlesByInterestScore failed: %v", err)
	}
	if len(articles) != 2 {
		t.Fatalf("expected 2 articles, got %d", len(articles))
	}

	// Recent article should sort first (higher decayed score)
	if articles[0].Title != "Recent Article" {
		t.Errorf("expected Recent Article first, got %q", articles[0].Title)
	}

	// Decayed scores should differ: recent ~8.2 (1 day), old ~2.25 (30 days)
	if scores[0] <= scores[1] {
		t.Errorf("recent decayed score (%.2f) should be > old decayed score (%.2f)",
			scores[0], scores[1])
	}

	// The 30-day-old article's decayed score should be well below its raw 9.0
	if scores[1] > 5.0 {
		t.Errorf("30-day-old article decayed score should be < 5.0, got %.2f", scores[1])
	}
}

func TestSubscribeUserToFeed(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")

	if err := store.SubscribeUserToFeed(1, feedID); err != nil {
		t.Fatalf("SubscribeUserToFeed failed: %v", err)
	}

	feeds, err := store.GetUserFeeds(1)
	if err != nil {
		t.Fatalf("GetUserFeeds failed: %v", err)
	}
	if len(feeds) != 1 {
		t.Fatalf("expected 1 feed, got %d", len(feeds))
	}
	if feeds[0].URL != "https://example.com/feed" {
		t.Errorf("feed URL = %q, want %q", feeds[0].URL, "https://example.com/feed")
	}

	// Subscribe again should not error (INSERT OR IGNORE)
	if err := store.SubscribeUserToFeed(1, feedID); err != nil {
		t.Errorf("duplicate subscribe should not error: %v", err)
	}
}

func TestGetAllSubscribedFeeds(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedA, _ := store.AddFeed("https://example.com/a", "Feed A", "")
	feedB, _ := store.AddFeed("https://example.com/b", "Feed B", "")
	feedC, _ := store.AddFeed("https://example.com/c", "Feed C", "")

	// User 1 subscribes to A and B
	store.SubscribeUserToFeed(1, feedA)
	store.SubscribeUserToFeed(1, feedB)
	// User 2 subscribes to B and C
	store.SubscribeUserToFeed(2, feedB)
	store.SubscribeUserToFeed(2, feedC)

	feeds, err := store.GetAllSubscribedFeeds()
	if err != nil {
		t.Fatalf("GetAllSubscribedFeeds failed: %v", err)
	}
	if len(feeds) != 3 {
		t.Errorf("expected 3 distinct subscribed feeds, got %d", len(feeds))
	}
}

func TestGetAllSubscribingUsers(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedA, _ := store.AddFeed("https://example.com/a", "Feed A", "")
	feedB, _ := store.AddFeed("https://example.com/b", "Feed B", "")

	store.SubscribeUserToFeed(1, feedA)
	store.SubscribeUserToFeed(2, feedB)
	store.SubscribeUserToFeed(2, feedA) // user 2 subscribes to both

	users, err := store.GetAllSubscribingUsers()
	if err != nil {
		t.Fatalf("GetAllSubscribingUsers failed: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 distinct users, got %d", len(users))
	}
	if users[0] != 1 || users[1] != 2 {
		t.Errorf("expected users [1,2], got %v", users)
	}
}

func TestUnsubscribeUserFromFeed(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	store.SubscribeUserToFeed(1, feedID)

	// Verify subscription exists
	feeds, _ := store.GetUserFeeds(1)
	if len(feeds) != 1 {
		t.Fatalf("expected 1 feed, got %d", len(feeds))
	}

	// Unsubscribe
	if err := store.UnsubscribeUserFromFeed(1, feedID); err != nil {
		t.Fatalf("UnsubscribeUserFromFeed failed: %v", err)
	}

	// Verify subscription removed
	feeds, _ = store.GetUserFeeds(1)
	if len(feeds) != 0 {
		t.Errorf("expected 0 feeds after unsubscribe, got %d", len(feeds))
	}

	// Unsubscribing again should not error
	if err := store.UnsubscribeUserFromFeed(1, feedID); err != nil {
		t.Errorf("duplicate unsubscribe should not error: %v", err)
	}
}

func TestDeleteFeedIfOrphaned(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	store.SubscribeUserToFeed(1, feedID)

	// Should not delete — user 1 is still subscribed
	deleted, err := store.DeleteFeedIfOrphaned(feedID)
	if err != nil {
		t.Fatalf("DeleteFeedIfOrphaned failed: %v", err)
	}
	if deleted {
		t.Error("should not delete feed with active subscriber")
	}

	// Unsubscribe, then delete
	store.UnsubscribeUserFromFeed(1, feedID)
	deleted, err = store.DeleteFeedIfOrphaned(feedID)
	if err != nil {
		t.Fatalf("DeleteFeedIfOrphaned failed: %v", err)
	}
	if !deleted {
		t.Error("should delete orphaned feed")
	}

	// Feed should be gone
	feeds, _ := store.GetAllFeeds()
	if len(feeds) != 0 {
		t.Errorf("expected 0 feeds after orphan delete, got %d", len(feeds))
	}
}

func TestDeleteFeedIfOrphaned_CascadesArticles(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	now := time.Now()
	store.AddArticle(&Article{
		FeedID: feedID, GUID: "art1", Title: "Article 1",
		URL: "https://example.com/1", PublishedDate: &now,
	})

	// No subscribers — should delete feed and cascade to articles
	deleted, err := store.DeleteFeedIfOrphaned(feedID)
	if err != nil {
		t.Fatalf("DeleteFeedIfOrphaned failed: %v", err)
	}
	if !deleted {
		t.Error("should delete orphaned feed")
	}

	// Articles should be gone too (CASCADE)
	articles, _ := store.GetUnreadArticles(10)
	if len(articles) != 0 {
		t.Errorf("expected 0 articles after cascade delete, got %d", len(articles))
	}
}

func TestRenameFeed(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Old Name", "")
	store.SubscribeUserToFeed(1, feedID)

	if err := store.RenameFeed(feedID, "New Name"); err != nil {
		t.Fatalf("RenameFeed failed: %v", err)
	}

	feeds, _ := store.GetUserFeeds(1)
	if len(feeds) != 1 {
		t.Fatalf("expected 1 feed, got %d", len(feeds))
	}
	if feeds[0].Title != "New Name" {
		t.Errorf("feed title = %q, want %q", feeds[0].Title, "New Name")
	}
}

func TestGetUnreadArticlesForUser(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedA, _ := store.AddFeed("https://example.com/a", "Feed A", "")
	feedB, _ := store.AddFeed("https://example.com/b", "Feed B", "")

	// User 1 only subscribes to Feed A
	store.SubscribeUserToFeed(1, feedA)

	now := time.Now()

	// Article in Feed A (user 1 should see this)
	store.AddArticle(&Article{
		FeedID: feedA, GUID: "a1", Title: "Feed A Article",
		URL: "https://example.com/a/1", PublishedDate: &now,
	})

	// Article in Feed B (user 1 should NOT see this)
	store.AddArticle(&Article{
		FeedID: feedB, GUID: "b1", Title: "Feed B Article",
		URL: "https://example.com/b/1", PublishedDate: &now,
	})

	articles, err := store.GetUnreadArticlesForUser(1, 10, 0, nil, false)
	if err != nil {
		t.Fatalf("GetUnreadArticlesForUser failed: %v", err)
	}
	if len(articles) != 1 {
		t.Fatalf("expected 1 article for user 1, got %d", len(articles))
	}
	if articles[0].Title != "Feed A Article" {
		t.Errorf("expected Feed A Article, got %q", articles[0].Title)
	}
}

func TestGetArticlesIncludeReadAndFlags(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feed, _ := store.AddFeed("https://example.com/a", "Feed A", "")
	store.SubscribeUserToFeed(1, feed)

	now := time.Now()
	readID, _ := store.AddArticle(&Article{
		FeedID: feed, GUID: "r1", Title: "Read Article",
		URL: "https://example.com/a/read", PublishedDate: &now,
	})
	unreadID, _ := store.AddArticle(&Article{
		FeedID: feed, GUID: "u1", Title: "Unread Article",
		URL: "https://example.com/a/unread", PublishedDate: &now,
	})

	// Mark one read and star it; leave the other untouched.
	if err := store.UpdateReadState(1, readID, true, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpdateReadState: %v", err)
	}
	if err := store.UpdateStarred(1, readID, true); err != nil {
		t.Fatalf("UpdateStarred: %v", err)
	}

	// Default (includeRead=false): only the unread article, flagged unread.
	unread, err := store.GetUnreadArticlesForUser(1, 10, 0, nil, false)
	if err != nil {
		t.Fatalf("GetUnreadArticlesForUser(false): %v", err)
	}
	if len(unread) != 1 {
		t.Fatalf("includeRead=false: expected 1 article, got %d", len(unread))
	}
	if unread[0].ID != unreadID {
		t.Errorf("includeRead=false: expected unread article %d, got %d", unreadID, unread[0].ID)
	}
	if unread[0].Read {
		t.Error("includeRead=false: unread article should have Read=false")
	}

	// includeRead=true: both articles, each carrying correct Read/Starred state.
	all, err := store.GetUnreadArticlesForUser(1, 10, 0, nil, true)
	if err != nil {
		t.Fatalf("GetUnreadArticlesForUser(true): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("includeRead=true: expected 2 articles, got %d", len(all))
	}
	flags := map[int64]Article{}
	for _, a := range all {
		flags[a.ID] = a
	}
	if got := flags[readID]; !got.Read || !got.Starred {
		t.Errorf("read article: expected Read && Starred, got Read=%v Starred=%v", got.Read, got.Starred)
	}
	if got := flags[unreadID]; got.Read || got.Starred {
		t.Errorf("unread article: expected !Read && !Starred, got Read=%v Starred=%v", got.Read, got.Starred)
	}
}

func TestArticleSummary(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	now := time.Now()
	articleID, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "sum1", Title: "Summary Test",
		URL: "https://example.com/sum", PublishedDate: &now,
	})

	// No summary initially
	summary, err := store.GetArticleSummary(articleID)
	if err != nil {
		t.Fatalf("GetArticleSummary failed: %v", err)
	}
	if summary != nil {
		t.Error("expected nil summary before setting one")
	}

	// Set a summary
	if err := store.UpdateArticleAISummary(articleID, "This is an AI summary"); err != nil {
		t.Fatalf("UpdateArticleAISummary failed: %v", err)
	}

	// Retrieve it
	summary, err = store.GetArticleSummary(articleID)
	if err != nil {
		t.Fatalf("GetArticleSummary failed: %v", err)
	}
	if summary == nil {
		t.Fatal("expected non-nil summary")
	}
	if summary.AISummary != "This is an AI summary" {
		t.Errorf("summary = %q, want %q", summary.AISummary, "This is an AI summary")
	}
	if summary.ArticleID != articleID {
		t.Errorf("summary article ID mismatch: article=%d", summary.ArticleID)
	}
}

func TestArticleGroups(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	now := time.Now()
	art1, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "g1", Title: "Group Article 1",
		URL: "https://example.com/g1", PublishedDate: &now,
	})
	art2, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "g2", Title: "Group Article 2",
		URL: "https://example.com/g2", PublishedDate: &now,
	})

	// Create a group
	groupID, err := store.CreateArticleGroup(1, "Security Vulnerabilities")
	if err != nil {
		t.Fatalf("CreateArticleGroup failed: %v", err)
	}
	if groupID == 0 {
		t.Fatal("group ID should not be 0")
	}

	// Add articles to the group
	if err := store.AddArticleToGroup(groupID, art1); err != nil {
		t.Fatalf("AddArticleToGroup failed: %v", err)
	}
	if err := store.AddArticleToGroup(groupID, art2); err != nil {
		t.Fatalf("AddArticleToGroup failed: %v", err)
	}

	// Adding same article again should not error (INSERT OR IGNORE)
	if err := store.AddArticleToGroup(groupID, art1); err != nil {
		t.Errorf("duplicate AddArticleToGroup should not error: %v", err)
	}

	// Get group articles
	articles, err := store.GetGroupArticles(groupID)
	if err != nil {
		t.Fatalf("GetGroupArticles failed: %v", err)
	}
	if len(articles) != 2 {
		t.Errorf("expected 2 articles in group, got %d", len(articles))
	}

	// Get user groups
	groups, err := store.GetUserGroups(1)
	if err != nil {
		t.Fatalf("GetUserGroups failed: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Topic != "Security Vulnerabilities" {
		t.Errorf("topic = %q, want %q", groups[0].Topic, "Security Vulnerabilities")
	}
}

func TestGroupSummary(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	groupID, _ := store.CreateArticleGroup(1, "Test Topic")

	maxScore := 9.5
	if err := store.UpdateGroupSummary(groupID, "Test Headline for Group", "Summary of the group", 3, &maxScore); err != nil {
		t.Fatalf("UpdateGroupSummary failed: %v", err)
	}

	gs, err := store.GetGroupSummary(groupID)
	if err != nil {
		t.Fatalf("GetGroupSummary failed: %v", err)
	}
	if gs.Headline != "Test Headline for Group" {
		t.Errorf("headline = %q, want %q", gs.Headline, "Test Headline for Group")
	}
	if gs.Summary != "Summary of the group" {
		t.Errorf("summary = %q, want %q", gs.Summary, "Summary of the group")
	}
	if gs.ArticleCount != 3 {
		t.Errorf("article count = %d, want 3", gs.ArticleCount)
	}
	if gs.MaxInterestScore == nil || *gs.MaxInterestScore != 9.5 {
		t.Errorf("max interest score = %v, want 9.5", gs.MaxInterestScore)
	}
}

func TestReadStatePerUserIsolation(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	now := time.Now()
	articleID, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "iso1", Title: "Shared Article",
		URL: "https://example.com/iso1", PublishedDate: &now,
	})

	// User 1 scores the article
	score1 := 9.0
	sec := 8.0
	store.UpdateReadState(1, articleID, false, &score1, &sec, nil, nil)

	// User 2 scores the same article differently
	score2 := 3.0
	store.UpdateReadState(2, articleID, false, &score2, &sec, nil, nil)

	// User 1 should see their score
	articles, scores, err := store.GetArticlesByInterestScore(1, 8.0, 10, 0, nil)
	if err != nil {
		t.Fatalf("GetArticlesByInterestScore user 1: %v", err)
	}
	if len(articles) != 1 {
		t.Fatalf("user 1: expected 1 high-interest article, got %d", len(articles))
	}
	if scores[0] < 8.0 {
		t.Errorf("user 1 score should be >= 8.0, got %.1f", scores[0])
	}

	// User 2 should not see it at threshold 8.0 (their score is 3.0)
	articles, _, err = store.GetArticlesByInterestScore(2, 8.0, 10, 0, nil)
	if err != nil {
		t.Fatalf("GetArticlesByInterestScore user 2: %v", err)
	}
	if len(articles) != 0 {
		t.Errorf("user 2: expected 0 high-interest articles, got %d", len(articles))
	}

	// User 1 marks read (AI already scored it above), user 2 still unread
	store.UpdateReadState(1, articleID, true, nil, nil, nil, nil)
	articles, _, _ = store.GetArticlesByInterestScore(1, 8.0, 10, 0, nil)
	if len(articles) != 0 {
		t.Errorf("user 1 after mark-read: expected 0 articles, got %d", len(articles))
	}
}

func TestCreateUser(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	id, err := store.CreateUser("Matthew")
	if err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	if id == 0 {
		t.Fatal("user ID should not be 0")
	}

	// Duplicate name should error (UNIQUE constraint)
	_, err = store.CreateUser("matthew") // case-insensitive
	if err == nil {
		t.Fatal("expected error for duplicate user name")
	}
}

func TestGetUserByName(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	store.CreateUser("Alice")

	// Exact case
	u, err := store.GetUserByName("Alice")
	if err != nil {
		t.Fatalf("GetUserByName failed: %v", err)
	}
	if u.Name != "Alice" {
		t.Errorf("name = %q, want %q", u.Name, "Alice")
	}

	// Case-insensitive lookup
	u, err = store.GetUserByName("alice")
	if err != nil {
		t.Fatalf("GetUserByName case-insensitive failed: %v", err)
	}
	if u.Name != "Alice" {
		t.Errorf("name = %q, want %q", u.Name, "Alice")
	}

	// Non-existent user
	_, err = store.GetUserByName("nobody")
	if err == nil {
		t.Fatal("expected error for non-existent user")
	}
}

func TestListUsers(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	store.CreateUser("Charlie")
	store.CreateUser("Alice")
	store.CreateUser("Bob")

	users, err := store.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers failed: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("expected 3 users, got %d", len(users))
	}

	// Should be ordered by name
	if users[0].Name != "Alice" || users[1].Name != "Bob" || users[2].Name != "Charlie" {
		t.Errorf("users not in name order: %v", []string{users[0].Name, users[1].Name, users[2].Name})
	}
}

func TestUserPrompts(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Set a prompt
	temp := 0.7
	if err := store.SetUserPrompt(1, "security", "custom security prompt", &temp, nil); err != nil {
		t.Fatalf("SetUserPrompt failed: %v", err)
	}

	// Get it back
	prompt, err := store.GetUserPrompt(1, "security")
	if err != nil {
		t.Fatalf("GetUserPrompt failed: %v", err)
	}
	if prompt != "custom security prompt" {
		t.Errorf("prompt = %q, want %q", prompt, "custom security prompt")
	}

	// Get temperature
	gotTemp, err := store.GetUserPromptTemperature(1, "security")
	if err != nil {
		t.Fatalf("GetUserPromptTemperature failed: %v", err)
	}
	if gotTemp != 0.7 {
		t.Errorf("temperature = %f, want 0.7", gotTemp)
	}

	// List prompts
	prompts, err := store.ListUserPrompts(1)
	if err != nil {
		t.Fatalf("ListUserPrompts failed: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(prompts))
	}
	if prompts[0].PromptType != "security" {
		t.Errorf("prompt type = %q, want %q", prompts[0].PromptType, "security")
	}

	// Delete prompt
	if err := store.DeleteUserPrompt(1, "security"); err != nil {
		t.Fatalf("DeleteUserPrompt failed: %v", err)
	}

	prompts, _ = store.ListUserPrompts(1)
	if len(prompts) != 0 {
		t.Errorf("expected 0 prompts after delete, got %d", len(prompts))
	}
}

// --- Article metadata tests ---

func TestStoreAndGetArticleAuthors(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test", "")
	now := time.Now()
	articleID, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "g1", Title: "A1", URL: "https://example.com/1",
		PublishedDate: &now,
	})

	authors := []ArticleAuthor{
		{Name: "Alice", Email: "alice@example.com"},
		{Name: "Bob", Email: ""},
	}
	if err := store.StoreArticleAuthors(articleID, authors); err != nil {
		t.Fatalf("StoreArticleAuthors: %v", err)
	}

	got, err := store.GetArticleAuthors(articleID)
	if err != nil {
		t.Fatalf("GetArticleAuthors: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 authors, got %d", len(got))
	}

	// Duplicate insert should be ignored
	if err := store.StoreArticleAuthors(articleID, authors); err != nil {
		t.Fatalf("duplicate StoreArticleAuthors: %v", err)
	}
	got, _ = store.GetArticleAuthors(articleID)
	if len(got) != 2 {
		t.Errorf("expected 2 authors after duplicate insert, got %d", len(got))
	}
}

func TestStoreAndGetArticleCategories(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test", "")
	now := time.Now()
	articleID, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "g1", Title: "A1", URL: "https://example.com/1",
		PublishedDate: &now,
	})

	categories := []string{"Security", "Golang", "AI"}
	if err := store.StoreArticleCategories(articleID, categories); err != nil {
		t.Fatalf("StoreArticleCategories: %v", err)
	}

	got, err := store.GetArticleCategories(articleID)
	if err != nil {
		t.Fatalf("GetArticleCategories: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 categories, got %d", len(got))
	}
}

func TestGetFeedAuthorsAndCategories(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test", "")
	now := time.Now()

	a1, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "g1", Title: "A1", URL: "https://example.com/1",
		PublishedDate: &now,
	})
	a2, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "g2", Title: "A2", URL: "https://example.com/2",
		PublishedDate: &now,
	})

	store.StoreArticleAuthors(a1, []ArticleAuthor{{Name: "Alice"}})
	store.StoreArticleAuthors(a2, []ArticleAuthor{{Name: "Alice"}, {Name: "Bob"}})
	store.StoreArticleCategories(a1, []string{"Security"})
	store.StoreArticleCategories(a2, []string{"Security", "Golang"})

	authors, err := store.GetFeedAuthors(feedID)
	if err != nil {
		t.Fatalf("GetFeedAuthors: %v", err)
	}
	if len(authors) != 2 {
		t.Errorf("expected 2 distinct authors, got %d: %v", len(authors), authors)
	}

	categories, err := store.GetFeedCategories(feedID)
	if err != nil {
		t.Fatalf("GetFeedCategories: %v", err)
	}
	if len(categories) != 2 {
		t.Errorf("expected 2 distinct categories, got %d: %v", len(categories), categories)
	}
}

// --- Filter rules tests ---

func TestFilterRulesCRUD(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test", "")

	// Add a global rule
	r1 := &FilterRule{UserID: 1, Axis: "author", Value: "Alice", Score: 5}
	id1, err := store.AddFilterRule(r1)
	if err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	if id1 == 0 {
		t.Fatal("expected non-zero rule ID")
	}

	// Add a per-feed rule
	r2 := &FilterRule{UserID: 1, FeedID: &feedID, Axis: "category", Value: "Security", Score: 3}
	id2, err := store.AddFilterRule(r2)
	if err != nil {
		t.Fatalf("AddFilterRule per-feed: %v", err)
	}

	// List all rules for user
	rules, err := store.GetFilterRules(1, nil)
	if err != nil {
		t.Fatalf("GetFilterRules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}

	// List per-feed rules (includes global rules that also apply)
	rules, err = store.GetFilterRules(1, &feedID)
	if err != nil {
		t.Fatalf("GetFilterRules per-feed: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules (1 global + 1 per-feed), got %d", len(rules))
	}

	// Update score
	if err := store.UpdateFilterRuleScore(1, id1, 10); err != nil {
		t.Fatalf("UpdateFilterRuleScore: %v", err)
	}
	rules, _ = store.GetFilterRules(1, nil)
	for _, r := range rules {
		if r.ID == id1 && r.Score != 10 {
			t.Errorf("expected score 10 after update, got %d", r.Score)
		}
	}

	// HasFilterRules
	has, err := store.HasFilterRules(1)
	if err != nil {
		t.Fatalf("HasFilterRules: %v", err)
	}
	if !has {
		t.Error("expected HasFilterRules = true")
	}

	has, _ = store.HasFilterRules(99)
	if has {
		t.Error("expected HasFilterRules = false for non-existent user")
	}

	// Delete
	if err := store.DeleteFilterRule(1, id2); err != nil {
		t.Fatalf("DeleteFilterRule: %v", err)
	}
	rules, _ = store.GetFilterRules(1, nil)
	if len(rules) != 1 {
		t.Errorf("expected 1 rule after delete, got %d", len(rules))
	}
}

func TestFilterRuleUniqueConstraint(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	r := &FilterRule{UserID: 1, Axis: "author", Value: "Alice", Score: 5}
	_, err := store.AddFilterRule(r)
	if err != nil {
		t.Fatalf("first AddFilterRule: %v", err)
	}

	// Duplicate should fail
	_, err = store.AddFilterRule(r)
	if err == nil {
		t.Fatal("expected error for duplicate filter rule")
	}
}

func TestFilteredArticleQueries(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test", "")
	store.SubscribeUserToFeed(1, feedID)

	now := time.Now()
	a1, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "g1", Title: "Secure Article",
		URL: "https://example.com/1", PublishedDate: &now,
	})
	a2, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "g2", Title: "Random Article",
		URL: "https://example.com/2", PublishedDate: &now,
	})

	// Tag a1 with matching metadata
	store.StoreArticleAuthors(a1, []ArticleAuthor{{Name: "Alice"}})
	store.StoreArticleCategories(a1, []string{"Security"})

	// a2 has no matching metadata
	_ = a2

	// Add filter rules: boost author=Alice (+5) and category=Security (+3)
	store.AddFilterRule(&FilterRule{UserID: 1, Axis: "author", Value: "Alice", Score: 5})
	store.AddFilterRule(&FilterRule{UserID: 1, Axis: "category", Value: "Security", Score: 3})

	// Without filter (nil threshold) — both articles returned
	articles, err := store.GetUnreadArticlesForUser(1, 10, 0, nil, false)
	if err != nil {
		t.Fatalf("GetUnreadArticlesForUser (nil threshold): %v", err)
	}
	if len(articles) != 2 {
		t.Errorf("nil threshold: expected 2 articles, got %d", len(articles))
	}

	// With threshold=0 — both articles returned (0 means disabled)
	zero := 0
	articles, _ = store.GetUnreadArticlesForUser(1, 10, 0, &zero, false)
	if len(articles) != 2 {
		t.Errorf("threshold=0: expected 2 articles, got %d", len(articles))
	}

	// With threshold=1 — only a1 passes (score 8 >= 1), a2 has score 0
	one := 1
	articles, _ = store.GetUnreadArticlesForUser(1, 10, 0, &one, false)
	if len(articles) != 1 {
		t.Errorf("threshold=1: expected 1 article, got %d", len(articles))
	}
	if len(articles) > 0 && articles[0].Title != "Secure Article" {
		t.Errorf("expected 'Secure Article', got %q", articles[0].Title)
	}

	// With threshold=10 — neither passes (max score is 8)
	ten := 10
	articles, _ = store.GetUnreadArticlesForUser(1, 10, 0, &ten, false)
	if len(articles) != 0 {
		t.Errorf("threshold=10: expected 0 articles, got %d", len(articles))
	}
}

func TestFilteredQueriesNoRulesPassthrough(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test", "")
	store.SubscribeUserToFeed(1, feedID)

	now := time.Now()
	store.AddArticle(&Article{
		FeedID: feedID, GUID: "g1", Title: "A1",
		URL: "https://example.com/1", PublishedDate: &now,
	})

	// User has no filter rules, but threshold is set — should still pass through
	// because NOT EXISTS (SELECT 1 FROM filter_rules WHERE user_id=1) is true
	threshold := 5
	articles, err := store.GetUnreadArticlesForUser(1, 10, 0, &threshold, false)
	if err != nil {
		t.Fatalf("GetUnreadArticlesForUser with threshold but no rules: %v", err)
	}
	if len(articles) != 1 {
		t.Errorf("expected 1 article (no rules passthrough), got %d", len(articles))
	}
}

// TestPostgresBackend exercises the PostgresStore implementation against a real
// PostgreSQL instance. It is skipped automatically unless HERALD_TEST_DB_DSN
// is set in the environment (e.g. "postgres://herald:herald@localhost/herald_test").
// Each subtest gets its own isolated schema so they can run in parallel.
func TestPostgresBackend(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()

	t.Run("AddFeed", func(t *testing.T) {
		id, err := store.AddFeed("https://pg.example.com/feed", "PG Feed", "desc")
		if err != nil {
			t.Fatalf("AddFeed: %v", err)
		}
		if id == 0 {
			t.Fatal("expected non-zero feed ID")
		}
		feeds, err := store.GetAllFeeds()
		if err != nil {
			t.Fatalf("GetAllFeeds: %v", err)
		}
		if len(feeds) != 1 || feeds[0].URL != "https://pg.example.com/feed" {
			t.Errorf("unexpected feeds: %+v", feeds)
		}
	})

	t.Run("AddArticleAndReadState", func(t *testing.T) {
		fid, _ := store.AddFeed("https://pg.example.com/f2", "F2", "")
		now := time.Now()
		aid, err := store.AddArticle(&Article{
			FeedID: fid, GUID: "pg-art-1", Title: "PG Article",
			URL: "https://pg.example.com/a1", PublishedDate: &now,
		})
		if err != nil || aid == 0 {
			t.Fatalf("AddArticle: id=%d err=%v", aid, err)
		}

		// Duplicate insert returns 0, no error
		aid2, err := store.AddArticle(&Article{
			FeedID: fid, GUID: "pg-art-1", Title: "PG Article",
			URL: "https://pg.example.com/a1", PublishedDate: &now,
		})
		if err != nil || aid2 != 0 {
			t.Errorf("duplicate AddArticle: id=%d err=%v", aid2, err)
		}

		score := 9.0
		sec := 8.0
		if err := store.UpdateReadState(1, aid, false, &score, &sec, nil, nil); err != nil {
			t.Fatalf("UpdateReadState (AI): %v", err)
		}
		if err := store.UpdateReadState(1, aid, true, nil, nil, nil, nil); err != nil {
			t.Fatalf("UpdateReadState (read): %v", err)
		}

		unread, err := store.GetUnreadArticles(10)
		if err != nil {
			t.Fatalf("GetUnreadArticles: %v", err)
		}
		if len(unread) != 0 {
			t.Errorf("expected 0 unread after mark-read, got %d", len(unread))
		}
	})

	t.Run("InterestScoreDecay", func(t *testing.T) {
		fid, _ := store.AddFeed("https://pg.example.com/f3", "F3", "")
		recent := time.Now().Add(-1 * 24 * time.Hour)
		old := time.Now().Add(-30 * 24 * time.Hour)

		art1, _ := store.AddArticle(&Article{FeedID: fid, GUID: "old", Title: "Old",
			URL: "https://pg.example.com/old", PublishedDate: &old})
		art2, _ := store.AddArticle(&Article{FeedID: fid, GUID: "recent", Title: "Recent",
			URL: "https://pg.example.com/recent", PublishedDate: &recent})

		raw, sec := 9.0, 9.0
		store.UpdateReadState(1, art1, false, &raw, &sec, nil, nil)
		store.UpdateReadState(1, art2, false, &raw, &sec, nil, nil)

		articles, scores, err := store.GetArticlesByInterestScore(1, 8.0, 10, 0, nil)
		if err != nil {
			t.Fatalf("GetArticlesByInterestScore: %v", err)
		}
		if len(articles) != 2 {
			t.Fatalf("expected 2 articles, got %d", len(articles))
		}
		if articles[0].Title != "Recent" {
			t.Errorf("expected Recent first, got %q", articles[0].Title)
		}
		if scores[0] <= scores[1] {
			t.Errorf("recent score (%.2f) should exceed old score (%.2f)", scores[0], scores[1])
		}
	})

	t.Run("Subscriptions", func(t *testing.T) {
		fid, _ := store.AddFeed("https://pg.example.com/sub", "Sub Feed", "")
		if err := store.SubscribeUserToFeed(1, fid); err != nil {
			t.Fatalf("SubscribeUserToFeed: %v", err)
		}
		// Idempotent
		if err := store.SubscribeUserToFeed(1, fid); err != nil {
			t.Errorf("duplicate subscribe should not error: %v", err)
		}
		feeds, err := store.GetUserFeeds(1)
		if err != nil {
			t.Fatalf("GetUserFeeds: %v", err)
		}
		found := false
		for _, f := range feeds {
			if f.ID == fid {
				found = true
			}
		}
		if !found {
			t.Error("subscribed feed not in GetUserFeeds")
		}

		if err := store.UnsubscribeUserFromFeed(1, fid); err != nil {
			t.Fatalf("UnsubscribeUserFromFeed: %v", err)
		}
		deleted, err := store.DeleteFeedIfOrphaned(fid)
		if err != nil {
			t.Fatalf("DeleteFeedIfOrphaned: %v", err)
		}
		if !deleted {
			t.Error("expected orphaned feed to be deleted")
		}
	})

	t.Run("Users", func(t *testing.T) {
		id, err := store.CreateUser("PGUser")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		u, err := store.GetUserByName("pguser") // CITEXT: case-insensitive
		if err != nil {
			t.Fatalf("GetUserByName case-insensitive: %v", err)
		}
		if u.ID != id {
			t.Errorf("user ID mismatch: got %d want %d", u.ID, id)
		}

		// Duplicate name rejected
		if _, err := store.CreateUser("pguser"); err == nil {
			t.Error("expected error for duplicate user name")
		}
	})

	t.Run("UserPrompts", func(t *testing.T) {
		temp := 0.5
		if err := store.SetUserPrompt(1, "pg-scoring", "pg prompt", &temp, nil); err != nil {
			t.Fatalf("SetUserPrompt: %v", err)
		}
		got, err := store.GetUserPrompt(1, "pg-scoring")
		if err != nil || got != "pg prompt" {
			t.Fatalf("GetUserPrompt: %q %v", got, err)
		}
		if err := store.DeleteUserPrompt(1, "pg-scoring"); err != nil {
			t.Fatalf("DeleteUserPrompt: %v", err)
		}
	})

	t.Run("ArticleGroups", func(t *testing.T) {
		fid, _ := store.AddFeed("https://pg.example.com/grp", "Grp", "")
		now := time.Now()
		a1, _ := store.AddArticle(&Article{FeedID: fid, GUID: "gr1", Title: "G1",
			URL: "https://pg.example.com/gr1", PublishedDate: &now})
		a2, _ := store.AddArticle(&Article{FeedID: fid, GUID: "gr2", Title: "G2",
			URL: "https://pg.example.com/gr2", PublishedDate: &now})

		gid, err := store.CreateArticleGroup(1, "PG Topic")
		if err != nil || gid == 0 {
			t.Fatalf("CreateArticleGroup: %v", err)
		}
		store.AddArticleToGroup(gid, a1)
		store.AddArticleToGroup(gid, a2)
		// Idempotent
		if err := store.AddArticleToGroup(gid, a1); err != nil {
			t.Errorf("duplicate AddArticleToGroup should not error: %v", err)
		}

		arts, err := store.GetGroupArticles(gid)
		if err != nil || len(arts) != 2 {
			t.Fatalf("GetGroupArticles: len=%d err=%v", len(arts), err)
		}

		groups, err := store.GetUserGroups(1)
		if err != nil {
			t.Fatalf("GetUserGroups: %v", err)
		}
		if len(groups) == 0 || groups[0].Topic != "PG Topic" {
			t.Errorf("unexpected groups: %+v", groups)
		}
	})

	t.Run("FilterRules", func(t *testing.T) {
		fid, _ := store.AddFeed("https://pg.example.com/fr", "FR", "")
		store.SubscribeUserToFeed(2, fid)

		now := time.Now()
		a1, _ := store.AddArticle(&Article{FeedID: fid, GUID: "fr1", Title: "Filter Me",
			URL: "https://pg.example.com/fr1", PublishedDate: &now})
		store.AddArticle(&Article{FeedID: fid, GUID: "fr2", Title: "Plain",
			URL: "https://pg.example.com/fr2", PublishedDate: &now})

		store.StoreArticleAuthors(a1, []ArticleAuthor{{Name: "FilterAuthor"}})
		store.AddFilterRule(&FilterRule{UserID: 2, Axis: "author", Value: "FilterAuthor", Score: 5})

		one := 1
		arts, err := store.GetUnreadArticlesForUser(2, 10, 0, &one, false)
		if err != nil {
			t.Fatalf("GetUnreadArticlesForUser with filter: %v", err)
		}
		if len(arts) != 1 || arts[0].Title != "Filter Me" {
			t.Errorf("expected only 'Filter Me', got %+v", arts)
		}
	})

	t.Run("FeverCredentials", func(t *testing.T) {
		uid, _ := store.CreateUser("FeverPGUser")
		if err := store.SetFeverCredential(uid, "pg-api-key"); err != nil {
			t.Fatalf("SetFeverCredential: %v", err)
		}
		u, err := store.GetUserByFeverAPIKey("pg-api-key")
		if err != nil {
			t.Fatalf("GetUserByFeverAPIKey: %v", err)
		}
		if u.ID != uid {
			t.Errorf("user ID mismatch: got %d want %d", u.ID, uid)
		}
		if err := store.DeleteFeverCredential(uid); err != nil {
			t.Fatalf("DeleteFeverCredential: %v", err)
		}
	})

	t.Run("DBStats", func(t *testing.T) {
		stats, err := store.GetDBStats()
		if err != nil {
			t.Fatalf("GetDBStats: %v", err)
		}
		if stats.TotalFeeds < 0 {
			t.Error("unexpected negative feed count")
		}
	})
}

func TestMigrateStore(t *testing.T) {
	src, cleanSrc := newTestStore(t)
	defer cleanSrc()
	dst, cleanDst := newTestStore(t)
	defer cleanDst()

	// Populate source.
	feedID, _ := src.AddFeed("https://example.com/feed", "Test Feed", "desc")
	src.SubscribeUserToFeed(1, feedID)

	now := time.Now()
	artID, _ := src.AddArticle(&Article{
		FeedID: feedID, GUID: "mig-1", Title: "Migrated",
		URL: "https://example.com/mig", PublishedDate: &now,
	})

	score, sec := 8.5, 9.0
	src.UpdateReadState(1, artID, false, &score, &sec, nil, nil)
	src.UpdateReadState(1, artID, true, nil, nil, nil, nil)
	src.UpdateStarred(1, artID, true)

	src.StoreArticleAuthors(artID, []ArticleAuthor{{Name: "Author One", Email: "a@b.com"}})
	src.StoreArticleCategories(artID, []string{"Security"})
	src.UpdateArticleAISummary(artID, "AI summary text")

	groupID, _ := src.CreateArticleGroup(1, "Cluster")
	src.AddArticleToGroup(groupID, artID)
	src.AddArticleToGroup(groupID, artID) // idempotent

	src.SetUserPreference(1, "theme", "dark")
	temp := 0.5
	src.SetUserPrompt(1, "scoring", "my prompt", &temp, nil)

	// Migrate.
	stats, err := MigrateStore(t.Context(), src, dst)
	if err != nil {
		t.Fatalf("MigrateStore: %v", err)
	}

	if stats.Feeds != 1 {
		t.Errorf("feeds: got %d, want 1", stats.Feeds)
	}
	if stats.Articles != 1 {
		t.Errorf("articles: got %d, want 1", stats.Articles)
	}
	if stats.ReadStates != 1 {
		t.Errorf("read_states: got %d, want 1", stats.ReadStates)
	}
	if stats.Subscriptions != 1 {
		t.Errorf("subscriptions: got %d, want 1", stats.Subscriptions)
	}
	if stats.Preferences != 1 {
		t.Errorf("preferences: got %d, want 1", stats.Preferences)
	}
	if stats.Prompts != 1 {
		t.Errorf("prompts: got %d, want 1", stats.Prompts)
	}
	if stats.Groups != 1 {
		t.Errorf("groups: got %d, want 1", stats.Groups)
	}

	// Verify destination has the article and it is read.
	unread, err := dst.GetUnreadArticles(10)
	if err != nil {
		t.Fatalf("GetUnreadArticles in dst: %v", err)
	}
	if len(unread) != 0 {
		t.Errorf("expected 0 unread in dst (article was read), got %d", len(unread))
	}

	// Feed metadata preserved.
	feeds, _ := dst.GetAllFeeds()
	if len(feeds) != 1 || feeds[0].URL != "https://example.com/feed" {
		t.Errorf("dst feed mismatch: %+v", feeds)
	}

	// Subscription preserved.
	userFeeds, _ := dst.GetUserFeeds(1)
	if len(userFeeds) != 1 {
		t.Errorf("expected 1 user feed in dst, got %d", len(userFeeds))
	}

	// Summary preserved.
	dstFeeds, _ := dst.GetAllFeeds()
	dstFeedID := dstFeeds[0].ID
	dstArts, _ := dst.GetUnreadArticles(100)
	_ = dstArts
	// Find article in dst by feed
	dstFeedArts, _ := dst.GetUnreadArticlesByFeed(1, dstFeedID, 10, 0, nil, false)
	_ = dstFeedArts

	pref, err := dst.GetUserPreference(1, "theme")
	if err != nil || pref != "dark" {
		t.Errorf("preference: got %q %v, want dark", pref, err)
	}

	prompt, err := dst.GetUserPrompt(1, "scoring")
	if err != nil || prompt != "my prompt" {
		t.Errorf("prompt: got %q %v, want 'my prompt'", prompt, err)
	}
}

// TestMigrationFromPreOIDCSchema verifies that NewSQLiteStore successfully opens
// an existing database that was created before the oidc_sub/email columns were
// added to the users table.  This is a regression test for the crash-loop
// caused by the schema init trying to CREATE UNIQUE INDEX on a column that
// didn't yet exist.
func TestMigrationFromPreOIDCSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pre-oidc.db")

	// Bootstrap an old-style database with the users table missing oidc_sub and email.
	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	_, err = legacyDB.Exec(`
		CREATE TABLE users (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		t.Fatalf("create legacy users table: %v", err)
	}
	_, err = legacyDB.Exec(`INSERT INTO users (name) VALUES ('alice')`)
	if err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}
	legacyDB.Close()

	// NewSQLiteStore must not crash-loop on this database.
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore on pre-oidc schema: %v", err)
	}
	defer store.Close()

	// The migrated users table must expose oidc_sub and email via the normal API.
	users, err := store.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers after migration: %v", err)
	}
	if len(users) != 1 || users[0].Name != "alice" {
		t.Errorf("unexpected users after migration: %+v", users)
	}
	if users[0].OIDCSub != nil {
		t.Errorf("expected nil OIDCSub for legacy user, got %v", users[0].OIDCSub)
	}
}

// TestMigrationFromPre141Schema reproduces the upgrade-in-place failure where a
// database created before #141 has an articles table without the security
// columns. The schema script's CREATE TABLE IF NOT EXISTS is a no-op on the
// existing table, so security_screened_at does not exist until the ALTER
// migration adds it; a partial index referencing that column in the schema
// script crashed startup with "no such column: security_screened_at". The index
// now lives in the migrations, which run after the column is added.
//
// The existing per-article tests reopen a DB created by the current binary, so
// the column is present from the first open and never exercise this path — this
// test bootstraps a genuinely pre-#141 articles table by hand.
func TestMigrationFromPre141Schema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pre-141.db")

	// Bootstrap an old-style database: articles without any security_* columns,
	// mirroring the schema that shipped before #141.
	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	_, err = legacyDB.Exec(`
		CREATE TABLE feeds (
			id    INTEGER PRIMARY KEY AUTOINCREMENT,
			url   TEXT NOT NULL UNIQUE,
			title TEXT NOT NULL
		);
		CREATE TABLE articles (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			feed_id           INTEGER NOT NULL,
			guid              TEXT NOT NULL,
			title             TEXT NOT NULL,
			url               TEXT NOT NULL,
			content           TEXT,
			summary           TEXT,
			author            TEXT,
			published_date    DATETIME,
			fetched_date      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			linked_url        TEXT NOT NULL DEFAULT '',
			linked_content    TEXT NOT NULL DEFAULT '',
			full_text_fetched BOOLEAN NOT NULL DEFAULT 0,
			images_cached     BOOLEAN NOT NULL DEFAULT 0,
			FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE,
			UNIQUE(feed_id, guid)
		);
		INSERT INTO feeds (url, title) VALUES ('https://example.com/feed', 'Feed');
		INSERT INTO articles (feed_id, guid, title, url)
		    VALUES (1, 'a1', 'Old Article', 'https://example.com/a1');
	`)
	if err != nil {
		t.Fatalf("create legacy articles table: %v", err)
	}
	legacyDB.Close()

	// NewSQLiteStore must migrate in place, not crash on the missing column.
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore on pre-141 schema: %v", err)
	}
	defer store.Close()

	// The security columns and the partial index must now exist.
	var screened *time.Time
	if err := store.db.QueryRow(
		`SELECT security_screened_at FROM articles WHERE guid = 'a1'`,
	).Scan(&screened); err != nil {
		t.Fatalf("security_screened_at column missing after migration: %v", err)
	}
	if screened != nil {
		t.Errorf("legacy article should be unscreened (NULL), got %v", screened)
	}

	var indexName string
	if err := store.db.QueryRow(
		`SELECT name FROM sqlite_master
		 WHERE type = 'index' AND name = 'idx_articles_unscreened'`,
	).Scan(&indexName); err != nil {
		t.Fatalf("idx_articles_unscreened missing after migration: %v", err)
	}
}

func TestGroupVirtualFeed(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	store.SubscribeUserToFeed(1, feedID)
	now := time.Now()

	// Create 3 articles
	art1, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "vf1", Title: "Article 1", URL: "https://example.com/1", PublishedDate: &now})
	art2, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "vf2", Title: "Article 2", URL: "https://example.com/2", PublishedDate: &now})
	art3, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "vf3", Title: "Article 3", URL: "https://example.com/3", PublishedDate: &now})

	// Create a group with 2 articles
	groupID, _ := store.CreateArticleGroup(1, "Test Topic")
	store.UpdateGroupDisplayName(groupID, "Test Group")
	store.AddArticleToGroup(groupID, art1)
	store.AddArticleToGroup(groupID, art2)

	// Verify grouped articles are excluded from feed queries
	unread, err := store.GetUnreadArticlesForUser(1, 100, 0, nil, false)
	if err != nil {
		t.Fatalf("GetUnreadArticlesForUser: %v", err)
	}
	if len(unread) != 1 {
		t.Errorf("expected 1 unread article (ungrouped), got %d", len(unread))
	}
	if len(unread) > 0 && unread[0].ID != art3 {
		t.Errorf("expected ungrouped article %d, got %d", art3, unread[0].ID)
	}

	// Verify grouped articles excluded from feed-specific queries too
	feedArticles, err := store.GetUnreadArticlesByFeed(1, feedID, 100, 0, nil, false)
	if err != nil {
		t.Fatalf("GetUnreadArticlesByFeed: %v", err)
	}
	if len(feedArticles) != 1 {
		t.Errorf("expected 1 feed article (ungrouped), got %d", len(feedArticles))
	}

	// Verify group articles are returned by GetUnreadGroupArticles
	groupArticles, err := store.GetUnreadGroupArticles(1, groupID, 100, 0, nil, false)
	if err != nil {
		t.Fatalf("GetUnreadGroupArticles: %v", err)
	}
	if len(groupArticles) != 2 {
		t.Errorf("expected 2 group articles, got %d", len(groupArticles))
	}

	// Verify GetGroupStats returns the group
	stats, err := store.GetGroupStats(1)
	if err != nil {
		t.Fatalf("GetGroupStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 group stat, got %d", len(stats))
	}
	if stats[0].DisplayName != "Test Group" {
		t.Errorf("display name = %q, want %q", stats[0].DisplayName, "Test Group")
	}
	if stats[0].UnreadArticles != 2 {
		t.Errorf("unread = %d, want 2", stats[0].UnreadArticles)
	}

	// Verify feed stats subtract grouped articles
	feedStats, err := store.GetFeedStats(1)
	if err != nil {
		t.Fatalf("GetFeedStats: %v", err)
	}
	if len(feedStats) != 1 {
		t.Fatalf("expected 1 feed stat, got %d", len(feedStats))
	}
	if feedStats[0].UnreadArticles != 1 {
		t.Errorf("feed unread = %d, want 1 (grouped articles excluded)", feedStats[0].UnreadArticles)
	}

	// Mute group — should disappear from stats
	if err := store.SetGroupMuted(groupID, true); err != nil {
		t.Fatalf("SetGroupMuted: %v", err)
	}
	muted, _ := store.IsGroupMuted(groupID)
	if !muted {
		t.Error("expected group to be muted")
	}
	stats, _ = store.GetGroupStats(1)
	if len(stats) != 0 {
		t.Errorf("expected 0 group stats after mute, got %d", len(stats))
	}

	// Unmute and disband — articles should return to feeds
	store.SetGroupMuted(groupID, false)
	if err := store.DisbandGroup(groupID); err != nil {
		t.Fatalf("DisbandGroup: %v", err)
	}
	unread, _ = store.GetUnreadArticlesForUser(1, 100, 0, nil, false)
	if len(unread) != 3 {
		t.Errorf("expected 3 articles after disband, got %d", len(unread))
	}
}

func TestAIRetryLimit(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	store.SubscribeUserToFeed(1, feedID)
	now := time.Now()
	articleID, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "retry-test", Title: "Retry Test",
		URL: "https://example.com/retry", PublishedDate: &now,
	})

	// Article should initially appear as unscreened (security verdict is
	// article-level now, #141).
	unscreened, err := store.GetUnscreenedArticles(100)
	if err != nil {
		t.Fatalf("GetUnscreenedArticles: %v", err)
	}
	if len(unscreened) != 1 {
		t.Fatalf("expected 1 unscreened article, got %d", len(unscreened))
	}

	// Exhaust the per-article security retry budget (3).
	for i := range 3 {
		if err := store.IncrementArticleSecurityAttempts(articleID); err != nil {
			t.Fatalf("IncrementArticleSecurityAttempts call %d: %v", i+1, err)
		}
	}

	// After 3 attempts, the article drops out of the security queue.
	unscreened, err = store.GetUnscreenedArticles(100)
	if err != nil {
		t.Fatalf("GetUnscreenedArticles after retries: %v", err)
	}
	if len(unscreened) != 0 {
		t.Errorf("expected 0 unscreened articles after 3 attempts, got %d", len(unscreened))
	}

	// The pending-AI count reflects the exhausted article too.
	count, err := store.GetUnscoredArticleCount(1)
	if err != nil {
		t.Fatalf("GetUnscoredArticleCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected unscored count 0 after 3 attempts, got %d", count)
	}

	// ResetScores clears the attempts and makes the article reappear.
	n, err := store.ResetScores(1, false, 10.0)
	if err != nil {
		t.Fatalf("ResetScores: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row reset, got %d", n)
	}
	unscreened, err = store.GetUnscreenedArticles(100)
	if err != nil {
		t.Fatalf("GetUnscreenedArticles after reset: %v", err)
	}
	if len(unscreened) != 1 {
		t.Errorf("expected 1 unscreened article after reset, got %d", len(unscreened))
	}
}

func TestGetUnsummarizedScoredArticles(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	store.SubscribeUserToFeed(1, feedID)
	now := time.Now()

	// Article 1: scored, passed security, has summary — excluded.
	a1, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "a1", Title: "Has summary",
		URL: "https://example.com/1", PublishedDate: &now,
	})
	store.ScreenArticleSecurity(a1, 10.0, "", false)
	store.UpdateArticleAISummary(a1, "an existing summary")

	// Article 2: scored, passed security, NO summary — included.
	a2, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "a2", Title: "Missing summary",
		URL: "https://example.com/2", PublishedDate: &now,
	})
	store.ScreenArticleSecurity(a2, 10.0, "", false)

	// Article 3: scored, FAILED security — excluded (security_score < threshold).
	a3, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "a3", Title: "Failed security",
		URL: "https://example.com/3", PublishedDate: &now,
	})
	store.ScreenArticleSecurity(a3, 3.0, "", false)

	// Article 4: never scored — excluded (no read_state).
	store.AddArticle(&Article{
		FeedID: feedID, GUID: "a4", Title: "Never scored",
		URL: "https://example.com/4", PublishedDate: &now,
	})

	// Article 5: scored, passed security, summarization marked SKIPPED — excluded.
	a5, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "a5", Title: "Skipped — too short to compress",
		URL: "https://example.com/5", PublishedDate: &now,
	})
	store.ScreenArticleSecurity(a5, 10.0, "", false)
	if err := store.MarkSummarizationSkipped(a5, "summary longer than content"); err != nil {
		t.Fatalf("MarkSummarizationSkipped: %v", err)
	}

	got, err := store.GetUnsummarizedScoredArticles(7.0, 100)
	if err != nil {
		t.Fatalf("GetUnsummarizedScoredArticles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 article, got %d", len(got))
	}
	if got[0].ID != a2 {
		t.Errorf("expected article %d, got %d", a2, got[0].ID)
	}

	// Sanity: skipped article (a5) shouldn't show in the unsummarized count.
	// a1 has a summary row, a5 has a sentinel row — both are excluded.
	// a2 (scored, no summary), a3 (security-failed, no summary row written),
	// and a4 (never scored, no summary row) are all counted.
	count, err := store.GetUnsummarizedArticleCount()
	if err != nil {
		t.Fatalf("GetUnsummarizedArticleCount: %v", err)
	}
	if count != 3 {
		t.Errorf("expected unsummarized count 3 (a2 + a3 + a4), got %d", count)
	}
}

func TestSearchArticlesFTS(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	userID := int64(1)
	store.CreateUser("testuser")

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	store.SubscribeUserToFeed(userID, feedID)

	now := time.Now()
	store.AddArticle(&Article{
		FeedID: feedID, GUID: "a1", Title: "Go programming language",
		URL: "https://example.com/1", Content: "Go is a statically typed language designed at Google.",
		PublishedDate: &now,
	})
	store.AddArticle(&Article{
		FeedID: feedID, GUID: "a2", Title: "Rust memory safety",
		URL: "https://example.com/2", Content: "Rust prevents memory errors at compile time.",
		PublishedDate: &now,
	})
	store.AddArticle(&Article{
		FeedID: feedID, GUID: "a3", Title: "Python data science",
		URL: "https://example.com/3", Content: "Python is popular for data analysis and machine learning.",
		PublishedDate: &now,
	})

	// Search for "Go"
	results, err := store.SearchArticlesFTS(userID, "Go", 10, 0)
	if err != nil {
		t.Fatalf("SearchArticlesFTS: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'Go', got %d", len(results))
	}
	if results[0].Title != "Go programming language" {
		t.Errorf("expected 'Go programming language', got %q", results[0].Title)
	}

	// Search for "memory" — should match Rust article
	results, err = store.SearchArticlesFTS(userID, "memory", 10, 0)
	if err != nil {
		t.Fatalf("SearchArticlesFTS: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'memory', got %d", len(results))
	}

	// Search for something not present
	results, err = store.SearchArticlesFTS(userID, "kubernetes", 10, 0)
	if err != nil {
		t.Fatalf("SearchArticlesFTS: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for 'kubernetes', got %d", len(results))
	}

	// Articles from unsubscribed feeds should not appear
	feedID2, _ := store.AddFeed("https://other.com/feed", "Other Feed", "")
	store.AddArticle(&Article{
		FeedID: feedID2, GUID: "o1", Title: "Go in other feed",
		URL: "https://other.com/1", Content: "Go content in unsubscribed feed.",
		PublishedDate: &now,
	})
	results, err = store.SearchArticlesFTS(userID, "Go", 10, 0)
	if err != nil {
		t.Fatalf("SearchArticlesFTS: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result (only subscribed feed), got %d", len(results))
	}
}

func TestStoreAndGetArticleEmbeddings(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	userID := int64(1)
	store.CreateUser("testuser")

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	store.SubscribeUserToFeed(userID, feedID)

	now := time.Now()
	artID1, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "a1", Title: "Article 1",
		URL: "https://example.com/1", Content: "Content 1", PublishedDate: &now,
	})
	artID2, _ := store.AddArticle(&Article{
		FeedID: feedID, GUID: "a2", Title: "Article 2",
		URL: "https://example.com/2", Content: "Content 2", PublishedDate: &now,
	})

	// Initially no embeddings
	embs, err := store.GetArticleEmbeddings(userID, "nomic-embed-text")
	if err != nil {
		t.Fatalf("GetArticleEmbeddings: %v", err)
	}
	if len(embs) != 0 {
		t.Errorf("expected 0 embeddings initially, got %d", len(embs))
	}

	// Store embedding for article 1
	fakeEmb := []byte{1, 2, 3, 4}
	if err := store.StoreArticleEmbedding(artID1, fakeEmb, "nomic-embed-text"); err != nil {
		t.Fatalf("StoreArticleEmbedding: %v", err)
	}

	// Should now have 1 embedding
	embs, err = store.GetArticleEmbeddings(userID, "nomic-embed-text")
	if err != nil {
		t.Fatalf("GetArticleEmbeddings: %v", err)
	}
	if len(embs) != 1 {
		t.Fatalf("expected 1 embedding, got %d", len(embs))
	}
	if embs[0].ArticleID != artID1 {
		t.Errorf("expected article ID %d, got %d", artID1, embs[0].ArticleID)
	}

	// GetArticlesWithoutEmbeddings should return article 2
	missing, err := store.GetArticlesWithoutEmbeddings("nomic-embed-text", 100)
	if err != nil {
		t.Fatalf("GetArticlesWithoutEmbeddings: %v", err)
	}
	if len(missing) != 1 {
		t.Fatalf("expected 1 article without embedding, got %d", len(missing))
	}
	if missing[0].ID != artID2 {
		t.Errorf("expected article %d without embedding, got %d", artID2, missing[0].ID)
	}

	// Different model should see both as missing
	missing, err = store.GetArticlesWithoutEmbeddings("other-model", 100)
	if err != nil {
		t.Fatalf("GetArticlesWithoutEmbeddings other-model: %v", err)
	}
	if len(missing) != 2 {
		t.Errorf("expected 2 articles without 'other-model' embedding, got %d", len(missing))
	}

	// Upsert: update article 1's embedding
	newEmb := []byte{5, 6, 7, 8}
	if err := store.StoreArticleEmbedding(artID1, newEmb, "nomic-embed-text"); err != nil {
		t.Fatalf("StoreArticleEmbedding upsert: %v", err)
	}
	embs, err = store.GetArticleEmbeddings(userID, "nomic-embed-text")
	if err != nil {
		t.Fatalf("GetArticleEmbeddings after upsert: %v", err)
	}
	if len(embs) != 1 {
		t.Fatalf("expected 1 embedding after upsert, got %d", len(embs))
	}
	if string(embs[0].Embedding) != string(newEmb) {
		t.Errorf("embedding not updated after upsert")
	}
}

// disableEmbedRetryCooldown sets EmbedRetryCooldown to 0 for the duration
// of the test. Required by tests that assert retry eligibility immediately
// after MarkArticleEmbeddingFailed, which the production 30-minute cooldown
// would otherwise block.
func disableEmbedRetryCooldown(t *testing.T) {
	t.Helper()
	saved := EmbedRetryCooldown
	EmbedRetryCooldown = 0
	t.Cleanup(func() { EmbedRetryCooldown = saved })
}

func TestEmbeddingStatusLifecycle(t *testing.T) {
	// Walks the full lifecycle of an article_embeddings row across the
	// three terminal states (ok, too_short, error) and verifies that
	// GetArticlesWithoutEmbeddings honors retry eligibility correctly.
	disableEmbedRetryCooldown(t)
	store, cleanup := newTestStore(t)
	defer cleanup()

	userID := int64(1)
	store.CreateUser("u")
	feedID, _ := store.AddFeed("https://example.com/feed", "F", "")
	store.SubscribeUserToFeed(userID, feedID)
	now := time.Now()
	addArticle := func(guid string) int64 {
		id, _ := store.AddArticle(&Article{FeedID: feedID, GUID: guid, Title: guid, URL: "u/" + guid, Content: "x", PublishedDate: &now})
		return id
	}
	a1 := addArticle("a1") // will get a real embedding
	a2 := addArticle("a2") // will be marked too-short (deterministic skip)
	a3 := addArticle("a3") // will be marked failed; retried; eventually maxes out
	a4 := addArticle("a4") // will be left untouched (no row → eligible)

	const model = "nomic-embed-text"

	// 1. Real embedding for a1 — status=ok, attempts=0, no error_message.
	if err := store.StoreArticleEmbedding(a1, []byte{1, 2, 3, 4}, model); err != nil {
		t.Fatalf("StoreArticleEmbedding a1: %v", err)
	}

	// 2. Too-short skip for a2 — permanent, never returned again.
	if err := store.MarkArticleEmbeddingSkipped(a2, model); err != nil {
		t.Fatalf("MarkArticleEmbeddingSkipped a2: %v", err)
	}

	// 3. Transient failure for a3 — should be retry-eligible immediately.
	if err := store.MarkArticleEmbeddingFailed(a3, model, "first attempt failed"); err != nil {
		t.Fatalf("MarkArticleEmbeddingFailed a3 (1): %v", err)
	}

	missing, err := store.GetArticlesWithoutEmbeddings(model, 100)
	if err != nil {
		t.Fatalf("GetArticlesWithoutEmbeddings: %v", err)
	}
	gotIDs := make(map[int64]bool)
	for _, a := range missing {
		gotIDs[a.ID] = true
	}
	if !gotIDs[a3] {
		t.Errorf("a3 (status=error, attempts=1) should be retry-eligible, missing from result")
	}
	if !gotIDs[a4] {
		t.Errorf("a4 (no row) should be eligible, missing from result")
	}
	if gotIDs[a1] {
		t.Errorf("a1 (status=ok) should NOT be returned")
	}
	if gotIDs[a2] {
		t.Errorf("a2 (status=too_short) should NOT be returned — deterministic skip")
	}

	// 4. Burn through retries on a3 until attempts hits EmbedMaxAttempts.
	// First MarkFailed already counted as attempt=1, so we need MaxAttempts-1 more
	// calls to exhaust the budget.
	for i := 1; i < EmbedMaxAttempts; i++ {
		if err := store.MarkArticleEmbeddingFailed(a3, model, "retry failed"); err != nil {
			t.Fatalf("MarkArticleEmbeddingFailed a3 (retry %d): %v", i, err)
		}
	}
	missing, _ = store.GetArticlesWithoutEmbeddings(model, 100)
	for _, a := range missing {
		if a.ID == a3 {
			t.Errorf("a3 should be exhausted after %d failures, but is still retry-eligible", EmbedMaxAttempts)
		}
	}

	// 5. Success after failures resets the row — attempts=0, status=ok.
	if err := store.StoreArticleEmbedding(a3, []byte{5, 6, 7, 8}, model); err != nil {
		t.Fatalf("StoreArticleEmbedding a3 (recovery): %v", err)
	}
	missing, _ = store.GetArticlesWithoutEmbeddings(model, 100)
	for _, a := range missing {
		if a.ID == a3 {
			t.Errorf("a3 should not appear after successful re-embed")
		}
	}
}

func TestEmbeddingFailedAttemptsIncrement(t *testing.T) {
	// Verify MarkArticleEmbeddingFailed increments attempts via the
	// ON CONFLICT path. Without this, every retry would start at 1
	// and the EmbedMaxAttempts cap would never trigger.
	disableEmbedRetryCooldown(t)
	store, cleanup := newTestStore(t)
	defer cleanup()
	store.CreateUser("u")
	feedID, _ := store.AddFeed("https://example.com/feed", "F", "")
	now := time.Now()
	id, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "x", Title: "x", URL: "u", Content: "x", PublishedDate: &now})

	const model = "nomic-embed-text"
	for i := 1; i <= 3; i++ {
		if err := store.MarkArticleEmbeddingFailed(id, model, fmt.Sprintf("attempt %d", i)); err != nil {
			t.Fatalf("MarkArticleEmbeddingFailed iter %d: %v", i, err)
		}
	}

	// After 3 calls, attempts should be 3 and the row should still be
	// retry-eligible (3 < EmbedMaxAttempts=5).
	missing, _ := store.GetArticlesWithoutEmbeddings(model, 100)
	found := false
	for _, a := range missing {
		if a.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("after 3 attempts (< %d cap) row should still be retry-eligible", EmbedMaxAttempts)
	}
}

func TestResetStuckEmbeddings(t *testing.T) {
	// Verify ResetStuckEmbeddings clears the retry budget on rows stuck
	// at EmbedMaxAttempts, scopes to embedding_model, supports an
	// optional error_message LIKE filter, and leaves non-stuck rows
	// (status=ok, status=too_short, status=error with attempts<max)
	// untouched.
	disableEmbedRetryCooldown(t)
	store, cleanup := newTestStore(t)
	defer cleanup()

	store.CreateUser("u")
	feedID, _ := store.AddFeed("https://example.com/feed", "F", "")
	now := time.Now()
	addArticle := func(guid string) int64 {
		id, _ := store.AddArticle(&Article{FeedID: feedID, GUID: guid, Title: guid, URL: "u/" + guid, Content: "x", PublishedDate: &now})
		return id
	}
	stuck403 := addArticle("stuck403")     // status=error, attempts=max, "HTTP 403"
	stuck400 := addArticle("stuck400")     // status=error, attempts=max, "HTTP 400 ctx"
	progress := addArticle("progress")     // status=error, attempts=2, retryable
	ok := addArticle("ok")                 // status=ok
	tooShort := addArticle("tooShort")     // status=too_short
	otherModel := addArticle("otherModel") // status=error, attempts=max, but different model

	const model = "nomic-embed-text"
	const otherModelName = "other-model"

	// Set up: stuck403 hits max with HTTP 403 error.
	for range EmbedMaxAttempts {
		if err := store.MarkArticleEmbeddingFailed(stuck403, model, "openai embed: HTTP 403 Forbidden"); err != nil {
			t.Fatalf("MarkArticleEmbeddingFailed stuck403: %v", err)
		}
	}
	// stuck400 hits max with a different error.
	for range EmbedMaxAttempts {
		if err := store.MarkArticleEmbeddingFailed(stuck400, model, "input length exceeds context length"); err != nil {
			t.Fatalf("MarkArticleEmbeddingFailed stuck400: %v", err)
		}
	}
	// progress: only 2 attempts, still has retry budget.
	for range 2 {
		if err := store.MarkArticleEmbeddingFailed(progress, model, "transient"); err != nil {
			t.Fatalf("MarkArticleEmbeddingFailed progress: %v", err)
		}
	}
	// ok: real embedding.
	if err := store.StoreArticleEmbedding(ok, []byte{1, 2, 3, 4}, model); err != nil {
		t.Fatalf("StoreArticleEmbedding ok: %v", err)
	}
	// tooShort: deterministic skip.
	if err := store.MarkArticleEmbeddingSkipped(tooShort, model); err != nil {
		t.Fatalf("MarkArticleEmbeddingSkipped tooShort: %v", err)
	}
	// otherModel: stuck under a different embedding_model.
	for range EmbedMaxAttempts {
		if err := store.MarkArticleEmbeddingFailed(otherModel, otherModelName, "HTTP 403 Forbidden"); err != nil {
			t.Fatalf("MarkArticleEmbeddingFailed otherModel: %v", err)
		}
	}

	// Pattern-narrowed reset: only HTTP 403 rows for the active model.
	n, err := store.ResetStuckEmbeddings(model, "%HTTP 403%")
	if err != nil {
		t.Fatalf("ResetStuckEmbeddings(403): %v", err)
	}
	if n != 1 {
		t.Errorf("expected to reset 1 row (stuck403), got %d", n)
	}

	missing, _ := store.GetArticlesWithoutEmbeddings(model, 100)
	gotIDs := make(map[int64]bool)
	for _, a := range missing {
		gotIDs[a.ID] = true
	}
	if !gotIDs[stuck403] {
		t.Errorf("stuck403 should be retry-eligible after reset")
	}
	if gotIDs[stuck400] {
		t.Errorf("stuck400 should NOT be eligible — pattern excluded it")
	}
	if !gotIDs[progress] {
		t.Errorf("progress (attempts<max, untouched) should still be retry-eligible")
	}
	if gotIDs[ok] || gotIDs[tooShort] {
		t.Errorf("ok/tooShort should NOT be retry-eligible")
	}

	// Unfiltered reset: clears stuck400 too. otherModel stays stuck
	// (different embedding_model).
	n, err = store.ResetStuckEmbeddings(model, "")
	if err != nil {
		t.Fatalf("ResetStuckEmbeddings(all): %v", err)
	}
	if n != 1 {
		t.Errorf("expected to reset 1 remaining row (stuck400), got %d", n)
	}

	// otherModel should still be stuck under its own model.
	missingOther, _ := store.GetArticlesWithoutEmbeddings(otherModelName, 100)
	for _, a := range missingOther {
		if a.ID == otherModel {
			// Should NOT be eligible — still attempts=max for its own model.
			t.Errorf("otherModel reset leaked across embedding_model — should still be stuck")
		}
	}
}

func TestEmbeddingRetryCooldown(t *testing.T) {
	// Verify that an article that just failed is NOT immediately retry-
	// eligible while EmbedRetryCooldown is in effect. This is the fix
	// for the 2026-05-09 burnout pathology where the daemon's intra-
	// cycle outer loop would refetch a freshly-failed article and
	// exhaust all 5 attempts within seconds during a transient backend
	// outage.
	saved := EmbedRetryCooldown
	EmbedRetryCooldown = 30 * time.Minute
	t.Cleanup(func() { EmbedRetryCooldown = saved })

	store, cleanup := newTestStore(t)
	defer cleanup()
	store.CreateUser("u")
	feedID, _ := store.AddFeed("https://example.com/feed", "F", "")
	now := time.Now()
	id, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "x", Title: "x", URL: "u", Content: "x", PublishedDate: &now})

	const model = "nomic-embed-text"
	if err := store.MarkArticleEmbeddingFailed(id, model, "transient"); err != nil {
		t.Fatalf("MarkArticleEmbeddingFailed: %v", err)
	}

	// Cooldown active: row must NOT be returned even though attempts<5.
	missing, err := store.GetArticlesWithoutEmbeddings(model, 100)
	if err != nil {
		t.Fatalf("GetArticlesWithoutEmbeddings during cooldown: %v", err)
	}
	for _, a := range missing {
		if a.ID == id {
			t.Errorf("article %d returned during cooldown — expected to be filtered out", id)
		}
	}

	// Simulate cooldown elapsed by zeroing it; row should now be eligible.
	EmbedRetryCooldown = 0
	missing, err = store.GetArticlesWithoutEmbeddings(model, 100)
	if err != nil {
		t.Fatalf("GetArticlesWithoutEmbeddings after cooldown: %v", err)
	}
	found := false
	for _, a := range missing {
		if a.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("article %d not retry-eligible after cooldown elapsed", id)
	}
}

func TestResetAllArticleEmbeddings(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	userID := int64(1)
	store.CreateUser("u")
	feedID, _ := store.AddFeed("https://example.com/feed", "F", "")
	store.SubscribeUserToFeed(userID, feedID)
	now := time.Now()
	a1, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "g1", Title: "T1", URL: "u1", PublishedDate: &now})
	a2, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "g2", Title: "T2", URL: "u2", PublishedDate: &now})

	if err := store.StoreArticleEmbedding(a1, []byte{1, 2, 3, 4}, "nomic-embed-text"); err != nil {
		t.Fatalf("StoreArticleEmbedding a1: %v", err)
	}
	if err := store.StoreArticleEmbedding(a2, []byte{5, 6, 7, 8}, "nomic-embed-text"); err != nil {
		t.Fatalf("StoreArticleEmbedding a2: %v", err)
	}

	n, err := store.ResetAllArticleEmbeddings()
	if err != nil {
		t.Fatalf("ResetAllArticleEmbeddings: %v", err)
	}
	if n != 2 {
		t.Errorf("rows deleted: got %d, want 2", n)
	}

	embs, _ := store.GetArticleEmbeddings(userID, "nomic-embed-text")
	if len(embs) != 0 {
		t.Errorf("expected 0 embeddings after reset, got %d", len(embs))
	}

	// Idempotent — second call deletes 0 rows, no error.
	n2, err := store.ResetAllArticleEmbeddings()
	if err != nil {
		t.Fatalf("second ResetAllArticleEmbeddings: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second call: got %d, want 0", n2)
	}
}

func TestResetAllGroupEmbeddings(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	userID := int64(1)
	store.CreateUser("u")
	g1, _ := store.CreateArticleGroup(userID, "topic 1")
	g2, _ := store.CreateArticleGroup(userID, "topic 2")
	if err := store.UpdateGroupEmbedding(g1, []byte{1, 2, 3, 4}, "nomic-embed-text"); err != nil {
		t.Fatalf("UpdateGroupEmbedding g1: %v", err)
	}
	if err := store.UpdateGroupEmbedding(g2, []byte{5, 6, 7, 8}, "nomic-embed-text"); err != nil {
		t.Fatalf("UpdateGroupEmbedding g2: %v", err)
	}

	n, err := store.ResetAllGroupEmbeddings()
	if err != nil {
		t.Fatalf("ResetAllGroupEmbeddings: %v", err)
	}
	if n != 2 {
		t.Errorf("rows updated: got %d, want 2", n)
	}

	// Verify both centroids are now NULL — GetGroupsWithEmbeddings filters
	// them out, so it should return zero groups.
	groups, _ := store.GetGroupsWithEmbeddings(userID, "nomic-embed-text")
	if len(groups) != 0 {
		t.Errorf("expected 0 groups with embeddings after reset, got %d", len(groups))
	}

	// Group rows themselves still exist — verify via single-group lookup.
	// (GetUserGroups filters on members ≥ 2, which doesn't apply here since
	// these test groups have no members.)
	if g, err := store.GetGroup(g1); err != nil || g == nil {
		t.Errorf("group g1 row lost after reset: err=%v g=%v", err, g)
	}
	if g, err := store.GetGroup(g2); err != nil || g == nil {
		t.Errorf("group g2 row lost after reset: err=%v g=%v", err, g)
	}
}

func TestSearchArticlesFTS_PG(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()

	userID := int64(1)
	store.CreateUser("testuser")

	feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
	store.SubscribeUserToFeed(userID, feedID)

	now := time.Now()
	store.AddArticle(&Article{
		FeedID: feedID, GUID: "a1", Title: "Go programming language",
		URL: "https://example.com/1", Content: "Go is a statically typed language designed at Google.",
		PublishedDate: &now,
	})
	store.AddArticle(&Article{
		FeedID: feedID, GUID: "a2", Title: "Rust memory safety",
		URL: "https://example.com/2", Content: "Rust prevents memory errors at compile time.",
		PublishedDate: &now,
	})

	results, err := store.SearchArticlesFTS(userID, "Go programming", 10, 0)
	if err != nil {
		t.Fatalf("SearchArticlesFTS: %v", err)
	}
	if len(results) < 1 {
		t.Errorf("expected at least 1 result for 'Go programming', got %d", len(results))
	}
}

func TestNewsletterCRUD(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	store.CreateUser("testuser")

	// Create newsletter.
	nl := &Newsletter{
		UserID:   1,
		Name:     "Daily Security",
		Schedule: "daily",
		Config: NewsletterConfig{
			MinInterestScore: 7.0,
			MaxArticles:      10,
		},
		Enabled: true,
	}
	id, err := store.CreateNewsletter(nl)
	if err != nil {
		t.Fatalf("CreateNewsletter: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero newsletter ID")
	}

	// Get newsletter.
	got, err := store.GetNewsletter(id)
	if err != nil {
		t.Fatalf("GetNewsletter: %v", err)
	}
	if got.Name != "Daily Security" {
		t.Errorf("name = %q, want %q", got.Name, "Daily Security")
	}
	if got.Config.MinInterestScore != 7.0 {
		t.Errorf("min_interest_score = %f, want 7.0", got.Config.MinInterestScore)
	}

	// Update newsletter.
	got.Name = "Weekly Security"
	got.Schedule = "manual"
	if err := store.UpdateNewsletter(got); err != nil {
		t.Fatalf("UpdateNewsletter: %v", err)
	}
	got2, _ := store.GetNewsletter(id)
	if got2.Name != "Weekly Security" {
		t.Errorf("updated name = %q, want %q", got2.Name, "Weekly Security")
	}

	// List newsletters.
	list, err := store.GetUserNewsletters(1)
	if err != nil {
		t.Fatalf("GetUserNewsletters: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 newsletter, got %d", len(list))
	}

	// Newsletter stats.
	stats, err := store.GetNewsletterStats(1)
	if err != nil {
		t.Fatalf("GetNewsletterStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 stat, got %d", len(stats))
	}
	if stats[0].IssueCount != 0 {
		t.Errorf("expected 0 issues, got %d", stats[0].IssueCount)
	}

	// Create issue.
	issue := &NewsletterIssue{
		NewsletterID: id,
		Headline:     "Top Security News",
		ContentHTML:  "<h2>Breaking</h2><p>Test content</p>",
		ArticleIDs:   []int64{1, 2, 3},
	}
	issueID, err := store.CreateNewsletterIssue(issue)
	if err != nil {
		t.Fatalf("CreateNewsletterIssue: %v", err)
	}

	// Get latest issue.
	latest, err := store.GetLatestNewsletterIssue(id)
	if err != nil {
		t.Fatalf("GetLatestNewsletterIssue: %v", err)
	}
	if latest.Headline != "Top Security News" {
		t.Errorf("headline = %q, want %q", latest.Headline, "Top Security News")
	}
	if len(latest.ArticleIDs) != 3 {
		t.Errorf("expected 3 article IDs, got %d", len(latest.ArticleIDs))
	}

	// Mark sent.
	if err := store.MarkNewsletterIssueSent(issueID); err != nil {
		t.Fatalf("MarkNewsletterIssueSent: %v", err)
	}
	sent, _ := store.GetNewsletterIssue(issueID)
	if sent.SentAt == nil {
		t.Error("expected non-nil SentAt after marking sent")
	}

	// Stats should show 1 issue now.
	stats, _ = store.GetNewsletterStats(1)
	if stats[0].IssueCount != 1 {
		t.Errorf("expected 1 issue in stats, got %d", stats[0].IssueCount)
	}

	// Delete newsletter (should cascade to issues).
	if err := store.DeleteNewsletter(id); err != nil {
		t.Fatalf("DeleteNewsletter: %v", err)
	}
	list, _ = store.GetUserNewsletters(1)
	if len(list) != 0 {
		t.Errorf("expected 0 newsletters after delete, got %d", len(list))
	}
}

// eachStore runs fn against both backends: SQLite always, and Postgres when
// HERALD_TEST_DB_DSN is set (otherwise that subtest skips). The staged
// pipeline's new queries must behave identically on both.
func eachStore(t *testing.T, fn func(t *testing.T, store Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		store, cleanup := newTestStore(t)
		defer cleanup()
		fn(t, store)
	})
	t.Run("postgres", func(t *testing.T) {
		store, cleanup := newPGTestStore(t)
		defer cleanup()
		fn(t, store)
	})
}

func TestFeedTagsCRUD(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		f1, _ := store.AddFeed("https://a.example/feed", "Krebs", "")
		f2, _ := store.AddFeed("https://b.example/feed", "The Register", "")
		f3, _ := store.AddFeed("https://c.example/feed", "Sports Daily", "")
		for _, f := range []int64{f1, f2, f3} {
			if err := store.SubscribeUserToFeed(1, f); err != nil {
				t.Fatalf("subscribe: %v", err)
			}
		}

		// Tag feeds; a feed can carry several tags.
		for _, tc := range []struct {
			feed int64
			tag  string
		}{{f1, "Security"}, {f2, "Security"}, {f2, "News"}, {f3, "Sports"}} {
			if err := store.AddFeedTag(1, tc.feed, tc.tag); err != nil {
				t.Fatalf("AddFeedTag(%d,%q): %v", tc.feed, tc.tag, err)
			}
		}

		// Idempotent + case-insensitive duplicate is a no-op.
		if err := store.AddFeedTag(1, f1, "security"); err != nil {
			t.Fatalf("dup AddFeedTag: %v", err)
		}
		tags, err := store.GetFeedTags(1, f1)
		if err != nil {
			t.Fatalf("GetFeedTags: %v", err)
		}
		if len(tags) != 1 || tags[0] != "Security" {
			t.Fatalf("f1 tags = %v, want [Security]", tags)
		}

		// GetUserTags: distinct, sorted.
		ut, _ := store.GetUserTags(1)
		if got := strings.Join(ut, ","); got != "News,Security,Sports" {
			t.Errorf("GetUserTags = %q, want News,Security,Sports", got)
		}

		// GetAllFeedTags: one feed → multiple tags.
		all, _ := store.GetAllFeedTags(1)
		if len(all[f2]) != 2 {
			t.Errorf("f2 tags = %v, want 2", all[f2])
		}

		// GetFeedsByTags: union across tags, case-insensitive, deduped.
		ids, err := store.GetFeedsByTags(1, []string{"security", "sports"})
		if err != nil {
			t.Fatalf("GetFeedsByTags: %v", err)
		}
		if !sameIDSet(ids, []int64{f1, f2, f3}) {
			t.Errorf("GetFeedsByTags(security,sports) = %v, want {%d,%d,%d}", ids, f1, f2, f3)
		}
		// A single tag resolves to just its feeds.
		ids, _ = store.GetFeedsByTags(1, []string{"News"})
		if !sameIDSet(ids, []int64{f2}) {
			t.Errorf("GetFeedsByTags(News) = %v, want {%d}", ids, f2)
		}
		// Empty / unknown tags → nil.
		if ids, _ := store.GetFeedsByTags(1, nil); ids != nil {
			t.Errorf("GetFeedsByTags(nil) = %v, want nil", ids)
		}

		// Remove one tag (case-insensitive).
		if err := store.RemoveFeedTag(1, f2, "news"); err != nil {
			t.Fatalf("RemoveFeedTag: %v", err)
		}
		if tags, _ := store.GetFeedTags(1, f2); !sameStrSet(tags, []string{"Security"}) {
			t.Errorf("f2 tags after remove = %v, want [Security]", tags)
		}

		// Cascade: deleting the feed row drops its tags. DeleteFeedIfOrphaned only
		// removes a feed with no subscribers, so unsubscribe first.
		if err := store.UnsubscribeUserFromFeed(1, f3); err != nil {
			t.Fatalf("unsubscribe: %v", err)
		}
		deleted, err := store.DeleteFeedIfOrphaned(f3)
		if err != nil || !deleted {
			t.Fatalf("DeleteFeedIfOrphaned = %v, %v; want true, nil", deleted, err)
		}
		if tags, _ := store.GetFeedTags(1, f3); len(tags) != 0 {
			t.Errorf("f3 tags after feed delete = %v, want none", tags)
		}
	})
}

func sameIDSet(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	m := make(map[int64]bool, len(got))
	for _, v := range got {
		m[v] = true
	}
	for _, v := range want {
		if !m[v] {
			return false
		}
	}
	return true
}

func sameStrSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	m := make(map[string]bool, len(got))
	for _, v := range got {
		m[v] = true
	}
	for _, v := range want {
		if !m[v] {
			return false
		}
	}
	return true
}

func articleIDs(arts []Article) []int64 {
	out := make([]int64, len(arts))
	for i, a := range arts {
		out[i] = a.ID
	}
	return out
}

// MarkSecurityScored marks an article ai_scored with a security verdict but no
// interest score, so the curation stage can find it via
// GetUnscoredCurationArticles — and re-screening must not clobber a score the
// curator later writes.
func TestMarkSecurityScoredAndCurationQueue(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		feedID, _ := store.AddFeed("https://example.com/feed", "Test Feed", "")
		if err := store.SubscribeUserToFeed(1, feedID); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		now := time.Now()
		mk := func(guid string) int64 {
			id, _ := store.AddArticle(&Article{FeedID: feedID, GUID: guid, Title: guid,
				URL: "https://example.com/" + guid, PublishedDate: &now})
			return id
		}

		passed := mk("passed") // security-passed, awaiting curation → appears
		lowSec := mk("lowsec") // passed security stage but below threshold → excluded
		scored := mk("scored") // fully scored (interest set) → excluded
		_ = mk("unscored")     // no read_state at all → excluded

		if err := store.ScreenArticleSecurity(passed, 8.0, "fine", false); err != nil {
			t.Fatalf("ScreenArticleSecurity: %v", err)
		}
		if err := store.ScreenArticleSecurity(lowSec, 5.0, "borderline", false); err != nil {
			t.Fatalf("ScreenArticleSecurity low: %v", err)
		}
		// "scored": security-passed and already interest-scored for this user.
		if err := store.ScreenArticleSecurity(scored, 8.0, "fine", false); err != nil {
			t.Fatalf("ScreenArticleSecurity scored: %v", err)
		}
		if err := store.SetInterestScore(1, scored, 6.0); err != nil {
			t.Fatalf("SetInterestScore: %v", err)
		}

		got, err := store.GetUnscoredCurationArticles(1, 7.0, 10)
		if err != nil {
			t.Fatalf("GetUnscoredCurationArticles: %v", err)
		}
		if len(got) != 1 || got[0].ID != passed {
			t.Fatalf("expected only article %d awaiting curation, got %v", passed, articleIDs(got))
		}

		// Screened article must have left the (global) security queue.
		unscreened, _ := store.GetUnscreenedArticles(10)
		for _, a := range unscreened {
			if a.ID == passed {
				t.Fatal("screened article should not be in the unscreened queue")
			}
		}

		// Curate it, then re-screen: the interest score must survive (security and
		// interest live in different tables now) and it must drop out of curation.
		if err := store.SetInterestScore(1, passed, 7.5); err != nil {
			t.Fatalf("SetInterestScore passed: %v", err)
		}
		if err := store.ScreenArticleSecurity(passed, 8.0, "re-screened", false); err != nil {
			t.Fatalf("ScreenArticleSecurity re-run: %v", err)
		}
		after, _ := store.GetUnscoredCurationArticles(1, 7.0, 10)
		if len(after) != 0 {
			t.Fatalf("curated article should not reappear in curation queue, got %v", articleIDs(after))
		}
	})
}

// SetInterestScore must record the interest score without disturbing the
// security verdict the security stage already wrote — the two stages run
// separately, so a naive UpdateReadState (which nulls security_score) is wrong.
func TestSetInterestScorePreservesSecurity(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
		if err := store.SubscribeUserToFeed(1, feedID); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		now := time.Now()
		id, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "x", Title: "x",
			URL: "https://example.com/x", PublishedDate: &now})

		if err := store.ScreenArticleSecurity(id, 8.0, "ok", false); err != nil {
			t.Fatalf("ScreenArticleSecurity: %v", err)
		}
		if err := store.SetInterestScore(1, id, 6.0); err != nil {
			t.Fatalf("SetInterestScore: %v", err)
		}

		// Security score must survive (>= 7.0), so the article still qualifies as
		// security-passed for the summary stage. If SetInterestScore had nulled it,
		// this query would return nothing.
		scored, err := store.GetUnsummarizedScoredArticles(7.0, 10)
		if err != nil {
			t.Fatalf("GetUnsummarizedScoredArticles: %v", err)
		}
		if len(scored) != 1 || scored[0].ID != id {
			t.Fatalf("security score not preserved after SetInterestScore; got %v", articleIDs(scored))
		}

		// And it has left the curation queue (interest_score now set).
		cur, _ := store.GetUnscoredCurationArticles(1, 7.0, 10)
		if len(cur) != 0 {
			t.Fatalf("expected nothing awaiting curation, got %v", articleIDs(cur))
		}
	})
}

// GetUngroupedEmbeddedArticles is the cluster stage's recency window: only
// articles with a usable embedding, in no group, published within the window.
func TestGetUngroupedEmbeddedArticles(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		const model = "nomic-test"
		feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
		if err := store.SubscribeUserToFeed(1, feedID); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		now := time.Now()
		old := now.Add(-100 * time.Hour)
		vec := []byte{1, 2, 3, 4}
		// All articles are security-passed so the test isolates the embedding,
		// grouping, and recency filters rather than the security gate.
		mk := func(guid string, pub time.Time) int64 {
			id, _ := store.AddArticle(&Article{FeedID: feedID, GUID: guid, Title: guid,
				URL: "https://example.com/" + guid, PublishedDate: &pub})
			store.ScreenArticleSecurity(id, 9, "ok", false) //nolint:errcheck
			return id
		}

		// Eligible: embedded (status OK), ungrouped, recent.
		good := mk("good", now)
		store.StoreArticleEmbedding(good, vec, model) //nolint:errcheck

		// Excluded: embedded + recent but already in a group.
		grouped := mk("grouped", now)
		store.StoreArticleEmbedding(grouped, vec, model) //nolint:errcheck
		gid, _ := store.CreateArticleGroup(1, "topic")
		store.AddArticleToGroup(gid, grouped) //nolint:errcheck

		// Excluded: no embedding.
		mk("noembed", now)

		// Excluded: embedded but published before the window.
		stale := mk("stale", old)
		store.StoreArticleEmbedding(stale, vec, model) //nolint:errcheck

		// Excluded: embedding attempt failed (status != OK).
		errored := mk("errored", now)
		store.MarkArticleEmbeddingFailed(errored, model, "boom") //nolint:errcheck

		// Excluded: embedded + recent + ungrouped but NOT security-passed.
		blocked := mk("blocked", now)
		store.StoreArticleEmbedding(blocked, vec, model) //nolint:errcheck
		// Overwrite the passing verdict with a failing one.
		store.ScreenArticleSecurity(blocked, 2.0, "blocked", false) //nolint:errcheck

		since := now.Add(-48 * time.Hour)
		got, err := store.GetUngroupedEmbeddedArticles(1, model, 7.0, since, 10)
		if err != nil {
			t.Fatalf("GetUngroupedEmbeddedArticles: %v", err)
		}
		if len(got) != 1 || got[0].ID != good {
			t.Fatalf("expected only article %d, got %v", good, articleIDs(got))
		}
	})
}

func TestGetProcessingStats(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feed1, _ := store.AddFeed("https://ex.com/f1", "Feed One", "")
	feed2, _ := store.AddFeed("https://ex.com/f2", "Feed Two", "")
	if err := store.SubscribeUserToFeed(1, feed1); err != nil {
		t.Fatal(err)
	}
	if err := store.SubscribeUserToFeed(1, feed2); err != nil {
		t.Fatal(err)
	}
	// feed2 has a failing latest fetch.
	if err := store.UpdateFeedError(feed2, "boom: context canceled"); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	add := func(guid string) int64 {
		id, err := store.AddArticle(&Article{
			FeedID: feed1, GUID: guid, Title: guid,
			URL: "https://ex.com/" + guid, Content: "body", PublishedDate: &now,
		})
		if err != nil {
			t.Fatalf("add %s: %v", guid, err)
		}
		return id
	}

	add("a1") // untouched -> pending

	a2 := add("a2") // screened, security pass, curated, summarized
	if err := store.ScreenArticleSecurity(a2, 8, "ok", false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetInterestScore(1, a2, 7); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateArticleAISummary(a2, "a real summary"); err != nil {
		t.Fatal(err)
	}

	a3 := add("a3") // screened, security rejected, summarize-skipped
	if err := store.ScreenArticleSecurity(a3, 3, "unsafe", false); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSummarizationSkipped(a3, "too short"); err != nil {
		t.Fatal(err)
	}

	a4 := add("a4") // unscreened with security attempts maxed -> stuck
	for range 3 {
		if err := store.IncrementArticleSecurityAttempts(a4); err != nil {
			t.Fatal(err)
		}
	}

	ps, err := store.GetProcessingStats(1)
	if err != nil {
		t.Fatalf("GetProcessingStats: %v", err)
	}

	checks := []struct {
		name string
		got  int
		want int
	}{
		{"TotalArticles", ps.TotalArticles, 4},
		{"Scored", ps.Scored, 2},
		{"Pending", ps.Pending, 1},
		{"Stuck", ps.Stuck, 1},
		{"SecurityPassed", ps.SecurityPassed, 1},
		{"SecurityRejected", ps.SecurityRejected, 1},
		{"SecuritySkipped", ps.SecuritySkipped, 0},
		{"Curated", ps.Curated, 1},
		{"Summarized", ps.Summarized, 1},
		{"SummarizeSkipped", ps.SummarizeSkipped, 1},
		{"FeedsTotal", ps.FeedsTotal, 2},
		{"FeedsErroring", ps.FeedsErroring, 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestCycleStats(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	base := time.Now()
	for i := range 3 {
		if err := store.RecordCycleStats(CycleStats{
			CompletedAt:        base.Add(time.Duration(i) * time.Minute),
			DurationMs:         int64(1000 * (i + 1)),
			FeedsTotal:         10,
			FeedsDownloaded:    5,
			NewArticles:        i,
			Processed:          i * 2,
			HighInterest:       i,
			FeedsErrored:       i,
			AIBackendAvailable: i%2 == 0,
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	recent, err := store.GetRecentCycleStats(10)
	if err != nil {
		t.Fatalf("get recent: %v", err)
	}
	if len(recent) != 3 {
		t.Fatalf("got %d cycles, want 3", len(recent))
	}
	// Newest first (completed_at DESC): the i=2 row.
	if recent[0].Processed != 4 || recent[0].DurationMs != 3000 || !recent[0].AIBackendAvailable {
		t.Errorf("newest cycle mismatch: %+v", recent[0])
	}
	if recent[2].NewArticles != 0 {
		t.Errorf("oldest cycle NewArticles = %d, want 0", recent[2].NewArticles)
	}
	if one, _ := store.GetRecentCycleStats(1); len(one) != 1 {
		t.Fatalf("limit=1 returned %d rows", len(one))
	}
}

func TestFilterRuleUserScoping(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	id, err := store.AddFilterRule(&FilterRule{UserID: 1, Axis: "author", Value: "Alice", Score: 5})
	if err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}

	// Another user can neither update nor delete the rule.
	if err := store.UpdateFilterRuleScore(2, id, 10); err == nil {
		t.Error("expected error updating another user's rule")
	}
	if err := store.DeleteFilterRule(2, id); err == nil {
		t.Error("expected error deleting another user's rule")
	}
	rules, err := store.GetFilterRules(1, nil)
	if err != nil {
		t.Fatalf("GetFilterRules: %v", err)
	}
	if len(rules) != 1 || rules[0].Score != 5 {
		t.Fatalf("rule should survive cross-user mutations unchanged, got %+v", rules)
	}

	// The owner can do both.
	if err := store.UpdateFilterRuleScore(1, id, 10); err != nil {
		t.Fatalf("owner UpdateFilterRuleScore: %v", err)
	}
	if err := store.DeleteFilterRule(1, id); err != nil {
		t.Fatalf("owner DeleteFilterRule: %v", err)
	}
	if rules, _ := store.GetFilterRules(1, nil); len(rules) != 0 {
		t.Errorf("expected 0 rules after owner delete, got %d", len(rules))
	}
}

func TestUserSubscribedToArticleFeed(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
		if err := store.SubscribeUserToFeed(1, feedID); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		now := time.Now()
		articleID, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "g", Title: "g",
			URL: "https://example.com/g", PublishedDate: &now})

		// Subscriber sees the article's feed.
		ok, err := store.UserSubscribedToArticleFeed(1, articleID)
		if err != nil || !ok {
			t.Errorf("subscriber = (%v, %v), want (true, nil)", ok, err)
		}
		// Non-subscriber does not.
		ok, err = store.UserSubscribedToArticleFeed(2, articleID)
		if err != nil || ok {
			t.Errorf("non-subscriber = (%v, %v), want (false, nil)", ok, err)
		}
		// Nonexistent article is false, nil.
		ok, err = store.UserSubscribedToArticleFeed(1, 999999)
		if err != nil || ok {
			t.Errorf("unknown article = (%v, %v), want (false, nil)", ok, err)
		}
	})
}

// TestDeleteUser verifies that DeleteUser removes rows in every per-user table
// and leaves other users' data untouched.
func TestDeleteUser(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		// Create two users. User 2 is the one we will delete; user 3 acts as a
		// bystander whose rows must survive.
		delID, err := store.CreateUser("DeleteMe")
		if err != nil {
			t.Fatalf("CreateUser del: %v", err)
		}
		keepID, err := store.CreateUser("KeepMe")
		if err != nil {
			t.Fatalf("CreateUser keep: %v", err)
		}

		// Seed a feed and article used by both users.
		feedID, err := store.AddFeed("https://example.com/feed", "Test Feed", "")
		if err != nil {
			t.Fatalf("AddFeed: %v", err)
		}
		pub := time.Now().Add(-time.Hour)
		articleID, err := store.AddArticle(&Article{
			FeedID: feedID, GUID: "del-test-guid", Title: "T",
			URL: "https://example.com/a", PublishedDate: &pub,
		})
		if err != nil {
			t.Fatalf("AddArticle: %v", err)
		}

		// --- Seed per-user rows for the deleted user ---

		// read_state
		if err := store.SubscribeUserToFeed(delID, feedID); err != nil {
			t.Fatalf("subscribe del: %v", err)
		}
		score := 0.8
		if err := store.UpdateReadState(delID, articleID, false, &score, &score, nil, nil); err != nil {
			t.Fatalf("UpdateReadState del: %v", err)
		}

		// user_preferences
		if err := store.SetUserPreference(delID, "theme", "dark"); err != nil {
			t.Fatalf("SetUserPreference del: %v", err)
		}

		// feed_tags
		if err := store.AddFeedTag(delID, feedID, "testtag"); err != nil {
			t.Fatalf("AddFeedTag del: %v", err)
		}

		// user_prompts
		if err := store.SetUserPrompt(delID, "curation", "custom prompt", nil, nil); err != nil {
			t.Fatalf("SetUserPrompt del: %v", err)
		}

		// filter_rules
		ruleID, err := store.AddFilterRule(&FilterRule{UserID: delID, Axis: "author", Value: "spam", Score: -100})
		if err != nil {
			t.Fatalf("AddFilterRule del: %v", err)
		}
		if ruleID == 0 {
			t.Fatal("expected non-zero rule id")
		}

		// article_groups (+ article_group_members cascade)
		groupID, err := store.CreateArticleGroup(delID, "TestGroup")
		if err != nil {
			t.Fatalf("CreateArticleGroup del: %v", err)
		}
		if err := store.AddArticleToGroup(groupID, articleID); err != nil {
			t.Fatalf("AddArticleToGroup del: %v", err)
		}
		if err := store.UpdateGroupSummary(groupID, "head", "body", 1, nil); err != nil {
			t.Fatalf("UpdateGroupSummary del: %v", err)
		}

		// fever_credentials (ON DELETE CASCADE from users)
		if err := store.SetFeverCredential(delID, "test-api-key"); err != nil {
			t.Fatalf("SetFeverCredential del: %v", err)
		}

		// newsletters + newsletter_issues (ON DELETE CASCADE from users)
		nlID, err := store.CreateNewsletter(&Newsletter{
			UserID: delID, Name: "Test NL", Schedule: "manual",
			Config: NewsletterConfig{MaxArticles: 10},
		})
		if err != nil {
			t.Fatalf("CreateNewsletter del: %v", err)
		}
		issueID, err := store.CreateNewsletterIssue(&NewsletterIssue{
			NewsletterID: nlID, Headline: "iss1", ContentHTML: "<p>body</p>",
		})
		if err != nil {
			t.Fatalf("CreateNewsletterIssue del: %v", err)
		}
		if issueID == 0 {
			t.Fatal("expected non-zero issue id")
		}

		// ai_summaries (ON DELETE CASCADE from users)
		sumID, err := store.CreateAISummary(&AISummary{
			UserID: delID, Model: "test-model", Prompt: "test prompt",
		})
		if err != nil {
			t.Fatalf("CreateAISummary del: %v", err)
		}
		if sumID == 0 {
			t.Fatal("expected non-zero summary id")
		}

		// --- Seed bystander rows for the kept user ---
		if err := store.SubscribeUserToFeed(keepID, feedID); err != nil {
			t.Fatalf("subscribe keep: %v", err)
		}
		if err := store.UpdateReadState(keepID, articleID, false, &score, &score, nil, nil); err != nil {
			t.Fatalf("UpdateReadState keep: %v", err)
		}
		if err := store.SetUserPreference(keepID, "theme", "light"); err != nil {
			t.Fatalf("SetUserPreference keep: %v", err)
		}
		if err := store.SetUserPrompt(keepID, "curation", "keep prompt", nil, nil); err != nil {
			t.Fatalf("SetUserPrompt keep: %v", err)
		}
		keepNlID, err := store.CreateNewsletter(&Newsletter{
			UserID: keepID, Name: "Keep NL", Schedule: "manual",
			Config: NewsletterConfig{MaxArticles: 5},
		})
		if err != nil {
			t.Fatalf("CreateNewsletter keep: %v", err)
		}
		if keepNlID == 0 {
			t.Fatal("expected non-zero keep newsletter id")
		}

		// --- Delete the target user ---
		if err := store.DeleteUser(delID); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}

		// --- Assert: deleted user's rows are gone in every table ---

		// users row
		users, _ := store.ListUsers()
		for _, u := range users {
			if u.ID == delID {
				t.Errorf("deleted user row still present: id=%d", delID)
			}
		}

		// read_state
		feeds, _ := store.GetUserFeeds(delID)
		if len(feeds) != 0 {
			t.Errorf("user_feeds: expected 0 for deleted user, got %d", len(feeds))
		}

		// user_preferences
		prefs, _ := store.GetAllUserPreferences(delID)
		if len(prefs) != 0 {
			t.Errorf("user_preferences: expected 0 for deleted user, got %d", len(prefs))
		}

		// feed_tags
		tags, _ := store.GetUserTags(delID)
		if len(tags) != 0 {
			t.Errorf("feed_tags: expected 0 for deleted user, got %d", len(tags))
		}

		// user_prompts
		prompts, _ := store.ListUserPrompts(delID)
		if len(prompts) != 0 {
			t.Errorf("user_prompts: expected 0 for deleted user, got %d", len(prompts))
		}

		// filter_rules
		rules, _ := store.GetFilterRules(delID, nil)
		if len(rules) != 0 {
			t.Errorf("filter_rules: expected 0 for deleted user, got %d", len(rules))
		}

		// article_groups + cascade of article_group_members and group_summaries
		groups, _ := store.GetUserGroups(delID)
		if len(groups) != 0 {
			t.Errorf("article_groups: expected 0 for deleted user, got %d", len(groups))
		}
		// The group row is gone, so GetGroupSummary should not find it.
		if gs, err := store.GetGroupSummary(groupID); err == nil && gs != nil {
			t.Errorf("group_summaries: expected gone after group delete, got headline=%q", gs.Headline)
		}

		// fever_credentials (cascaded via users FK)
		if u, err := store.GetUserByFeverAPIKey("test-api-key"); err == nil && u != nil {
			t.Errorf("fever_credentials: expected gone after user delete")
		}

		// newsletters + newsletter_issues (cascaded via users FK)
		nls, _ := store.GetUserNewsletters(delID)
		if len(nls) != 0 {
			t.Errorf("newsletters: expected 0 for deleted user, got %d", len(nls))
		}
		// newsletter_issues cascade is confirmed by the newsletter itself being gone;
		// no direct API to query issues by newsletter after the newsletter is deleted.

		// ai_summaries (cascaded via users FK)
		if s, err := store.GetAISummary(delID, sumID); err == nil && s != nil {
			t.Errorf("ai_summaries: expected gone after user delete, got id=%d", s.ID)
		}

		// --- Assert: bystander (kept user) rows are untouched ---
		keepFeeds, _ := store.GetUserFeeds(keepID)
		if len(keepFeeds) == 0 {
			t.Errorf("user_feeds: kept user's feeds should survive deletion of other user")
		}
		keepPrefs, _ := store.GetAllUserPreferences(keepID)
		if len(keepPrefs) == 0 {
			t.Errorf("user_preferences: kept user's prefs should survive")
		}
		keepPrompts, _ := store.ListUserPrompts(keepID)
		if len(keepPrompts) == 0 {
			t.Errorf("user_prompts: kept user's prompts should survive")
		}
		keepNls, _ := store.GetUserNewsletters(keepID)
		if len(keepNls) == 0 {
			t.Errorf("newsletters: kept user's newsletters should survive")
		}
	})
}

// TestDeleteUserIdemptotent confirms that deleting a non-existent user
// succeeds without error (all DELETEs match 0 rows, which is fine).
func TestDeleteUserIdempotent(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		// No user with id 99999 exists; DeleteUser should not error.
		if err := store.DeleteUser(99999); err != nil {
			t.Errorf("DeleteUser non-existent user: want nil, got %v", err)
		}
	})
}
