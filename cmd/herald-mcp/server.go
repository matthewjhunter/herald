package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

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
	poller *poller // non-nil when --poll is enabled
}

func newServer(engine *herald.Engine, userID int64) *server {
	return &server{engine: engine, userID: userID}
}

// resolveUser maps a speaker name to a user ID.
// If speaker is empty or unknown, it returns the default user ID.
func (s *server) resolveUser(speaker string) int64 {
	if speaker == "" {
		return s.userID
	}
	id, err := s.engine.ResolveUser(speaker)
	if err != nil || id == 0 {
		return s.userID
	}
	return id
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
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
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
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
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
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
					"required": []string{"article_id"},
				},
			},
			{
				"name":        "feeds_list",
				"description": "List all subscribed RSS/Atom feeds with their titles, URLs, and last fetch times.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
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
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
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
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
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
					"type": "object",
					"properties": map[string]any{
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
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
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
					"required": []string{"group_id"},
				},
			},
			{
				"name":        "feed_stats",
				"description": "Get article statistics per feed and totals: total articles, unread count, and unsummarized count. Use this to understand pipeline health and coverage.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
				},
			},
			{
				"name":        "poll_now",
				"description": "Trigger an immediate feed poll cycle: fetch all feeds, score new articles through the AI pipeline, and return results. Only available when the server is running with --poll. Use this when the user asks to check for new articles right now.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
			{
				"name":        "preferences_get",
				"description": "Get all user preferences as structured JSON. Returns keywords, interest threshold, and notification settings.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
				},
			},
			{
				"name":        "preference_set",
				"description": "Set a single user preference by key. Valid keys: keywords (JSON array), interest_threshold (number), notify_when (\"present\"|\"always\"|\"queue\"), notify_min_score (number).",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"key": map[string]any{
							"type":        "string",
							"description": "Preference key to set",
							"enum":        []string{"keywords", "interest_threshold", "notify_when", "notify_min_score"},
						},
						"value": map[string]any{
							"type":        "string",
							"description": "Value to set (keywords as JSON array, thresholds as number strings, notify_when as enum)",
						},
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
					"required": []string{"key", "value"},
				},
			},
			{
				"name":        "prompts_list",
				"description": "List all customizable prompt types with their current status (custom or default) and temperature.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
				},
			},
			{
				"name":        "prompt_get",
				"description": "Get the active prompt template and temperature for a given type. Prompt types: curation, summarization, group_summary, related_groups.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"prompt_type": map[string]any{
							"type":        "string",
							"description": "The prompt type to retrieve",
							"enum":        []string{"curation", "summarization", "group_summary", "related_groups"},
						},
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
					"required": []string{"prompt_type"},
				},
			},
			{
				"name":        "prompt_set",
				"description": "Customize a prompt template and/or temperature. At least one of template or temperature must be provided. Prompt types: curation, summarization, group_summary, related_groups.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"prompt_type": map[string]any{
							"type":        "string",
							"description": "The prompt type to customize",
							"enum":        []string{"curation", "summarization", "group_summary", "related_groups"},
						},
						"template": map[string]any{
							"type":        "string",
							"description": "New prompt template text",
						},
						"temperature": map[string]any{
							"type":        "number",
							"description": "Temperature setting (0.0-2.0)",
						},
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
					"required": []string{"prompt_type"},
				},
			},
			{
				"name":        "prompt_reset",
				"description": "Revert a prompt type to its embedded default. Deletes any custom template and temperature.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"prompt_type": map[string]any{
							"type":        "string",
							"description": "The prompt type to reset",
							"enum":        []string{"curation", "summarization", "group_summary", "related_groups"},
						},
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
					"required": []string{"prompt_type"},
				},
			},
			{
				"name":        "briefing",
				"description": "Generate a markdown briefing from high-interest unread articles. Includes titles, scores, URLs, and AI summaries.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
				},
			},
			{
				"name":        "article_star",
				"description": "Set or clear the starred flag on an article. Starred articles are saved for later reference.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"article_id": map[string]any{
							"type":        "integer",
							"description": "The article ID to star/unstar",
						},
						"starred": map[string]any{
							"type":        "boolean",
							"description": "true to star, false to unstar",
						},
						"speaker": map[string]any{
							"type":        "string",
							"description": "Speaker name for multi-user resolution. If omitted, uses the default user.",
						},
					},
					"required": []string{"article_id", "starred"},
				},
			},
			{
				"name":        "user_register",
				"description": "Register a speaker name as a herald user. Returns the new user's ID. Use this when a new household member wants their own feeds, preferences, and read state.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{
							"type":        "string",
							"description": "Speaker name to register",
						},
					},
					"required": []string{"name"},
				},
			},
			{
				"name":        "user_list",
				"description": "List all registered herald users.",
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
		return s.handleFeedsList(call.Arguments)
	case "feed_subscribe":
		return s.handleFeedSubscribe(call.Arguments)
	case "feed_unsubscribe":
		return s.handleFeedUnsubscribe(call.Arguments)
	case "feed_rename":
		return s.handleFeedRename(call.Arguments)
	case "article_groups":
		return s.handleArticleGroups(call.Arguments)
	case "article_group_get":
		return s.handleArticleGroupGet(call.Arguments)
	case "feed_stats":
		return s.handleFeedStats(call.Arguments)
	case "poll_now":
		return s.handlePollNow()
	case "preferences_get":
		return s.handlePreferencesGet(call.Arguments)
	case "preference_set":
		return s.handlePreferenceSet(call.Arguments)
	case "prompts_list":
		return s.handlePromptsList(call.Arguments)
	case "prompt_get":
		return s.handlePromptGet(call.Arguments)
	case "prompt_set":
		return s.handlePromptSet(call.Arguments)
	case "prompt_reset":
		return s.handlePromptReset(call.Arguments)
	case "briefing":
		return s.handleBriefing(call.Arguments)
	case "article_star":
		return s.handleArticleStar(call.Arguments)
	case "user_register":
		return s.handleUserRegister(call.Arguments)
	case "user_list":
		return s.handleUserList()
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
		Speaker  string  `json:"speaker"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	userID := s.resolveUser(params.Speaker)

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}

	if params.MinScore > 0 {
		articles, scores, err := s.engine.GetHighInterestArticles(userID, params.MinScore, limit, params.Offset)
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

	articles, err := s.engine.GetUnreadArticles(userID, limit, params.Offset)
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
		ArticleID int64  `json:"article_id"`
		Speaker   string `json:"speaker"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.ArticleID == 0 {
		return mcpError("article_id parameter is required")
	}
	userID := s.resolveUser(params.Speaker)

	article, err := s.engine.GetArticleForUser(userID, params.ArticleID)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("articles_get: id=%d", params.ArticleID)
	return mcpJSON(article)
}

