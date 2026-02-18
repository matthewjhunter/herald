package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/matthewjhunter/herald"
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

func newTestServer(t *testing.T) *server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	engine, err := herald.NewEngine(herald.EngineConfig{DBPath: dbPath})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	return newServer(engine, 1)
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

// subscribeFeed subscribes to the test feed server and returns the feed ID.
func subscribeFeed(t *testing.T, srv *server, feedURL string) int64 {
	t.Helper()
	resp := srv.handleRequest(toolCall(1, "feed_subscribe", map[string]any{
		"url": feedURL,
	}))
	if resultIsError(t, resp) {
		t.Fatalf("subscribe error: %s", resultText(t, resp))
	}

	resp = srv.handleRequest(toolCall(2, "feeds_list", map[string]any{}))
	text := resultText(t, resp)
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

// rpc builds a jsonRPCRequest for testing.
func rpc(id int, method string, params any) jsonRPCRequest {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
	}
	idBytes, _ := json.Marshal(id)
	req.ID = idBytes
	if params != nil {
		p, _ := json.Marshal(params)
		req.Params = p
	}
	return req
}

// toolCall builds a tools/call request.
func toolCall(id int, name string, args any) jsonRPCRequest {
	return rpc(id, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
}

// resultText extracts the first text content from an MCP tool response.
func resultText(t *testing.T, resp jsonRPCResponse) string {
	t.Helper()
	b, _ := json.Marshal(resp.Result)
	var r struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &r); err != nil || len(r.Content) == 0 {
		t.Fatalf("could not extract text from result: %s", b)
	}
	return r.Content[0].Text
}

// resultIsError checks whether an MCP tool response is an error.
func resultIsError(t *testing.T, resp jsonRPCResponse) bool {
	t.Helper()
	b, _ := json.Marshal(resp.Result)
	var r struct {
		IsError bool `json:"isError"`
	}
	json.Unmarshal(b, &r)
	return r.IsError
}

// --- Protocol tests ---

func TestInitialize(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(rpc(1, "initialize", nil))

	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
	b, _ := json.Marshal(resp.Result)
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	json.Unmarshal(b, &result)
	if result.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocol version = %q, want 2024-11-05", result.ProtocolVersion)
	}
	if result.ServerInfo.Name != "herald" {
		t.Errorf("server name = %q, want herald", result.ServerInfo.Name)
	}
}

func TestPing(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(rpc(1, "ping", nil))

	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
}

func TestToolsList(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(rpc(1, "tools/list", nil))

	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	b, _ := json.Marshal(resp.Result)
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	json.Unmarshal(b, &result)

	expected := []string{
		"articles_unread", "articles_get", "articles_mark_read",
		"feeds_list", "feed_subscribe", "feed_unsubscribe", "feed_rename",
		"article_groups", "article_group_get", "feed_stats", "poll_now",
		"preferences_get", "preference_set",
		"prompts_list", "prompt_get", "prompt_set", "prompt_reset",
		"briefing", "article_star",
		"user_register", "user_list",
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

func TestUnknownMethod(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(rpc(1, "nonexistent/method", nil))

	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601", resp.Error.Code)
	}
}

// --- Tool tests ---

func TestFeedsListEmpty(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "feeds_list", map[string]any{}))

	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
	if resultIsError(t, resp) {
		t.Fatal("unexpected tool error")
	}
}

func TestFeedSubscribeAndList(t *testing.T) {
	srv := newTestServer(t)
	ts := feedServer(t)

	// Subscribe
	resp := srv.handleRequest(toolCall(1, "feed_subscribe", map[string]any{
		"url":   ts.URL + "/feed.xml",
		"title": "Test Feed",
	}))
	if resultIsError(t, resp) {
		t.Fatalf("subscribe error: %s", resultText(t, resp))
	}

	// List should show the feed
	resp = srv.handleRequest(toolCall(2, "feeds_list", map[string]any{}))
	text := resultText(t, resp)
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
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "feed_subscribe", map[string]any{}))

	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing URL")
	}
}

