package storage

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	return store, func() { store.Close() }
}

func TestNewStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
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

	// Update read state
	interestScore := 8.5
	securityScore := 9.0
	if err := store.UpdateReadState(articleID, true, &interestScore, &securityScore); err != nil {
		t.Fatalf("UpdateReadState failed: %v", err)
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
		store.UpdateReadState(articleID, false, &score, &secScore)
	}

	// Get articles with score >= 8.0
	articles, scores, err := store.GetArticlesByInterestScore(8.0, 10, 0)
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
	old := time.Now().Add(-30 * 24 * time.Hour)    // 30 days old

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
	store.UpdateReadState(art1, false, &rawScore, &secScore)
	store.UpdateReadState(art2, false, &rawScore, &secScore)

	articles, scores, err := store.GetArticlesByInterestScore(8.0, 10, 0)
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

	articles, err := store.GetUnreadArticlesForUser(1, 10, 0)
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
	if err := store.UpdateGroupSummary(groupID, "Summary of the group", 3, &maxScore); err != nil {
		t.Fatalf("UpdateGroupSummary failed: %v", err)
	}

	gs, err := store.GetGroupSummary(groupID)
	if err != nil {
		t.Fatalf("GetGroupSummary failed: %v", err)
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
	if err := store.SetUserPrompt(1, "security", "custom security prompt", &temp); err != nil {
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
