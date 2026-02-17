package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/matthewjhunter/herald"
)

// JSON-RPC 2.0 types

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// server is the Herald MCP server.
type server struct {
	engine *herald.Engine
	userID int64
}

func newServer(engine *herald.Engine, userID int64) *server {
	return &server{engine: engine, userID: userID}
}

// run starts the MCP server, reading from stdin and writing to stdout.
func (s *server) run() error {
	log.SetOutput(os.Stderr)
	log.Printf("herald-mcp starting (user=%d)", s.userID)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("invalid json-rpc: %v", err)
			continue
		}

		// Notifications have no ID — don't respond
		if req.ID == nil || string(req.ID) == "null" {
			log.Printf("notification: %s", req.Method)
			continue
		}

		resp := s.handleRequest(req)
		respBytes, _ := json.Marshal(resp)
		fmt.Fprintf(os.Stdout, "%s\n", respBytes)
	}

	return scanner.Err()
}

func (s *server) handleRequest(req jsonRPCRequest) jsonRPCResponse {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
	}

	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "herald",
				"version": "0.1.0",
			},
		}
	case "tools/list":
		resp.Result = s.handleToolsList()
	case "tools/call":
		resp.Result = s.handleToolsCall(req.Params)
	case "ping":
		resp.Result = map[string]any{}
	default:
		resp.Error = &rpcError{
			Code:    -32601,
			Message: fmt.Sprintf("method not found: %s", req.Method),
		}
	}

	return resp
}

func (s *server) handleToolsList() any {
	return map[string]any{
		"tools": []map[string]any{
			{
				"name":        "articles_unread",
				"description": "Get unread articles from subscribed feeds, optionally filtered by minimum interest score. Returns article titles, URLs, summaries, and scores.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"limit": map[string]any{
							"type":        "integer",
							"description": "Maximum number of articles to return (default 20)",
						},
						"offset": map[string]any{
							"type":        "integer",
							"description": "Number of articles to skip for pagination (default 0)",
						},
						"min_score": map[string]any{
							"type":        "number",
							"description": "Minimum interest score filter (0-10). Only returns articles scored at or above this threshold.",
						},
					},
				},
			},
			{
				"name":        "articles_get",
				"description": "Get full article content by ID. Use this to read the complete text of an article for follow-up discussion or analysis.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"article_id": map[string]any{
							"type":        "integer",
							"description": "The article ID to retrieve",
						},
					},
					"required": []string{"article_id"},
				},
			},
			{
				"name":        "articles_mark_read",
				"description": "Mark an article as read. Call this after discussing or presenting an article to the user.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"article_id": map[string]any{
							"type":        "integer",
							"description": "The article ID to mark as read",
						},
					},
					"required": []string{"article_id"},
				},
			},
			{
				"name":        "feeds_list",
				"description": "List all subscribed RSS/Atom feeds with their titles, URLs, and last fetch times.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
			{
				"name":        "feed_subscribe",
				"description": "Subscribe to an RSS/Atom feed by URL. Creates the feed if new, then subscribes the user. Find the feed URL first (e.g. via web search) before calling this.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{
							"type":        "string",
							"description": "The RSS/Atom feed URL",
						},
						"title": map[string]any{
							"type":        "string",
							"description": "Optional display title for the feed. If omitted, the feed's own title is used once fetched.",
						},
					},
					"required": []string{"url"},
				},
			},
			{
				"name":        "feed_unsubscribe",
				"description": "Unsubscribe from a feed by ID. Use feeds_list to find the feed ID. If no other users subscribe to it, the feed and its articles are deleted.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"feed_id": map[string]any{
							"type":        "integer",
							"description": "The feed ID to unsubscribe from",
						},
					},
					"required": []string{"feed_id"},
				},
			},
			{
				"name":        "feed_rename",
				"description": "Rename a feed's display title. Use feeds_list to find the feed ID.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"feed_id": map[string]any{
							"type":        "integer",
							"description": "The feed ID to rename",
						},
						"title": map[string]any{
							"type":        "string",
							"description": "The new display title",
						},
					},
					"required": []string{"feed_id", "title"},
				},
			},
			{
				"name":        "article_groups",
				"description": "List article groups (clusters of articles covering the same event or topic). Each group has a topic label, article count, and max interest score. Use this for briefings to present related coverage together.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
			{
				"name":        "article_group_get",
				"description": "Get a specific article group with all its articles and scores. Use this to drill into a topic cluster and see all related coverage.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"group_id": map[string]any{
							"type":        "integer",
							"description": "The group ID to retrieve",
						},
					},
					"required": []string{"group_id"},
				},
			},
			{
				"name":        "feed_stats",
				"description": "Get article statistics per feed and totals: total articles, unread count, and unsummarized count. Use this to understand pipeline health and coverage.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
	}
}

func (s *server) handleToolsCall(params json.RawMessage) any {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}

	if err := json.Unmarshal(params, &call); err != nil {
		return mcpError("invalid tool call: %v", err)
	}

	switch call.Name {
	case "articles_unread":
		return s.handleArticlesUnread(call.Arguments)
	case "articles_get":
		return s.handleArticlesGet(call.Arguments)
	case "articles_mark_read":
		return s.handleArticlesMarkRead(call.Arguments)
	case "feeds_list":
		return s.handleFeedsList()
	case "feed_subscribe":
		return s.handleFeedSubscribe(call.Arguments)
	case "feed_unsubscribe":
		return s.handleFeedUnsubscribe(call.Arguments)
	case "feed_rename":
		return s.handleFeedRename(call.Arguments)
	case "article_groups":
		return s.handleArticleGroups()
	case "article_group_get":
		return s.handleArticleGroupGet(call.Arguments)
	case "feed_stats":
		return s.handleFeedStats()
	default:
		return mcpError("unknown tool: %s", call.Name)
	}
}