func TestFeedRename(t *testing.T) {
	srv := newTestServer(t)
	ts := feedServer(t)
	feedID := subscribeFeed(t, srv, ts.URL+"/feed.xml")

	// Rename
	resp := srv.handleRequest(toolCall(3, "feed_rename", map[string]any{
		"feed_id": feedID,
		"title":   "Renamed Feed",
	}))
	if resultIsError(t, resp) {
		t.Fatalf("rename error: %s", resultText(t, resp))
	}

	// Verify
	resp = srv.handleRequest(toolCall(4, "feeds_list", map[string]any{}))
	text := resultText(t, resp)
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
	srv := newTestServer(t)

	resp := srv.handleRequest(toolCall(1, "feed_rename", map[string]any{
		"feed_id": 1,
	}))
	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing title")
	}

	resp = srv.handleRequest(toolCall(2, "feed_rename", map[string]any{
		"title": "Foo",
	}))
	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing feed_id")
	}
}

func TestFeedUnsubscribe(t *testing.T) {
	srv := newTestServer(t)
	ts := feedServer(t)
	feedID := subscribeFeed(t, srv, ts.URL+"/feed.xml")

	// Unsubscribe
	resp := srv.handleRequest(toolCall(3, "feed_unsubscribe", map[string]any{
		"feed_id": feedID,
	}))
	if resultIsError(t, resp) {
		t.Fatalf("unsubscribe error: %s", resultText(t, resp))
	}

	// List should be empty
	resp = srv.handleRequest(toolCall(4, "feeds_list", map[string]any{}))
	text := resultText(t, resp)
	var remaining []struct{}
	json.Unmarshal([]byte(text), &remaining)
	if len(remaining) != 0 {
		t.Errorf("expected 0 feeds after unsubscribe, got %d", len(remaining))
	}
}

func TestArticlesUnreadEmpty(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "articles_unread", map[string]any{}))

	if resultIsError(t, resp) {
		t.Fatalf("unexpected error: %s", resultText(t, resp))
	}
}

func TestArticlesUnreadWithFeed(t *testing.T) {
	srv := newTestServer(t)
	ts := feedServer(t)
	subscribeFeed(t, srv, ts.URL+"/feed.xml")

	// Subscribe stores articles from the initial fetch
	resp := srv.handleRequest(toolCall(1, "articles_unread", map[string]any{
		"limit": 10,
	}))
	if resultIsError(t, resp) {
		t.Fatalf("articles_unread error: %s", resultText(t, resp))
	}

	text := resultText(t, resp)
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
	srv := newTestServer(t)
	ts := feedServer(t)
	subscribeFeed(t, srv, ts.URL+"/feed.xml")

	// Get article list to find an ID
	resp := srv.handleRequest(toolCall(1, "articles_unread", map[string]any{"limit": 1}))
	text := resultText(t, resp)
	var articles []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &articles)
	if len(articles) == 0 {
		t.Fatal("no articles to test with")
	}
	articleID := articles[0].ID

	// Get full article
	resp = srv.handleRequest(toolCall(2, "articles_get", map[string]any{
		"article_id": articleID,
	}))
	if resultIsError(t, resp) {
		t.Fatalf("articles_get error: %s", resultText(t, resp))
	}

	// Mark read
	resp = srv.handleRequest(toolCall(3, "articles_mark_read", map[string]any{
		"article_id": articleID,
	}))
	if resultIsError(t, resp) {
		t.Fatalf("articles_mark_read error: %s", resultText(t, resp))
	}
}

func TestArticlesGetMissingID(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "articles_get", map[string]any{}))

	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing article_id")
	}
}

func TestArticlesMarkReadMissingID(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "articles_mark_read", map[string]any{}))

	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing article_id")
	}
}

