package storage

import (
	"os"
	"testing"
	"time"
)

func TestNewStore(t *testing.T) {
	dbPath := "test.db"
	defer os.Remove(dbPath)

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
	dbPath := "test.db"
	defer os.Remove(dbPath)

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

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
	dbPath := "test.db"
	defer os.Remove(dbPath)

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

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
	dbPath := "test.db"
	defer os.Remove(dbPath)

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

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
	err = store.UpdateReadState(articleID, true, &interestScore, &securityScore)
	if err != nil {
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
	dbPath := "test.db"
	defer os.Remove(dbPath)

	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

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
	articles, scores, err := store.GetArticlesByInterestScore(8.0, 10)
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
