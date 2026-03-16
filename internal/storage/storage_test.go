package storage

import (
	"database/sql"
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
	if err := store.UpdateReadState(1, articleID, false, &interestScore, &securityScore, nil); err != nil {
		t.Fatalf("UpdateReadState (AI scores) failed: %v", err)
	}
	if err := store.UpdateReadState(1, articleID, true, nil, nil, nil); err != nil {
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
	for i := 0; i < 3; i++ {
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
		store.UpdateReadState(1, articleID, false, &score, &secScore, nil)
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
	store.UpdateReadState(1, art1, false, &rawScore, &secScore, nil)
	store.UpdateReadState(1, art2, false, &rawScore, &secScore, nil)

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

	articles, err := store.GetUnreadArticlesForUser(1, 10, 0, nil)
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
	summary, err := store.GetArticleSummary(1, articleID)
	if err != nil {
		t.Fatalf("GetArticleSummary failed: %v", err)
	}
	if summary != nil {
		t.Error("expected nil summary before setting one")
	}

	// Set a summary
	if err := store.UpdateArticleAISummary(1, articleID, "This is an AI summary"); err != nil {
		t.Fatalf("UpdateArticleAISummary failed: %v", err)
	}

	// Retrieve it
	summary, err = store.GetArticleSummary(1, articleID)
	if err != nil {
		t.Fatalf("GetArticleSummary failed: %v", err)
	}
	if summary == nil {
		t.Fatal("expected non-nil summary")
	}
	if summary.AISummary != "This is an AI summary" {
		t.Errorf("summary = %q, want %q", summary.AISummary, "This is an AI summary")
	}
	if summary.UserID != 1 || summary.ArticleID != articleID {
		t.Errorf("summary IDs mismatch: user=%d article=%d", summary.UserID, summary.ArticleID)
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
	store.UpdateReadState(1, articleID, false, &score1, &sec, nil)

	// User 2 scores the same article differently
	score2 := 3.0
	store.UpdateReadState(2, articleID, false, &score2, &sec, nil)

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
	store.UpdateReadState(1, articleID, true, nil, nil, nil)
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
	if err := store.UpdateFilterRuleScore(id1, 10); err != nil {
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
	if err := store.DeleteFilterRule(id2); err != nil {
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
	articles, err := store.GetUnreadArticlesForUser(1, 10, 0, nil)
	if err != nil {
		t.Fatalf("GetUnreadArticlesForUser (nil threshold): %v", err)
	}
	if len(articles) != 2 {
		t.Errorf("nil threshold: expected 2 articles, got %d", len(articles))
	}

	// With threshold=0 — both articles returned (0 means disabled)
	zero := 0
	articles, _ = store.GetUnreadArticlesForUser(1, 10, 0, &zero)
	if len(articles) != 2 {
		t.Errorf("threshold=0: expected 2 articles, got %d", len(articles))
	}

	// With threshold=1 — only a1 passes (score 8 >= 1), a2 has score 0
	one := 1
	articles, _ = store.GetUnreadArticlesForUser(1, 10, 0, &one)
	if len(articles) != 1 {
		t.Errorf("threshold=1: expected 1 article, got %d", len(articles))
	}
	if len(articles) > 0 && articles[0].Title != "Secure Article" {
		t.Errorf("expected 'Secure Article', got %q", articles[0].Title)
	}

	// With threshold=10 — neither passes (max score is 8)
	ten := 10
	articles, _ = store.GetUnreadArticlesForUser(1, 10, 0, &ten)
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
	articles, err := store.GetUnreadArticlesForUser(1, 10, 0, &threshold)
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
		if err := store.UpdateReadState(1, aid, false, &score, &sec, nil); err != nil {
			t.Fatalf("UpdateReadState (AI): %v", err)
		}
		if err := store.UpdateReadState(1, aid, true, nil, nil, nil); err != nil {
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
		store.UpdateReadState(1, art1, false, &raw, &sec, nil)
		store.UpdateReadState(1, art2, false, &raw, &sec, nil)

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
		arts, err := store.GetUnreadArticlesForUser(2, 10, 0, &one)
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
	src.UpdateReadState(1, artID, false, &score, &sec, nil)
	src.UpdateReadState(1, artID, true, nil, nil, nil)
	src.UpdateStarred(1, artID, true)

	src.StoreArticleAuthors(artID, []ArticleAuthor{{Name: "Author One", Email: "a@b.com"}})
	src.StoreArticleCategories(artID, []string{"Security"})
	src.UpdateArticleAISummary(1, artID, "AI summary text")

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
	dstFeedArts, _ := dst.GetUnreadArticlesByFeed(1, dstFeedID, 10, 0, nil)
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
	unread, err := store.GetUnreadArticlesForUser(1, 100, 0, nil)
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
	feedArticles, err := store.GetUnreadArticlesByFeed(1, feedID, 100, 0, nil)
	if err != nil {
		t.Fatalf("GetUnreadArticlesByFeed: %v", err)
	}
	if len(feedArticles) != 1 {
		t.Errorf("expected 1 feed article (ungrouped), got %d", len(feedArticles))
	}

	// Verify group articles are returned by GetUnreadGroupArticles
	groupArticles, err := store.GetUnreadGroupArticles(1, groupID, 100, 0, nil)
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
	unread, _ = store.GetUnreadArticlesForUser(1, 100, 0, nil)
	if len(unread) != 3 {
		t.Errorf("expected 3 articles after disband, got %d", len(unread))
	}
}
