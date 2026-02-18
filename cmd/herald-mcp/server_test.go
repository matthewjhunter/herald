package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/matthewjhunter/herald"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <link>https://example.com</link>
    <description>A test feed</description>
    <item>
      <title>Test Article</title>
      <link>https://example.com/article-1</link>
      <guid>article-1</guid>
      <description>Article body text.</description>
    </item>
  </channel>
</rss>`

// --- Test helpers ---

func newTestSessionWithUserID(t *testing.T, userID int64) (*heraldServer, *mcp.ClientSession) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	engine, err := herald.NewEngine(herald.EngineConfig{DBPath: dbPath})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	hs := &heraldServer{engine: engine, userID: userID}
	server := newMCPServer(hs)

	serverT, clientT := mcp.NewInMemoryTransports()
	_, err = server.Connect(context.Background(), serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	return hs, session
}

func newTestSession(t *testing.T) (*heraldServer, *mcp.ClientSession) {
	t.Helper()
	return newTestSessionWithUserID(t, 1)
}

// feedServer returns a test HTTP server serving valid RSS.
func feedServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, testRSS)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// callTool calls an MCP tool and returns both the result and any protocol error.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	return session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
}

// mustCallTool calls an MCP tool and fatals on protocol errors.
func mustCallTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := callTool(t, session, name, args)
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return result
}

// expectError calls a tool and asserts either a protocol error or tool error.
func expectError(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) {
	t.Helper()
	result, err := callTool(t, session, name, args)
	if err == nil && (result == nil || !result.IsError) {
		t.Fatalf("expected error calling %s", name)
	}
}

// resultText extracts the first text content from a tool result.
func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("no content in result")
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("first content is %T, want *mcp.TextContent", result.Content[0])
	}
	return tc.Text
}

// subscribeFeed subscribes to the test feed server and returns the feed ID.
func subscribeFeed(t *testing.T, session *mcp.ClientSession, feedURL string) int64 {
	t.Helper()
	result := mustCallTool(t, session, "feed_subscribe", map[string]any{
		"url": feedURL,
	})
	if result.IsError {
		t.Fatalf("subscribe error: %s", resultText(t, result))
	}

	result = mustCallTool(t, session, "feeds_list", map[string]any{})
	text := resultText(t, result)
	var feeds []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &feeds); err != nil {
		t.Fatalf("unmarshal feeds: %v", err)
	}
	if len(feeds) == 0 {
		t.Fatal("no feeds after subscribe")
	}
	return feeds[0].ID
}

// --- Protocol tests ---

func TestInitialize(t *testing.T) {
	// SDK handles initialization during Connect. If we get here, it worked.
	_, session := newTestSession(t)
	_ = session
}

func TestToolsList(t *testing.T) {
	_, session := newTestSession(t)
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	expected := []string{
		"articles_unread", "articles_get", "articles_mark_read",
		"feeds_list", "feed_subscribe", "feed_unsubscribe", "feed_rename",
		"article_groups", "article_group_get", "feed_stats", "poll_now",
		"preferences_get", "preference_set",
		"prompts_list", "prompt_get", "prompt_set", "prompt_reset",
		"briefing", "article_star",
		"user_register", "user_list",
		"filter_rules_list", "filter_rule_add", "filter_rule_update",
		"filter_rule_delete", "feed_metadata",
	}
	if len(result.Tools) != len(expected) {
		t.Fatalf("got %d tools, want %d", len(result.Tools), len(expected))
	}
	names := make(map[string]bool)
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("missing tool: %s", name)
		}
	}
}

// --- Tool tests ---

func TestFeedsListEmpty(t *testing.T) {
	_, session := newTestSession(t)
	result := mustCallTool(t, session, "feeds_list", map[string]any{})
	if result.IsError {
		t.Fatal("unexpected tool error")
	}
}

func TestFeedSubscribeAndList(t *testing.T) {
	_, session := newTestSession(t)
	ts := feedServer(t)

	// Subscribe
	result := mustCallTool(t, session, "feed_subscribe", map[string]any{
		"url":   ts.URL + "/feed.xml",
		"title": "Test Feed",
	})
	if result.IsError {
		t.Fatalf("subscribe error: %s", resultText(t, result))
	}

	// List should show the feed
	result = mustCallTool(t, session, "feeds_list", map[string]any{})
	text := resultText(t, result)
	var feeds []struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(text), &feeds); err != nil {
		t.Fatalf("unmarshal feeds: %v", err)
	}
	if len(feeds) != 1 {
		t.Fatalf("got %d feeds, want 1", len(feeds))
	}
	if feeds[0].Title != "Test Feed" {
		t.Errorf("feed title = %q, want %q", feeds[0].Title, "Test Feed")
	}
}

func TestFeedSubscribeMissingURL(t *testing.T) {
	_, session := newTestSession(t)
	expectError(t, session, "feed_subscribe", map[string]any{})
}

func TestFeedRename(t *testing.T) {
	_, session := newTestSession(t)
	ts := feedServer(t)
	feedID := subscribeFeed(t, session, ts.URL+"/feed.xml")

	// Rename
	result := mustCallTool(t, session, "feed_rename", map[string]any{
		"feed_id": feedID,
		"title":   "Renamed Feed",
	})
	if result.IsError {
		t.Fatalf("rename error: %s", resultText(t, result))
	}

	// Verify
	result = mustCallTool(t, session, "feeds_list", map[string]any{})
	text := resultText(t, result)
	var updated []struct {
		Title string `json:"title"`
	}
	json.Unmarshal([]byte(text), &updated)
	if len(updated) == 0 {
		t.Fatal("no feeds after rename")
	}
	if updated[0].Title != "Renamed Feed" {
		t.Errorf("title = %q, want %q", updated[0].Title, "Renamed Feed")
	}
}

func TestFeedRenameMissingParams(t *testing.T) {
	_, session := newTestSession(t)
	expectError(t, session, "feed_rename", map[string]any{"feed_id": 1})
	expectError(t, session, "feed_rename", map[string]any{"title": "Foo"})
}

func TestFeedUnsubscribe(t *testing.T) {
	_, session := newTestSession(t)
	ts := feedServer(t)
	feedID := subscribeFeed(t, session, ts.URL+"/feed.xml")

	// Unsubscribe
	result := mustCallTool(t, session, "feed_unsubscribe", map[string]any{
		"feed_id": feedID,
	})
	if result.IsError {
		t.Fatalf("unsubscribe error: %s", resultText(t, result))
	}

	// List should be empty
	result = mustCallTool(t, session, "feeds_list", map[string]any{})
	text := resultText(t, result)
	var remaining []struct{}
	json.Unmarshal([]byte(text), &remaining)
	if len(remaining) != 0 {
		t.Errorf("expected 0 feeds after unsubscribe, got %d", len(remaining))
	}
}

func TestArticlesUnreadEmpty(t *testing.T) {
	_, session := newTestSession(t)
	result := mustCallTool(t, session, "articles_unread", map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
}

func TestArticlesUnreadWithFeed(t *testing.T) {
	_, session := newTestSession(t)
	ts := feedServer(t)
	subscribeFeed(t, session, ts.URL+"/feed.xml")

	result := mustCallTool(t, session, "articles_unread", map[string]any{
		"limit": 10,
	})
	if result.IsError {
		t.Fatalf("articles_unread error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	var articles []struct {
		Title string `json:"title"`
	}
	json.Unmarshal([]byte(text), &articles)
	if len(articles) == 0 {
		t.Fatal("expected at least one article after subscribing")
	}
	if articles[0].Title != "Test Article" {
		t.Errorf("article title = %q, want %q", articles[0].Title, "Test Article")
	}
}

func TestArticlesGetAndMarkRead(t *testing.T) {
	_, session := newTestSession(t)
	ts := feedServer(t)
	subscribeFeed(t, session, ts.URL+"/feed.xml")

	// Get article list to find an ID
	result := mustCallTool(t, session, "articles_unread", map[string]any{"limit": 1})
	text := resultText(t, result)
	var articles []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &articles)
	if len(articles) == 0 {
		t.Fatal("no articles to test with")
	}
	articleID := articles[0].ID

	// Get full article
	result = mustCallTool(t, session, "articles_get", map[string]any{
		"article_id": articleID,
	})
	if result.IsError {
		t.Fatalf("articles_get error: %s", resultText(t, result))
	}

	// Mark read
	result = mustCallTool(t, session, "articles_mark_read", map[string]any{
		"article_id": articleID,
	})
	if result.IsError {
		t.Fatalf("articles_mark_read error: %s", resultText(t, result))
	}
}

func TestArticlesGetMissingID(t *testing.T) {
	_, session := newTestSession(t)
	expectError(t, session, "articles_get", map[string]any{})
}

func TestArticlesMarkReadMissingID(t *testing.T) {
	_, session := newTestSession(t)
	expectError(t, session, "articles_mark_read", map[string]any{})
}

func TestArticleGroupsEmpty(t *testing.T) {
	_, session := newTestSession(t)
	result := mustCallTool(t, session, "article_groups", map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
}

func TestArticleGroupGetMissingID(t *testing.T) {
	_, session := newTestSession(t)
	expectError(t, session, "article_group_get", map[string]any{})
}

func TestFeedStats(t *testing.T) {
	_, session := newTestSession(t)
	result := mustCallTool(t, session, "feed_stats", map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
}

func TestFeedStatsWithFeed(t *testing.T) {
	_, session := newTestSession(t)
	ts := feedServer(t)
	subscribeFeed(t, session, ts.URL+"/feed.xml")

	result := mustCallTool(t, session, "feed_stats", map[string]any{})
	if result.IsError {
		t.Fatalf("feed_stats error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	var stats struct {
		Feeds []struct {
			TotalArticles int `json:"total_articles"`
		} `json:"feeds"`
	}
	json.Unmarshal([]byte(text), &stats)
	if len(stats.Feeds) == 0 {
		t.Fatal("expected feed stats")
	}
	if stats.Feeds[0].TotalArticles == 0 {
		t.Error("expected non-zero article count")
	}
}

func TestUnknownTool(t *testing.T) {
	_, session := newTestSession(t)
	_, err := callTool(t, session, "nonexistent_tool", map[string]any{})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestPollNowDisabled(t *testing.T) {
	_, session := newTestSession(t)
	// poller is nil — poll_now should return an error
	result := mustCallTool(t, session, "poll_now", map[string]any{})
	if !result.IsError {
		t.Fatal("expected error when polling is disabled")
	}
	text := resultText(t, result)
	if text == "" {
		t.Fatal("expected error message")
	}
}

func TestPollNowEnabled(t *testing.T) {
	hs, session := newTestSession(t)
	ts := feedServer(t)

	// Subscribe to a feed so poll has something to fetch
	subscribeFeed(t, session, ts.URL+"/feed.xml")

	// Attach a poller
	p := newPoller(hs.engine, hs.userID, 10*time.Minute, 8.0)
	hs.poller = p

	result := mustCallTool(t, session, "poll_now", map[string]any{})
	if result.IsError {
		t.Fatalf("poll_now error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	var pollResult struct {
		FeedsTotal      int `json:"feeds_total"`
		FeedsDownloaded int `json:"feeds_downloaded"`
	}
	if err := json.Unmarshal([]byte(text), &pollResult); err != nil {
		t.Fatalf("unmarshal poll result: %v", err)
	}
	if pollResult.FeedsTotal == 0 {
		t.Error("expected non-zero feeds_total")
	}
}

func TestFeedUnsubscribeMissingID(t *testing.T) {
	_, session := newTestSession(t)
	expectError(t, session, "feed_unsubscribe", map[string]any{})
}

// --- Preferences tests ---

func TestPreferencesGetDefaults(t *testing.T) {
	_, session := newTestSession(t)
	result := mustCallTool(t, session, "preferences_get", map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	var prefs herald.UserPreferences
	if err := json.Unmarshal([]byte(text), &prefs); err != nil {
		t.Fatalf("unmarshal preferences: %v", err)
	}
	if prefs.NotifyWhen != "present" {
		t.Errorf("notify_when = %q, want %q", prefs.NotifyWhen, "present")
	}
	if prefs.NotifyMinScore != 7.0 {
		t.Errorf("notify_min_score = %v, want 7.0", prefs.NotifyMinScore)
	}
}

func TestPreferenceSetAndGet(t *testing.T) {
	_, session := newTestSession(t)

	// Set keywords
	result := mustCallTool(t, session, "preference_set", map[string]any{
		"key":   "keywords",
		"value": `["security","golang"]`,
	})
	if result.IsError {
		t.Fatalf("set keywords error: %s", resultText(t, result))
	}

	// Set interest_threshold
	result = mustCallTool(t, session, "preference_set", map[string]any{
		"key":   "interest_threshold",
		"value": "6.5",
	})
	if result.IsError {
		t.Fatalf("set threshold error: %s", resultText(t, result))
	}

	// Set notify_when
	result = mustCallTool(t, session, "preference_set", map[string]any{
		"key":   "notify_when",
		"value": "always",
	})
	if result.IsError {
		t.Fatalf("set notify_when error: %s", resultText(t, result))
	}

	// Verify via preferences_get
	result = mustCallTool(t, session, "preferences_get", map[string]any{})
	text := resultText(t, result)
	var prefs herald.UserPreferences
	if err := json.Unmarshal([]byte(text), &prefs); err != nil {
		t.Fatalf("unmarshal preferences: %v", err)
	}

	if len(prefs.Keywords) != 2 || prefs.Keywords[0] != "security" || prefs.Keywords[1] != "golang" {
		t.Errorf("keywords = %v, want [security golang]", prefs.Keywords)
	}
	if prefs.InterestThreshold != 6.5 {
		t.Errorf("interest_threshold = %v, want 6.5", prefs.InterestThreshold)
	}
	if prefs.NotifyWhen != "always" {
		t.Errorf("notify_when = %q, want %q", prefs.NotifyWhen, "always")
	}
}

func TestPreferenceSetValidation(t *testing.T) {
	_, session := newTestSession(t)

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"unknown key", "bogus_key", "value"},
		{"keywords not JSON", "keywords", "not-json"},
		{"threshold not number", "interest_threshold", "abc"},
		{"notify_when invalid", "notify_when", "never"},
		{"missing key", "", "value"},
		{"missing value", "keywords", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mustCallTool(t, session, "preference_set", map[string]any{
				"key":   tt.key,
				"value": tt.value,
			})
			if !result.IsError {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

// --- Prompt tests ---

func TestPromptsListDefaults(t *testing.T) {
	_, session := newTestSession(t)
	result := mustCallTool(t, session, "prompts_list", map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	var prompts []herald.PromptInfo
	if err := json.Unmarshal([]byte(text), &prompts); err != nil {
		t.Fatalf("unmarshal prompts: %v", err)
	}
	if len(prompts) != 4 {
		t.Fatalf("got %d prompt types, want 4", len(prompts))
	}

	for _, p := range prompts {
		if p.Status != "default" {
			t.Errorf("prompt %q status = %q, want %q", p.Type, p.Status, "default")
		}
	}
}

func TestPromptGetDefault(t *testing.T) {
	_, session := newTestSession(t)
	result := mustCallTool(t, session, "prompt_get", map[string]any{
		"prompt_type": "curation",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	var detail herald.PromptDetail
	if err := json.Unmarshal([]byte(text), &detail); err != nil {
		t.Fatalf("unmarshal prompt detail: %v", err)
	}
	if detail.Type != "curation" {
		t.Errorf("type = %q, want %q", detail.Type, "curation")
	}
	if detail.Template == "" {
		t.Error("expected non-empty default template")
	}
	if detail.IsCustom {
		t.Error("expected IsCustom = false for default prompt")
	}
}

func TestPromptSetAndGet(t *testing.T) {
	_, session := newTestSession(t)
	customTemplate := "Rate this article: {{.Title}}"
	temp := 0.5

	// Set custom prompt
	result := mustCallTool(t, session, "prompt_set", map[string]any{
		"prompt_type": "curation",
		"template":    customTemplate,
		"temperature": temp,
	})
	if result.IsError {
		t.Fatalf("prompt_set error: %s", resultText(t, result))
	}

	// Verify via prompt_get
	result = mustCallTool(t, session, "prompt_get", map[string]any{
		"prompt_type": "curation",
	})
	text := resultText(t, result)
	var detail herald.PromptDetail
	if err := json.Unmarshal([]byte(text), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if detail.Template != customTemplate {
		t.Errorf("template = %q, want %q", detail.Template, customTemplate)
	}
	if detail.Temperature != temp {
		t.Errorf("temperature = %v, want %v", detail.Temperature, temp)
	}
	if !detail.IsCustom {
		t.Error("expected IsCustom = true after setting")
	}

	// Verify prompts_list shows custom status
	result = mustCallTool(t, session, "prompts_list", map[string]any{})
	listText := resultText(t, result)
	var prompts []herald.PromptInfo
	json.Unmarshal([]byte(listText), &prompts)
	found := false
	for _, p := range prompts {
		if p.Type == "curation" && p.Status == "custom" {
			found = true
		}
	}
	if !found {
		t.Error("expected curation prompt to show status=custom in list")
	}
}

func TestPromptSetTemperatureOnly(t *testing.T) {
	_, session := newTestSession(t)
	temp := 1.5

	// Set only temperature (template should be preserved from default)
	result := mustCallTool(t, session, "prompt_set", map[string]any{
		"prompt_type": "summarization",
		"temperature": temp,
	})
	if result.IsError {
		t.Fatalf("prompt_set error: %s", resultText(t, result))
	}

	// Verify template is non-empty (default preserved) and temperature is set
	result = mustCallTool(t, session, "prompt_get", map[string]any{
		"prompt_type": "summarization",
	})
	text := resultText(t, result)
	var detail herald.PromptDetail
	json.Unmarshal([]byte(text), &detail)
	if detail.Template == "" {
		t.Error("expected non-empty template when only temperature was set")
	}
	if detail.Temperature != temp {
		t.Errorf("temperature = %v, want %v", detail.Temperature, temp)
	}
}

func TestPromptReset(t *testing.T) {
	_, session := newTestSession(t)

	// Set a custom prompt first
	result := mustCallTool(t, session, "prompt_set", map[string]any{
		"prompt_type": "curation",
		"template":    "custom template",
	})
	if result.IsError {
		t.Fatalf("prompt_set error: %s", resultText(t, result))
	}

	// Reset
	result = mustCallTool(t, session, "prompt_reset", map[string]any{
		"prompt_type": "curation",
	})
	if result.IsError {
		t.Fatalf("prompt_reset error: %s", resultText(t, result))
	}

	// Verify it's back to default
	result = mustCallTool(t, session, "prompt_get", map[string]any{
		"prompt_type": "curation",
	})
	text := resultText(t, result)
	var detail herald.PromptDetail
	json.Unmarshal([]byte(text), &detail)
	if detail.IsCustom {
		t.Error("expected IsCustom = false after reset")
	}
	if detail.Template == "custom template" {
		t.Error("expected template to revert to default after reset")
	}
}

func TestPromptSecurityBlocked(t *testing.T) {
	_, session := newTestSession(t)

	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"get security", "prompt_get", map[string]any{"prompt_type": "security"}},
		{"set security", "prompt_set", map[string]any{"prompt_type": "security", "template": "hack"}},
		{"reset security", "prompt_reset", map[string]any{"prompt_type": "security"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mustCallTool(t, session, tt.tool, tt.args)
			if !result.IsError {
				t.Fatalf("expected error when accessing security prompt via %s", tt.tool)
			}
		})
	}
}

func TestPromptGetUnknownType(t *testing.T) {
	_, session := newTestSession(t)
	result := mustCallTool(t, session, "prompt_get", map[string]any{
		"prompt_type": "nonexistent",
	})
	if !result.IsError {
		t.Fatal("expected error for unknown prompt type")
	}
}

func TestPromptSetMissingParams(t *testing.T) {
	_, session := newTestSession(t)

	// Missing prompt_type — caught by schema validation
	expectError(t, session, "prompt_set", map[string]any{"template": "foo"})

	// Neither template nor temperature — caught by handler
	result := mustCallTool(t, session, "prompt_set", map[string]any{
		"prompt_type": "curation",
	})
	if !result.IsError {
		t.Fatal("expected error when neither template nor temperature provided")
	}
}

// --- Briefing tests ---

func TestBriefingEmpty(t *testing.T) {
	_, session := newTestSession(t)
	result := mustCallTool(t, session, "briefing", map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	if text == "" {
		t.Fatal("expected a response message even with no articles")
	}
}

// --- Article star tests ---

func TestArticleStarAndUnstar(t *testing.T) {
	_, session := newTestSession(t)
	ts := feedServer(t)
	subscribeFeed(t, session, ts.URL+"/feed.xml")

	// Get an article ID
	result := mustCallTool(t, session, "articles_unread", map[string]any{"limit": 1})
	text := resultText(t, result)
	var articles []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &articles)
	if len(articles) == 0 {
		t.Fatal("no articles to test with")
	}
	articleID := articles[0].ID

	// Star
	result = mustCallTool(t, session, "article_star", map[string]any{
		"article_id": articleID,
		"starred":    true,
	})
	if result.IsError {
		t.Fatalf("star error: %s", resultText(t, result))
	}

	// Unstar
	result = mustCallTool(t, session, "article_star", map[string]any{
		"article_id": articleID,
		"starred":    false,
	})
	if result.IsError {
		t.Fatalf("unstar error: %s", resultText(t, result))
	}
}

func TestArticleStarMissingParams(t *testing.T) {
	_, session := newTestSession(t)

	// Missing article_id — caught by schema validation
	expectError(t, session, "article_star", map[string]any{"starred": true})

	// Missing starred (pointer type, optional in schema) — caught by handler
	result := mustCallTool(t, session, "article_star", map[string]any{
		"article_id": 1,
	})
	if !result.IsError {
		t.Fatal("expected error for missing starred")
	}
}

// --- User management tests ---

func TestUserRegister(t *testing.T) {
	_, session := newTestSession(t)

	result := mustCallTool(t, session, "user_register", map[string]any{
		"name": "alice",
	})
	if result.IsError {
		t.Fatalf("user_register error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	var user struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(text), &user); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if user.ID == 0 {
		t.Error("expected non-zero user ID")
	}
	if user.Name != "alice" {
		t.Errorf("name = %q, want %q", user.Name, "alice")
	}
}

func TestUserRegisterMissingName(t *testing.T) {
	_, session := newTestSession(t)
	expectError(t, session, "user_register", map[string]any{})
}

func TestUserRegisterDuplicate(t *testing.T) {
	_, session := newTestSession(t)

	result := mustCallTool(t, session, "user_register", map[string]any{"name": "bob"})
	if result.IsError {
		t.Fatalf("first register error: %s", resultText(t, result))
	}

	result = mustCallTool(t, session, "user_register", map[string]any{"name": "bob"})
	if !result.IsError {
		t.Fatal("expected error for duplicate name")
	}
}

func TestUserList(t *testing.T) {
	_, session := newTestSession(t)

	// Empty initially
	result := mustCallTool(t, session, "user_list", map[string]any{})
	if result.IsError {
		t.Fatalf("user_list error: %s", resultText(t, result))
	}
	text := resultText(t, result)
	var users []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(text), &users); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(users) != 0 {
		t.Fatalf("expected 0 users, got %d", len(users))
	}

	// Register two users
	mustCallTool(t, session, "user_register", map[string]any{"name": "alice"})
	mustCallTool(t, session, "user_register", map[string]any{"name": "bob"})

	result = mustCallTool(t, session, "user_list", map[string]any{})
	text = resultText(t, result)
	if err := json.Unmarshal([]byte(text), &users); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
}

// --- Speaker resolution tests ---

func TestSpeakerResolution(t *testing.T) {
	// Use a high default user ID (99) so it doesn't collide with
	// auto-increment IDs from user_register.
	_, session := newTestSessionWithUserID(t, 99)
	ts := feedServer(t)

	// Register "alice" as user (gets ID 1)
	result := mustCallTool(t, session, "user_register", map[string]any{"name": "alice"})
	if result.IsError {
		t.Fatalf("register error: %s", resultText(t, result))
	}

	// Subscribe a feed as alice (via speaker)
	result = mustCallTool(t, session, "feed_subscribe", map[string]any{
		"url":     ts.URL + "/feed.xml",
		"speaker": "alice",
	})
	if result.IsError {
		t.Fatalf("subscribe error: %s", resultText(t, result))
	}

	// Alice should see the feed
	result = mustCallTool(t, session, "feeds_list", map[string]any{
		"speaker": "alice",
	})
	text := resultText(t, result)
	var feeds []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &feeds)
	if len(feeds) != 1 {
		t.Fatalf("alice should have 1 feed, got %d", len(feeds))
	}

	// Default user (ID 99) should NOT see alice's feed
	result = mustCallTool(t, session, "feeds_list", map[string]any{})
	text = resultText(t, result)
	var defaultFeeds []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &defaultFeeds)
	if len(defaultFeeds) != 0 {
		t.Errorf("default user should have 0 feeds, got %d", len(defaultFeeds))
	}
}

func TestSpeakerFallback(t *testing.T) {
	_, session := newTestSession(t)

	// Unknown speaker should fall back to default user
	result := mustCallTool(t, session, "feeds_list", map[string]any{
		"speaker": "unknown_person",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
}

// --- Filter rules tests ---

func TestFilterRuleAddAndList(t *testing.T) {
	_, session := newTestSession(t)

	// Add a global rule
	result := mustCallTool(t, session, "filter_rule_add", map[string]any{
		"axis":  "author",
		"value": "Alice",
		"score": 5,
	})
	if result.IsError {
		t.Fatalf("filter_rule_add error: %s", resultText(t, result))
	}

	// List rules
	result = mustCallTool(t, session, "filter_rules_list", map[string]any{})
	if result.IsError {
		t.Fatalf("filter_rules_list error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	var rules []struct {
		ID    int64  `json:"id"`
		Axis  string `json:"axis"`
		Value string `json:"value"`
		Score int    `json:"score"`
	}
	if err := json.Unmarshal([]byte(text), &rules); err != nil {
		t.Fatalf("unmarshal rules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Axis != "author" || rules[0].Value != "Alice" || rules[0].Score != 5 {
		t.Errorf("rule mismatch: %+v", rules[0])
	}
}

func TestFilterRuleAddValidation(t *testing.T) {
	_, session := newTestSession(t)

	// Invalid axis
	result := mustCallTool(t, session, "filter_rule_add", map[string]any{
		"axis":  "invalid",
		"value": "x",
		"score": 1,
	})
	if !result.IsError {
		t.Fatal("expected error for invalid axis")
	}
}

func TestFilterRuleUpdate(t *testing.T) {
	_, session := newTestSession(t)

	// Add a rule
	result := mustCallTool(t, session, "filter_rule_add", map[string]any{
		"axis": "category", "value": "Security", "score": 3,
	})
	if result.IsError {
		t.Fatalf("add error: %s", resultText(t, result))
	}

	// Get the rule ID
	result = mustCallTool(t, session, "filter_rules_list", map[string]any{})
	text := resultText(t, result)
	var rules []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &rules)
	if len(rules) == 0 {
		t.Fatal("no rules to update")
	}

	// Update score
	result = mustCallTool(t, session, "filter_rule_update", map[string]any{
		"rule_id": rules[0].ID,
		"score":   10,
	})
	if result.IsError {
		t.Fatalf("update error: %s", resultText(t, result))
	}

	// Verify
	result = mustCallTool(t, session, "filter_rules_list", map[string]any{})
	text = resultText(t, result)
	var updated []struct {
		Score int `json:"score"`
	}
	json.Unmarshal([]byte(text), &updated)
	if len(updated) > 0 && updated[0].Score != 10 {
		t.Errorf("expected score 10, got %d", updated[0].Score)
	}
}

func TestFilterRuleDelete(t *testing.T) {
	_, session := newTestSession(t)

	// Add and delete
	mustCallTool(t, session, "filter_rule_add", map[string]any{
		"axis": "tag", "value": "golang", "score": 2,
	})

	result := mustCallTool(t, session, "filter_rules_list", map[string]any{})
	text := resultText(t, result)
	var rules []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &rules)
	if len(rules) == 0 {
		t.Fatal("no rules to delete")
	}

	result = mustCallTool(t, session, "filter_rule_delete", map[string]any{
		"rule_id": rules[0].ID,
	})
	if result.IsError {
		t.Fatalf("delete error: %s", resultText(t, result))
	}

	// Verify empty
	result = mustCallTool(t, session, "filter_rules_list", map[string]any{})
	text = resultText(t, result)
	json.Unmarshal([]byte(text), &rules)
	if len(rules) != 0 {
		t.Errorf("expected 0 rules after delete, got %d", len(rules))
	}
}

func TestFeedMetadata(t *testing.T) {
	_, session := newTestSession(t)
	ts := feedServer(t)
	feedID := subscribeFeed(t, session, ts.URL+"/feed.xml")

	result := mustCallTool(t, session, "feed_metadata", map[string]any{
		"feed_id": feedID,
	})
	if result.IsError {
		t.Fatalf("feed_metadata error: %s", resultText(t, result))
	}

	text := resultText(t, result)
	var meta struct {
		FeedID     int64    `json:"feed_id"`
		Authors    []string `json:"authors"`
		Categories []string `json:"categories"`
	}
	if err := json.Unmarshal([]byte(text), &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta.FeedID != feedID {
		t.Errorf("feed_id = %d, want %d", meta.FeedID, feedID)
	}
}

func TestFilterRuleAddMissingParams(t *testing.T) {
	_, session := newTestSession(t)

	// Missing axis
	expectError(t, session, "filter_rule_add", map[string]any{"value": "x", "score": 1})

	// Missing value
	expectError(t, session, "filter_rule_add", map[string]any{"axis": "author", "score": 1})

	// Missing score
	expectError(t, session, "filter_rule_add", map[string]any{"axis": "author", "value": "x"})
}

func TestFeedMetadataMissingID(t *testing.T) {
	_, session := newTestSession(t)
	expectError(t, session, "feed_metadata", map[string]any{})
}

func TestSpeakerOmitted(t *testing.T) {
	_, session := newTestSession(t)
	ts := feedServer(t)

	// Subscribe without speaker — should use default user
	result := mustCallTool(t, session, "feed_subscribe", map[string]any{
		"url": ts.URL + "/feed.xml",
	})
	if result.IsError {
		t.Fatalf("subscribe error: %s", resultText(t, result))
	}

	// List without speaker — should see the feed
	result = mustCallTool(t, session, "feeds_list", map[string]any{})
	text := resultText(t, result)
	var feeds []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &feeds)
	if len(feeds) != 1 {
		t.Fatalf("expected 1 feed, got %d", len(feeds))
	}
}