func TestArticleGroupsEmpty(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "article_groups", map[string]any{}))

	if resultIsError(t, resp) {
		t.Fatalf("unexpected error: %s", resultText(t, resp))
	}
}

func TestArticleGroupGetMissingID(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "article_group_get", map[string]any{}))

	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing group_id")
	}
}

func TestFeedStats(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "feed_stats", map[string]any{}))

	if resultIsError(t, resp) {
		t.Fatalf("unexpected error: %s", resultText(t, resp))
	}
}

func TestFeedStatsWithFeed(t *testing.T) {
	srv := newTestServer(t)
	ts := feedServer(t)
	subscribeFeed(t, srv, ts.URL+"/feed.xml")

	resp := srv.handleRequest(toolCall(1, "feed_stats", map[string]any{}))
	if resultIsError(t, resp) {
		t.Fatalf("feed_stats error: %s", resultText(t, resp))
	}

	text := resultText(t, resp)
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
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "nonexistent_tool", map[string]any{}))

	if !resultIsError(t, resp) {
		t.Fatal("expected error for unknown tool")
	}
	text := resultText(t, resp)
	if text == "" {
		t.Fatal("expected error message")
	}
}

func TestInvalidToolCallParams(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(rpc(1, "tools/call", "not-valid-json"))

	if resultIsError(t, resp) {
		return // expected
	}
	t.Fatal("expected error for invalid params")
}

func TestPollNowDisabled(t *testing.T) {
	srv := newTestServer(t)
	// poller is nil — poll_now should return an error
	resp := srv.handleRequest(toolCall(1, "poll_now", map[string]any{}))

	if !resultIsError(t, resp) {
		t.Fatal("expected error when polling is disabled")
	}
	text := resultText(t, resp)
	if text == "" {
		t.Fatal("expected error message")
	}
}

