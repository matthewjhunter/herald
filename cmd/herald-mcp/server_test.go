package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
		"article_groups", "article_group_get", "feed_stats",
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

func TestFeedUnsubscribeMissingID(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRequest(toolCall(1, "feed_unsubscribe", map[string]any{}))

	if !resultIsError(t, resp) {
		t.Fatal("expected error for missing feed_id")
	}
}