// --- tool handlers ---

func (s *server) handleArticlesUnread(args json.RawMessage) any {
	var params struct {
		Limit    int     `json:"limit"`
		Offset   int     `json:"offset"`
		MinScore float64 `json:"min_score"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}

	if params.MinScore > 0 {
		articles, scores, err := s.engine.GetHighInterestArticles(s.userID, params.MinScore, limit, params.Offset)
		if err != nil {
			return mcpError("%v", err)
		}
		type scoredArticle struct {
			herald.Article
			InterestScore float64 `json:"interest_score"`
		}
		result := make([]scoredArticle, len(articles))
		for i, a := range articles {
			a.Content = "" // omit bodies from listings
			score := 0.0
			if i < len(scores) {
				score = scores[i]
			}
			result[i] = scoredArticle{Article: a, InterestScore: score}
		}
		log.Printf("articles_unread: limit=%d min_score=%.1f -> %d results", limit, params.MinScore, len(result))
		return mcpJSON(result)
	}

	articles, err := s.engine.GetUnreadArticles(s.userID, limit, params.Offset)
	if err != nil {
		return mcpError("%v", err)
	}
	for i := range articles {
		articles[i].Content = "" // omit bodies from listings
	}
	log.Printf("articles_unread: limit=%d -> %d results", limit, len(articles))
	return mcpJSON(articles)
}

func (s *server) handleArticlesGet(args json.RawMessage) any {
	var params struct {
		ArticleID int64 `json:"article_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.ArticleID == 0 {
		return mcpError("article_id parameter is required")
	}

	article, err := s.engine.GetArticleForUser(s.userID, params.ArticleID)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("articles_get: id=%d", params.ArticleID)
	return mcpJSON(article)
}

func (s *server) handleArticlesMarkRead(args json.RawMessage) any {
	var params struct {
		ArticleID int64 `json:"article_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.ArticleID == 0 {
		return mcpError("article_id parameter is required")
	}

	if err := s.engine.MarkArticleRead(params.ArticleID); err != nil {
		return mcpError("%v", err)
	}

	log.Printf("articles_mark_read: id=%d", params.ArticleID)
	return mcpText("Article %d marked as read.", params.ArticleID)
}

func (s *server) handleFeedsList() any {
	feeds, err := s.engine.GetUserFeeds(s.userID)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("feeds_list: %d feeds", len(feeds))
	return mcpJSON(feeds)
}

func (s *server) handleFeedSubscribe(args json.RawMessage) any {
	var params struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.URL == "" {
		return mcpError("url parameter is required")
	}

	if err := s.engine.SubscribeFeed(s.userID, params.URL, params.Title); err != nil {
		return mcpError("%v", err)
	}

	log.Printf("feed_subscribe: url=%s title=%q", params.URL, params.Title)
	return mcpText("Subscribed to %s", params.URL)
}

func (s *server) handleFeedUnsubscribe(args json.RawMessage) any {
	var params struct {
		FeedID int64 `json:"feed_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.FeedID == 0 {
		return mcpError("feed_id parameter is required")
	}

	if err := s.engine.UnsubscribeFeed(s.userID, params.FeedID); err != nil {
		return mcpError("%v", err)
	}

	log.Printf("feed_unsubscribe: feed_id=%d", params.FeedID)
	return mcpText("Unsubscribed from feed %d.", params.FeedID)
}

func (s *server) handleFeedRename(args json.RawMessage) any {
	var params struct {
		FeedID int64  `json:"feed_id"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.FeedID == 0 {
		return mcpError("feed_id parameter is required")
	}
	if params.Title == "" {
		return mcpError("title parameter is required")
	}

	if err := s.engine.RenameFeed(params.FeedID, params.Title); err != nil {
		return mcpError("%v", err)
	}

	log.Printf("feed_rename: feed_id=%d title=%q", params.FeedID, params.Title)
	return mcpText("Feed %d renamed to %q.", params.FeedID, params.Title)
}

func (s *server) handleArticleGroups() any {
	groups, err := s.engine.GetUserGroups(s.userID)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("article_groups: %d groups", len(groups))
	return mcpJSON(groups)
}

func (s *server) handleArticleGroupGet(args json.RawMessage) any {
	var params struct {
		GroupID int64 `json:"group_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.GroupID == 0 {
		return mcpError("group_id parameter is required")
	}

	group, err := s.engine.GetGroupArticles(params.GroupID)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("article_group_get: id=%d", params.GroupID)
	return mcpJSON(group)
}

func (s *server) handleFeedStats() any {
	stats, err := s.engine.GetFeedStats(s.userID)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("feed_stats")
	return mcpJSON(stats)
}

// --- MCP response helpers ---

func mcpText(format string, args ...any) any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": fmt.Sprintf(format, args...)},
		},
	}
}

func mcpJSON(data any) any {
	b, err := json.Marshal(data)
	if err != nil {
		return mcpError("marshal response: %v", err)
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(b)},
		},
	}
}

func mcpError(format string, args ...any) any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": fmt.Sprintf("Error: "+format, args...)},
		},
		"isError": true,
	}
}