func TestPollNowEnabled(t *testing.T) {
	srv := newTestServer(t)
	ts := feedServer(t)

	// Subscribe to a feed so poll has something to fetch
	subscribeFeed(t, srv, ts.URL+"/feed.xml")

	// Attach a poller
	p := newPoller(srv.engine, srv.userID, 10*time.Minute, 8.0)
	srv.poller = p

	resp := srv.handleRequest(toolCall(1, "poll_now", map[string]any{}))
	if resultIsError(t, resp) {
		t.Fatalf("poll_now error: %s", resultText(t, resp))
	}

	text := resultText(t, resp)
	var result struct {
		FeedsTotal      int `json:"feeds_total"`
		FeedsDownloaded int `json:"feeds_downloaded"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("unmarshal poll result: %v", err)
	}
	if result.FeedsTotal == 0 {
		t.Error("expected non-zero feeds_total")
	}
}

func TestFeedUnsubscribeMissingID(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "feed_unsubscribe", map[string]any{}))

	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing feed_id")
	}
}

// --- Preferences tests ---

func TestPreferencesGetDefaults(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "preferences_get", map[string]any{}))

	if resultIsError(t, resp) {
		t.Fatalf("unexpected error: %s", resultText(t, resp))
	}

	text := resultText(t, resp)
	var prefs herald.UserPreferences
	if err := json.Unmarshal([]byte(text), &prefs); err != nil {
		t.Fatalf("unmarshal preferences: %v", err)
	}
	// Should have sensible defaults
	if prefs.NotifyWhen != "present" {
		t.Errorf("notify_when = %q, want %q", prefs.NotifyWhen, "present")
	}
	if prefs.NotifyMinScore != 7.0 {
		t.Errorf("notify_min_score = %v, want 7.0", prefs.NotifyMinScore)
	}
}

func TestPreferenceSetAndGet(t *testing.T) {
	srv := newTestServer(t)

	// Set keywords
	resp := srv.handleRequest(toolCall(1, "preference_set", map[string]any{
		"key":   "keywords",
		"value": `["security","golang"]`,
	}))
	if resultIsError(t, resp) {
		t.Fatalf("set keywords error: %s", resultText(t, resp))
	}

	// Set interest_threshold
	resp = srv.handleRequest(toolCall(2, "preference_set", map[string]any{
		"key":   "interest_threshold",
		"value": "6.5",
	}))
	if resultIsError(t, resp) {
		t.Fatalf("set threshold error: %s", resultText(t, resp))
	}

	// Set notify_when
	resp = srv.handleRequest(toolCall(3, "preference_set", map[string]any{
		"key":   "notify_when",
		"value": "always",
	}))
	if resultIsError(t, resp) {
		t.Fatalf("set notify_when error: %s", resultText(t, resp))
	}

	// Verify via preferences_get
	resp = srv.handleRequest(toolCall(4, "preferences_get", map[string]any{}))
	text := resultText(t, resp)
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
	srv := newTestServer(t)

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
			resp := srv.handleRequest(toolCall(1, "preference_set", map[string]any{
				"key":   tt.key,
				"value": tt.value,
			}))
			if !resultIsError(t, resp) {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

// --- Prompt tests ---

func TestPromptsListDefaults(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "prompts_list", map[string]any{}))

	if resultIsError(t, resp) {
		t.Fatalf("unexpected error: %s", resultText(t, resp))
	}

	text := resultText(t, resp)
	var prompts []herald.PromptInfo
	if err := json.Unmarshal([]byte(text), &prompts); err != nil {
		t.Fatalf("unmarshal prompts: %v", err)
	}
	if len(prompts) != 4 {
		t.Fatalf("got %d prompt types, want 4", len(prompts))
	}

	// All should be default status
	for _, p := range prompts {
		if p.Status != "default" {
			t.Errorf("prompt %q status = %q, want %q", p.Type, p.Status, "default")
		}
	}
}

func TestPromptGetDefault(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "prompt_get", map[string]any{
		"prompt_type": "curation",
	}))

	if resultIsError(t, resp) {
		t.Fatalf("unexpected error: %s", resultText(t, resp))
	}

	text := resultText(t, resp)
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
	srv := newTestServer(t)
	customTemplate := "Rate this article: {{.Title}}"
	temp := 0.5

	// Set custom prompt
	resp := srv.handleRequest(toolCall(1, "prompt_set", map[string]any{
		"prompt_type": "curation",
		"template":    customTemplate,
		"temperature": temp,
	}))
	if resultIsError(t, resp) {
		t.Fatalf("prompt_set error: %s", resultText(t, resp))
	}

	// Verify via prompt_get
	resp = srv.handleRequest(toolCall(2, "prompt_get", map[string]any{
		"prompt_type": "curation",
	}))
	text := resultText(t, resp)
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
	resp = srv.handleRequest(toolCall(3, "prompts_list", map[string]any{}))
	listText := resultText(t, resp)
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
	srv := newTestServer(t)
	temp := 1.5

	// Set only temperature (template should be preserved from default)
	resp := srv.handleRequest(toolCall(1, "prompt_set", map[string]any{
		"prompt_type": "summarization",
		"temperature": temp,
	}))
	if resultIsError(t, resp) {
		t.Fatalf("prompt_set error: %s", resultText(t, resp))
	}

	// Verify template is non-empty (default preserved) and temperature is set
	resp = srv.handleRequest(toolCall(2, "prompt_get", map[string]any{
		"prompt_type": "summarization",
	}))
	text := resultText(t, resp)
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
	srv := newTestServer(t)

	// Set a custom prompt first
	resp := srv.handleRequest(toolCall(1, "prompt_set", map[string]any{
		"prompt_type": "curation",
		"template":    "custom template",
	}))
	if resultIsError(t, resp) {
		t.Fatalf("prompt_set error: %s", resultText(t, resp))
	}

	// Reset
	resp = srv.handleRequest(toolCall(2, "prompt_reset", map[string]any{
		"prompt_type": "curation",
	}))
	if resultIsError(t, resp) {
		t.Fatalf("prompt_reset error: %s", resultText(t, resp))
	}

	// Verify it's back to default
	resp = srv.handleRequest(toolCall(3, "prompt_get", map[string]any{
		"prompt_type": "curation",
	}))
	text := resultText(t, resp)
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
	srv := newTestServer(t)

	// All prompt operations should block "security" type
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
			resp := srv.handleRequest(toolCall(1, tt.tool, tt.args))
			if !resultIsError(t, resp) {
				t.Fatalf("expected error when accessing security prompt via %s", tt.tool)
			}
		})
	}
}

func TestPromptGetUnknownType(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "prompt_get", map[string]any{
		"prompt_type": "nonexistent",
	}))
	if !resultIsError(t, resp) {
		t.Fatal("expected error for unknown prompt type")
	}
}

func TestPromptSetMissingParams(t *testing.T) {
	srv := newTestServer(t)

	// Missing prompt_type
	resp := srv.handleRequest(toolCall(1, "prompt_set", map[string]any{
		"template": "foo",
	}))
	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing prompt_type")
	}

	// Neither template nor temperature
	resp = srv.handleRequest(toolCall(2, "prompt_set", map[string]any{
		"prompt_type": "curation",
	}))
	if !resultIsError(t, resp) {
		t.Fatal("expected error when neither template nor temperature provided")
	}
}

// --- Briefing tests ---

func TestBriefingEmpty(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "briefing", map[string]any{}))

	if resultIsError(t, resp) {
		t.Fatalf("unexpected error: %s", resultText(t, resp))
	}
	text := resultText(t, resp)
	if text == "" {
		t.Fatal("expected a response message even with no articles")
	}
}

// --- Article star tests ---

func TestArticleStarAndUnstar(t *testing.T) {
	srv := newTestServer(t)
	ts := feedServer(t)
	subscribeFeed(t, srv, ts.URL+"/feed.xml")

	// Get an article ID
	resp := srv.handleRequest(toolCall(1, "articles_unread", map[string]any{"limit": 1}))
	text := resultText(t, resp)
	var articles []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &articles)
	if len(articles) == 0 {
		t.Fatal("no articles to test with")
	}
	articleID := articles[0].ID

	// Star
	resp = srv.handleRequest(toolCall(2, "article_star", map[string]any{
		"article_id": articleID,
		"starred":    true,
	}))
	if resultIsError(t, resp) {
		t.Fatalf("star error: %s", resultText(t, resp))
	}

	// Unstar
	resp = srv.handleRequest(toolCall(3, "article_star", map[string]any{
		"article_id": articleID,
		"starred":    false,
	}))
	if resultIsError(t, resp) {
		t.Fatalf("unstar error: %s", resultText(t, resp))
	}
}

func TestArticleStarMissingParams(t *testing.T) {
	srv := newTestServer(t)

	resp := srv.handleRequest(toolCall(1, "article_star", map[string]any{
		"starred": true,
	}))
	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing article_id")
	}

	resp = srv.handleRequest(toolCall(2, "article_star", map[string]any{
		"article_id": 1,
	}))
	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing starred")
	}
}

// --- User management tests ---

func TestUserRegister(t *testing.T) {
	srv := newTestServer(t)

	resp := srv.handleRequest(toolCall(1, "user_register", map[string]any{
		"name": "alice",
	}))
	if resultIsError(t, resp) {
		t.Fatalf("user_register error: %s", resultText(t, resp))
	}

	text := resultText(t, resp)
	var result struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.ID == 0 {
		t.Error("expected non-zero user ID")
	}
	if result.Name != "alice" {
		t.Errorf("name = %q, want %q", result.Name, "alice")
	}
}

func TestUserRegisterMissingName(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "user_register", map[string]any{}))
	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing name")
	}
}

func TestUserRegisterDuplicate(t *testing.T) {
	srv := newTestServer(t)

	resp := srv.handleRequest(toolCall(1, "user_register", map[string]any{"name": "bob"}))
	if resultIsError(t, resp) {
		t.Fatalf("first register error: %s", resultText(t, resp))
	}

	resp = srv.handleRequest(toolCall(2, "user_register", map[string]any{"name": "bob"}))
	if !resultIsError(t, resp) {
		t.Fatal("expected error for duplicate name")
	}
}

func TestUserList(t *testing.T) {
	srv := newTestServer(t)

	// Empty initially
	resp := srv.handleRequest(toolCall(1, "user_list", map[string]any{}))
	if resultIsError(t, resp) {
		t.Fatalf("user_list error: %s", resultText(t, resp))
	}
	text := resultText(t, resp)
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
	srv.handleRequest(toolCall(2, "user_register", map[string]any{"name": "alice"}))
	srv.handleRequest(toolCall(3, "user_register", map[string]any{"name": "bob"}))

	resp = srv.handleRequest(toolCall(4, "user_list", map[string]any{}))
	text = resultText(t, resp)
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
	dbPath := filepath.Join(t.TempDir(), "test.db")
	engine, err := herald.NewEngine(herald.EngineConfig{DBPath: dbPath})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	srv := newServer(engine, 99)
	ts := feedServer(t)

	// Register "alice" as user (gets ID 1)
	resp := srv.handleRequest(toolCall(1, "user_register", map[string]any{"name": "alice"}))
	if resultIsError(t, resp) {
		t.Fatalf("register error: %s", resultText(t, resp))
	}

	// Subscribe a feed as alice (via speaker)
	resp = srv.handleRequest(toolCall(2, "feed_subscribe", map[string]any{
		"url":     ts.URL + "/feed.xml",
		"speaker": "alice",
	}))
	if resultIsError(t, resp) {
		t.Fatalf("subscribe error: %s", resultText(t, resp))
	}

	// Alice should see the feed
	resp = srv.handleRequest(toolCall(3, "feeds_list", map[string]any{
		"speaker": "alice",
	}))
	text := resultText(t, resp)
	var feeds []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &feeds)
	if len(feeds) != 1 {
		t.Fatalf("alice should have 1 feed, got %d", len(feeds))
	}

	// Default user (ID 99) should NOT see alice's feed
	resp = srv.handleRequest(toolCall(4, "feeds_list", map[string]any{}))
	text = resultText(t, resp)
	var defaultFeeds []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &defaultFeeds)
	if len(defaultFeeds) != 0 {
		t.Errorf("default user should have 0 feeds, got %d", len(defaultFeeds))
	}
}

func TestSpeakerFallback(t *testing.T) {
	srv := newTestServer(t)

	// Unknown speaker should fall back to default user
	resp := srv.handleRequest(toolCall(1, "feeds_list", map[string]any{
		"speaker": "unknown_person",
	}))
	if resultIsError(t, resp) {
		t.Fatalf("unexpected error: %s", resultText(t, resp))
	}
	// Should succeed (using default user) — no error
}

func TestSpeakerOmitted(t *testing.T) {
	srv := newTestServer(t)
	ts := feedServer(t)

	// Subscribe without speaker — should use default user
	resp := srv.handleRequest(toolCall(1, "feed_subscribe", map[string]any{
		"url": ts.URL + "/feed.xml",
	}))
	if resultIsError(t, resp) {
		t.Fatalf("subscribe error: %s", resultText(t, resp))
	}

	// List without speaker — should see the feed
	resp = srv.handleRequest(toolCall(2, "feeds_list", map[string]any{}))
	text := resultText(t, resp)
	var feeds []struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(text), &feeds)
	if len(feeds) != 1 {
		t.Fatalf("expected 1 feed, got %d", len(feeds))
	}
}
