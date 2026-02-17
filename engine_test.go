package herald

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

func newTestEngine(t *testing.T) (*Engine, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	engine, err := NewEngine(EngineConfig{
		DBPath:        dbPath,
		OllamaBaseURL: "http://localhost:11434",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine, func() { engine.Close() }
}

func TestNewEngine(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	if engine.store == nil {
		t.Fatal("store is nil")
	}
	if engine.fetcher == nil {
		t.Fatal("fetcher is nil")
	}
	if engine.ai == nil {
		t.Fatal("ai processor is nil")
	}
}

func TestNewEngineDefaults(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	engine, err := NewEngine(EngineConfig{DBPath: dbPath})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer engine.Close()

	// Verify defaults were applied
	if engine.config.Ollama.BaseURL != "http://localhost:11434" {
		t.Errorf("default base URL: got %s", engine.config.Ollama.BaseURL)
	}
	if engine.config.Ollama.SecurityModel != "gemma3:4b" {
		t.Errorf("default security model: got %s", engine.config.Ollama.SecurityModel)
	}
	if engine.config.Ollama.CurationModel != "llama3" {
		t.Errorf("default curation model: got %s", engine.config.Ollama.CurationModel)
	}
	if engine.config.Thresholds.InterestScore != 8.0 {
		t.Errorf("default interest threshold: got %f", engine.config.Thresholds.InterestScore)
	}
	if engine.config.Thresholds.SecurityScore != 7.0 {
		t.Errorf("default security threshold: got %f", engine.config.Thresholds.SecurityScore)
	}
}

func TestSubscribeAndGetFeeds(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	err := engine.SubscribeFeed(1, "https://example.com/feed.xml", "Test Feed")
	if err != nil {
		t.Fatalf("SubscribeFeed: %v", err)
	}

	feeds, err := engine.GetUserFeeds(1)
	if err != nil {
		t.Fatalf("GetUserFeeds: %v", err)
	}
	if len(feeds) != 1 {
		t.Fatalf("expected 1 feed, got %d", len(feeds))
	}
	if feeds[0].URL != "https://example.com/feed.xml" {
		t.Errorf("feed URL: got %q", feeds[0].URL)
	}
	if feeds[0].Title != "Test Feed" {
		t.Errorf("feed title: got %q", feeds[0].Title)
	}
}

func TestGetUserFeedsEmpty(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feeds, err := engine.GetUserFeeds(1)
	if err != nil {
		t.Fatalf("GetUserFeeds: %v", err)
	}
	if len(feeds) != 0 {
		t.Errorf("expected 0 feeds, got %d", len(feeds))
	}
}

func TestGetUnreadArticlesEmpty(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	articles, err := engine.GetUnreadArticles(1, 10, 0)
	if err != nil {
		t.Fatalf("GetUnreadArticles: %v", err)
	}
	if len(articles) != 0 {
		t.Errorf("expected 0 articles, got %d", len(articles))
	}
}

func TestArticleLifecycle(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	// Add a feed and subscribe user
	err := engine.SubscribeFeed(1, "https://example.com/feed.xml", "Test Feed")
	if err != nil {
		t.Fatalf("SubscribeFeed: %v", err)
	}

	feeds, _ := engine.GetUserFeeds(1)
	feedID := feeds[0].ID

	// Insert an article directly via the store (simulates a fetch)
	now := time.Now()
	articleID, err := engine.store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "test-guid-1",
		Title:         "Test Article",
		URL:           "https://example.com/article/1",
		Content:       "This is a test article about security.",
		Summary:       "Test summary",
		Author:        "Test Author",
		PublishedDate: &now,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	// Get the article by ID
	article, err := engine.GetArticle(articleID)
	if err != nil {
		t.Fatalf("GetArticle: %v", err)
	}
	if article.Title != "Test Article" {
		t.Errorf("article title: got %q", article.Title)
	}
	if article.URL != "https://example.com/article/1" {
		t.Errorf("article URL: got %q", article.URL)
	}

	// Mark as read
	err = engine.MarkArticleRead(articleID)
	if err != nil {
		t.Fatalf("MarkArticleRead: %v", err)
	}
}

func TestGetArticleNotFound(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	_, err := engine.GetArticle(999)
	if err == nil {
		t.Fatal("expected error for non-existent article")
	}
}

func TestGroupLifecycle(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	// Subscribe and add articles
	err := engine.SubscribeFeed(1, "https://example.com/feed.xml", "Test Feed")
	if err != nil {
		t.Fatalf("SubscribeFeed: %v", err)
	}
	feeds, _ := engine.GetUserFeeds(1)
	feedID := feeds[0].ID

	now := time.Now()
	id1, _ := engine.store.AddArticle(&storage.Article{
		FeedID: feedID, GUID: "g1", Title: "Article One",
		URL: "https://example.com/1", PublishedDate: &now,
	})
	id2, _ := engine.store.AddArticle(&storage.Article{
		FeedID: feedID, GUID: "g2", Title: "Article Two",
		URL: "https://example.com/2", PublishedDate: &now,
	})

	// Create a group and add articles
	groupID, err := engine.store.CreateArticleGroup(1, "Test Topic")
	if err != nil {
		t.Fatalf("CreateArticleGroup: %v", err)
	}
	engine.store.AddArticleToGroup(groupID, id1)
	engine.store.AddArticleToGroup(groupID, id2)

	// Get user groups
	groups, err := engine.GetUserGroups(1)
	if err != nil {
		t.Fatalf("GetUserGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Topic != "Test Topic" {
		t.Errorf("group topic: got %q", groups[0].Topic)
	}

	// Get group articles
	group, err := engine.GetGroupArticles(groupID)
	if err != nil {
		t.Fatalf("GetGroupArticles: %v", err)
	}
	if len(group.Articles) != 2 {
		t.Errorf("expected 2 articles in group, got %d", len(group.Articles))
	}
	if group.Count != 2 {
		t.Errorf("expected count 2, got %d", group.Count)
	}
}

func TestGetGroupArticlesNotFound(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	group, err := engine.GetGroupArticles(999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty group is fine, not an error
	if len(group.Articles) != 0 {
		t.Errorf("expected 0 articles, got %d", len(group.Articles))
	}
}

func TestGenerateBriefingEmpty(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	briefing, err := engine.GenerateBriefing(1)
	if err != nil {
		t.Fatalf("GenerateBriefing: %v", err)
	}
	if briefing != "" {
		t.Errorf("expected empty briefing with no articles, got %q", briefing)
	}
}

func TestClose(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	_ = cleanup // don't use cleanup, test Close directly

	err := engine.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}