func (s *server) handleArticlesMarkRead(args json.RawMessage) any {
	var params struct {
		ArticleID int64  `json:"article_id"`
		Speaker   string `json:"speaker"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.ArticleID == 0 {
		return mcpError("article_id parameter is required")
	}
	userID := s.resolveUser(params.Speaker)

	if err := s.engine.MarkArticleRead(userID, params.ArticleID); err != nil {
		return mcpError("%v", err)
	}

	log.Printf("articles_mark_read: id=%d", params.ArticleID)
	return mcpText("Article %d marked as read.", params.ArticleID)
}

func (s *server) handleFeedsList(args json.RawMessage) any {
	var params struct {
		Speaker string `json:"speaker"`
	}
	json.Unmarshal(args, &params)
	userID := s.resolveUser(params.Speaker)

	feeds, err := s.engine.GetUserFeeds(userID)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("feeds_list: %d feeds", len(feeds))
	return mcpJSON(feeds)
}

func (s *server) handleFeedSubscribe(args json.RawMessage) any {
	var params struct {
		URL     string `json:"url"`
		Title   string `json:"title"`
		Speaker string `json:"speaker"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.URL == "" {
		return mcpError("url parameter is required")
	}
	userID := s.resolveUser(params.Speaker)

	if err := s.engine.SubscribeFeed(userID, params.URL, params.Title); err != nil {
		return mcpError("%v", err)
	}

	log.Printf("feed_subscribe: url=%s title=%q", params.URL, params.Title)
	return mcpText("Subscribed to %s", params.URL)
}

func (s *server) handleFeedUnsubscribe(args json.RawMessage) any {
	var params struct {
		FeedID  int64  `json:"feed_id"`
		Speaker string `json:"speaker"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.FeedID == 0 {
		return mcpError("feed_id parameter is required")
	}
	userID := s.resolveUser(params.Speaker)

	if err := s.engine.UnsubscribeFeed(userID, params.FeedID); err != nil {
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

func (s *server) handleArticleGroups(args json.RawMessage) any {
	var params struct {
		Speaker string `json:"speaker"`
	}
	json.Unmarshal(args, &params)
	userID := s.resolveUser(params.Speaker)

	groups, err := s.engine.GetUserGroups(userID)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("article_groups: %d groups", len(groups))
	return mcpJSON(groups)
}

func (s *server) handleArticleGroupGet(args json.RawMessage) any {
	var params struct {
		GroupID int64  `json:"group_id"`
		Speaker string `json:"speaker"`
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

func (s *server) handleFeedStats(args json.RawMessage) any {
	var params struct {
		Speaker string `json:"speaker"`
	}
	json.Unmarshal(args, &params)
	userID := s.resolveUser(params.Speaker)

	stats, err := s.engine.GetFeedStats(userID)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("feed_stats")
	return mcpJSON(stats)
}

func (s *server) handlePollNow() any {
	if s.poller == nil {
		return mcpError("polling is not enabled (start with --poll)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := s.poller.poll(ctx)
	if err != nil {
		return mcpError("poll failed: %v", err)
	}

	log.Printf("poll_now: %d/%d feeds, %d new, %d scored, %d high-interest",
		result.FeedsDownloaded, result.FeedsTotal,
		result.NewArticles, result.ProcessedCount, result.HighInterest)
	return mcpJSON(result)
}

func (s *server) handlePreferencesGet(args json.RawMessage) any {
	var params struct {
		Speaker string `json:"speaker"`
	}
	json.Unmarshal(args, &params)
	userID := s.resolveUser(params.Speaker)

	prefs, err := s.engine.GetPreferences(userID)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("preferences_get")
	return mcpJSON(prefs)
}

func (s *server) handlePreferenceSet(args json.RawMessage) any {
	var params struct {
		Key     string `json:"key"`
		Value   string `json:"value"`
		Speaker string `json:"speaker"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.Key == "" {
		return mcpError("key parameter is required")
	}
	if params.Value == "" {
		return mcpError("value parameter is required")
	}
	userID := s.resolveUser(params.Speaker)

	if err := s.engine.SetPreference(userID, params.Key, params.Value); err != nil {
		return mcpError("%v", err)
	}

	log.Printf("preference_set: %s=%s", params.Key, params.Value)
	return mcpText("Preference %q set.", params.Key)
}

func (s *server) handlePromptsList(args json.RawMessage) any {
	var params struct {
		Speaker string `json:"speaker"`
	}
	json.Unmarshal(args, &params)
	userID := s.resolveUser(params.Speaker)

	prompts, err := s.engine.ListPrompts(userID)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("prompts_list: %d types", len(prompts))
	return mcpJSON(prompts)
}

func (s *server) handlePromptGet(args json.RawMessage) any {
	var params struct {
		PromptType string `json:"prompt_type"`
		Speaker    string `json:"speaker"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.PromptType == "" {
		return mcpError("prompt_type parameter is required")
	}
	if params.PromptType == "security" {
		return mcpError("the security prompt type cannot be viewed or modified")
	}
	userID := s.resolveUser(params.Speaker)

	detail, err := s.engine.GetPrompt(userID, params.PromptType)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("prompt_get: type=%s custom=%v", params.PromptType, detail.IsCustom)
	return mcpJSON(detail)
}

func (s *server) handlePromptSet(args json.RawMessage) any {
	var params struct {
		PromptType  string   `json:"prompt_type"`
		Template    string   `json:"template"`
		Temperature *float64 `json:"temperature"`
		Speaker     string   `json:"speaker"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.PromptType == "" {
		return mcpError("prompt_type parameter is required")
	}
	if params.PromptType == "security" {
		return mcpError("the security prompt type cannot be viewed or modified")
	}
	if params.Template == "" && params.Temperature == nil {
		return mcpError("at least one of template or temperature is required")
	}
	userID := s.resolveUser(params.Speaker)

	if err := s.engine.SetPrompt(userID, params.PromptType, params.Template, params.Temperature); err != nil {
		return mcpError("%v", err)
	}

	log.Printf("prompt_set: type=%s", params.PromptType)
	return mcpText("Prompt %q updated.", params.PromptType)
}

func (s *server) handlePromptReset(args json.RawMessage) any {
	var params struct {
		PromptType string `json:"prompt_type"`
		Speaker    string `json:"speaker"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.PromptType == "" {
		return mcpError("prompt_type parameter is required")
	}
	if params.PromptType == "security" {
		return mcpError("the security prompt type cannot be viewed or modified")
	}
	userID := s.resolveUser(params.Speaker)

	if err := s.engine.ResetPrompt(userID, params.PromptType); err != nil {
		return mcpError("%v", err)
	}

	log.Printf("prompt_reset: type=%s", params.PromptType)
	return mcpText("Prompt %q reset to default.", params.PromptType)
}

func (s *server) handleBriefing(args json.RawMessage) any {
	var params struct {
		Speaker string `json:"speaker"`
	}
	json.Unmarshal(args, &params)
	userID := s.resolveUser(params.Speaker)

	briefing, err := s.engine.GenerateBriefing(userID)
	if err != nil {
		return mcpError("%v", err)
	}

	if briefing == "" {
		return mcpText("No high-interest unread articles for a briefing.")
	}

	log.Printf("briefing: generated")
	return mcpText("%s", briefing)
}

func (s *server) handleArticleStar(args json.RawMessage) any {
	var params struct {
		ArticleID int64  `json:"article_id"`
		Starred   *bool  `json:"starred"`
		Speaker   string `json:"speaker"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.ArticleID == 0 {
		return mcpError("article_id parameter is required")
	}
	if params.Starred == nil {
		return mcpError("starred parameter is required")
	}
	userID := s.resolveUser(params.Speaker)

	if err := s.engine.StarArticle(userID, params.ArticleID, *params.Starred); err != nil {
		return mcpError("%v", err)
	}

	action := "starred"
	if !*params.Starred {
		action = "unstarred"
	}
	log.Printf("article_star: id=%d %s", params.ArticleID, action)
	return mcpText("Article %d %s.", params.ArticleID, action)
}

func (s *server) handleUserRegister(args json.RawMessage) any {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return mcpError("invalid arguments: %v", err)
	}
	if params.Name == "" {
		return mcpError("name parameter is required")
	}

	id, err := s.engine.RegisterUser(params.Name)
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("user_register: name=%q id=%d", params.Name, id)
	return mcpJSON(map[string]any{"id": id, "name": params.Name})
}

func (s *server) handleUserList() any {
	users, err := s.engine.ListUsers()
	if err != nil {
		return mcpError("%v", err)
	}

	log.Printf("user_list: %d users", len(users))
	return mcpJSON(users)
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
