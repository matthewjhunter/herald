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

// subscribeDirect adds a feed and subscribes the user without HTTP validation.
// Used by tests that don't need a real feed URL.
func subscribeDirect(t *testing.T, engine *Engine, userID int64, url, title string) int64 {
	t.Helper()
	feedID, err := engine.store.AddFeed(url, title, "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := engine.store.SubscribeUserToFeed(userID, feedID); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}
	return feedID
}

func TestSubscribeAndGetFeeds(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Test Feed")

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
	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Test Feed")

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
	err = engine.MarkArticleRead(1, articleID)
	if err != nil {
		t.Fatalf("MarkArticleRead: %v", err)
	}
}

func TestGetArticleForUser(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Test Feed")

	now := time.Now()
	articleID, err := engine.store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "test-guid-foruser",
		Title:         "AI Summary Article",
		URL:           "https://example.com/article/ai",
		Content:       "Full content here.",
		Summary:       "RSS summary here.",
		PublishedDate: &now,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	// Without AI summary — AISummary should be empty
	article, err := engine.GetArticleForUser(1, articleID)
	if err != nil {
		t.Fatalf("GetArticleForUser: %v", err)
	}
	if article.Title != "AI Summary Article" {
		t.Errorf("title: got %q", article.Title)
	}
	if article.AISummary != "" {
		t.Errorf("expected empty AISummary, got %q", article.AISummary)
	}

	// Store an AI summary and verify it's returned
	engine.store.UpdateArticleAISummary(1, articleID, "This is the AI-generated summary.")

	article, err = engine.GetArticleForUser(1, articleID)
	if err != nil {
		t.Fatalf("GetArticleForUser with summary: %v", err)
	}
	if article.AISummary != "This is the AI-generated summary." {
		t.Errorf("AISummary: got %q", article.AISummary)
	}

	// Different user should not see user 1's summary
	article, err = engine.GetArticleForUser(2, articleID)
	if err != nil {
		t.Fatalf("GetArticleForUser user 2: %v", err)
	}
	if article.AISummary != "" {
		t.Errorf("expected empty AISummary for user 2, got %q", article.AISummary)
	}
}

func TestGetArticleForUserNotFound(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	_, err := engine.GetArticleForUser(1, 999)
	if err == nil {
		t.Fatal("expected error for non-existent article")
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
	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Test Feed")

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

func TestGetFeedStats(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	// Two feeds, user 1
	feed1 := subscribeDirect(t, engine, 1, "https://example.com/feed1.xml", "Feed One")
	feed2 := subscribeDirect(t, engine, 1, "https://example.com/feed2.xml", "Feed Two")

	now := time.Now()

	// Feed 1: 3 articles
	id1, _ := engine.store.AddArticle(&storage.Article{
		FeedID: feed1, GUID: "f1-1", Title: "Article 1",
		URL: "https://example.com/1", PublishedDate: &now,
	})
	engine.store.AddArticle(&storage.Article{
		FeedID: feed1, GUID: "f1-2", Title: "Article 2",
		URL: "https://example.com/2", PublishedDate: &now,
	})
	engine.store.AddArticle(&storage.Article{
		FeedID: feed1, GUID: "f1-3", Title: "Article 3",
		URL: "https://example.com/3", PublishedDate: &now,
	})

	// Feed 2: 2 articles
	id4, _ := engine.store.AddArticle(&storage.Article{
		FeedID: feed2, GUID: "f2-1", Title: "Article 4",
		URL: "https://example.com/4", PublishedDate: &now,
	})
	engine.store.AddArticle(&storage.Article{
		FeedID: feed2, GUID: "f2-2", Title: "Article 5",
		URL: "https://example.com/5", PublishedDate: &now,
	})

	// Mark one article read in feed 1
	engine.store.UpdateReadState(1, id1, true, nil, nil)

	// Summarize one article in feed 2
	engine.store.UpdateArticleAISummary(1, id4, "Summary of article 4")

	result, err := engine.GetFeedStats(1)
	if err != nil {
		t.Fatalf("GetFeedStats: %v", err)
	}

	if len(result.Feeds) != 2 {
		t.Fatalf("expected 2 feeds, got %d", len(result.Feeds))
	}

	// Feeds are ordered by title
	f1 := result.Feeds[0] // "Feed One"
	f2 := result.Feeds[1] // "Feed Two"

	if f1.FeedTitle != "Feed One" || f2.FeedTitle != "Feed Two" {
		t.Fatalf("unexpected feed order: %q, %q", f1.FeedTitle, f2.FeedTitle)
	}

	// Feed One: 3 total, 2 unread (1 read), 3 unsummarized
	if f1.TotalArticles != 3 {
		t.Errorf("feed1 total: got %d, want 3", f1.TotalArticles)
	}
	if f1.UnreadArticles != 2 {
		t.Errorf("feed1 unread: got %d, want 2", f1.UnreadArticles)
	}
	if f1.UnsummarizedArticles != 3 {
		t.Errorf("feed1 unsummarized: got %d, want 3", f1.UnsummarizedArticles)
	}

	// Feed Two: 2 total, 2 unread, 1 unsummarized
	if f2.TotalArticles != 2 {
		t.Errorf("feed2 total: got %d, want 2", f2.TotalArticles)
	}
	if f2.UnreadArticles != 2 {
		t.Errorf("feed2 unread: got %d, want 2", f2.UnreadArticles)
	}
	if f2.UnsummarizedArticles != 1 {
		t.Errorf("feed2 unsummarized: got %d, want 1", f2.UnsummarizedArticles)
	}

	// Totals: 5 total, 4 unread, 4 unsummarized
	if result.Total.TotalArticles != 5 {
		t.Errorf("total articles: got %d, want 5", result.Total.TotalArticles)
	}
	if result.Total.UnreadArticles != 4 {
		t.Errorf("total unread: got %d, want 4", result.Total.UnreadArticles)
	}
	if result.Total.UnsummarizedArticles != 4 {
		t.Errorf("total unsummarized: got %d, want 4", result.Total.UnsummarizedArticles)
	}
}

func TestGetFeedStatsEmpty(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	result, err := engine.GetFeedStats(1)
	if err != nil {
		t.Fatalf("GetFeedStats: %v", err)
	}
	if len(result.Feeds) != 0 {
		t.Errorf("expected 0 feeds, got %d", len(result.Feeds))
	}
	if result.Total.TotalArticles != 0 {
		t.Errorf("expected 0 total, got %d", result.Total.TotalArticles)
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

// --- Filter rules tests ---

func TestFilterRulesCRUD(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	userID := int64(1)

	// No rules initially
	rules, err := engine.GetFilterRules(userID, nil)
	if err != nil {
		t.Fatalf("GetFilterRules: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected 0 rules, got %d", len(rules))
	}

	// Add a rule
	rule := FilterRule{Axis: "author", Value: "Alice", Score: 5}
	id, err := engine.AddFilterRule(userID, rule)
	if err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero rule ID")
	}

	// Verify
	rules, _ = engine.GetFilterRules(userID, nil)
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Axis != "author" || rules[0].Value != "Alice" || rules[0].Score != 5 {
		t.Errorf("rule mismatch: %+v", rules[0])
	}

	// Update score
	if err := engine.UpdateFilterRule(id, 10); err != nil {
		t.Fatalf("UpdateFilterRule: %v", err)
	}
	rules, _ = engine.GetFilterRules(userID, nil)
	if rules[0].Score != 10 {
		t.Errorf("expected score 10, got %d", rules[0].Score)
	}

	// Delete
	if err := engine.DeleteFilterRule(id); err != nil {
		t.Fatalf("DeleteFilterRule: %v", err)
	}
	rules, _ = engine.GetFilterRules(userID, nil)
	if len(rules) != 0 {
		t.Errorf("expected 0 rules after delete, got %d", len(rules))
	}
}

func TestFilterRuleValidation(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	// Invalid axis
	_, err := engine.AddFilterRule(1, FilterRule{Axis: "bogus", Value: "x", Score: 1})
	if err == nil {
		t.Fatal("expected error for invalid axis")
	}

	// Empty value
	_, err = engine.AddFilterRule(1, FilterRule{Axis: "author", Value: "", Score: 1})
	if err == nil {
		t.Fatal("expected error for empty value")
	}

	// Valid axes
	for _, axis := range []string{"author", "category", "tag"} {
		_, err := engine.AddFilterRule(1, FilterRule{Axis: axis, Value: "test", Score: 1})
		if err != nil {
			t.Errorf("AddFilterRule(%s): %v", axis, err)
		}
	}
}

func TestGetFeedMetadata(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed", "Test")

	now := time.Now()
	a := &storage.Article{
		FeedID: feedID, GUID: "g1", Title: "A1",
		URL: "https://example.com/1", PublishedDate: &now,
	}
	articleID, err := engine.store.AddArticle(a)
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	engine.store.StoreArticleAuthors(articleID, []storage.ArticleAuthor{
		{Name: "Alice"}, {Name: "Bob"},
	})
	engine.store.StoreArticleCategories(articleID, []string{"Security", "Golang"})

	meta, err := engine.GetFeedMetadata(feedID)
	if err != nil {
		t.Fatalf("GetFeedMetadata: %v", err)
	}
	if len(meta.Authors) != 2 {
		t.Errorf("expected 2 authors, got %d", len(meta.Authors))
	}
	if len(meta.Categories) != 2 {
		t.Errorf("expected 2 categories, got %d", len(meta.Categories))
	}
}

func TestFilterThresholdPreference(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	// Default should be 0
	prefs, err := engine.GetPreferences(1)
	if err != nil {
		t.Fatalf("GetPreferences: %v", err)
	}
	if prefs.FilterThreshold != 0 {
		t.Errorf("default filter_threshold: got %d, want 0", prefs.FilterThreshold)
	}

	// Set it
	if err := engine.SetPreference(1, "filter_threshold", "5"); err != nil {
		t.Fatalf("SetPreference: %v", err)
	}

	prefs, _ = engine.GetPreferences(1)
	if prefs.FilterThreshold != 5 {
		t.Errorf("filter_threshold: got %d, want 5", prefs.FilterThreshold)
	}

	// Invalid value
	if err := engine.SetPreference(1, "filter_threshold", "abc"); err == nil {
		t.Fatal("expected error for non-integer filter_threshold")
	}
}
