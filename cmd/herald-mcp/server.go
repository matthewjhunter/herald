package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/matthewjhunter/herald"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// heraldServer holds shared state for all tool handlers.
type heraldServer struct {
	engine *herald.Engine
	userID int64
	poller *poller // non-nil when --poll is enabled
}

func newHeraldServer(engine *herald.Engine, userID int64) *heraldServer {
	return &heraldServer{engine: engine, userID: userID}
}

// resolveUser maps a speaker name to a user ID.
// If speaker is empty or unknown, it returns the default user ID.
func (hs *heraldServer) resolveUser(speaker string) int64 {
	if speaker == "" {
		return hs.userID
	}
	id, err := hs.engine.ResolveUser(speaker)
	if err != nil || id == 0 {
		return hs.userID
	}
	return id
}

// newMCPServer creates an MCP server with all herald tools registered.
func newMCPServer(hs *heraldServer) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "herald",
		Version: "0.1.0",
	}, nil)
	registerTools(s, hs)
	return s
}

// --- Response helpers ---

func textResult(format string, args ...any) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}, nil, nil
}

func jsonResult(data any) (*mcp.CallToolResult, any, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return errResult("marshal response: %v", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
	}, nil, nil
}

func errResult(format string, args ...any) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("Error: "+format, args...)},
		},
		IsError: true,
	}, nil, nil
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// registerTools registers all 26 herald tools with the MCP server.
func registerTools(s *mcp.Server, hs *heraldServer) {

	mcp.AddTool(s, &mcp.Tool{
		Name:        "articles_unread",
		Description: "Get unread articles from subscribed feeds, optionally filtered by minimum interest score. Returns article titles, URLs, summaries, and scores.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input articlesUnreadInput) (*mcp.CallToolResult, any, error) {
		userID := hs.resolveUser(ptrStr(input.Speaker))
		limit := 20
		if input.Limit != nil && *input.Limit > 0 {
			limit = *input.Limit
		}
		offset := 0
		if input.Offset != nil {
			offset = *input.Offset
		}
		minScore := 0.0
		if input.MinScore != nil {
			minScore = *input.MinScore
		}

		if minScore > 0 {
			articles, scores, err := hs.engine.GetHighInterestArticles(userID, minScore, limit, offset)
			if err != nil {
				return errResult("%v", err)
			}
			type scoredArticle struct {
				herald.Article
				InterestScore float64 `json:"interest_score"`
			}
			result := make([]scoredArticle, len(articles))
			for i, a := range articles {
				a.Content = ""
				score := 0.0
				if i < len(scores) {
					score = scores[i]
				}
				result[i] = scoredArticle{Article: a, InterestScore: score}
			}
			log.Printf("articles_unread: limit=%d min_score=%.1f -> %d results", limit, minScore, len(result))
			return jsonResult(result)
		}

		articles, err := hs.engine.GetUnreadArticles(userID, limit, offset)
		if err != nil {
			return errResult("%v", err)
		}
		for i := range articles {
			articles[i].Content = ""
		}
		log.Printf("articles_unread: limit=%d -> %d results", limit, len(articles))
		return jsonResult(articles)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "articles_get",
		Description: "Get full article content by ID. Use this to read the complete text of an article for follow-up discussion or analysis.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input articleIDInput) (*mcp.CallToolResult, any, error) {
		if input.ArticleID == 0 {
			return errResult("article_id parameter is required")
		}
		userID := hs.resolveUser(ptrStr(input.Speaker))
		article, err := hs.engine.GetArticleForUser(userID, input.ArticleID)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("articles_get: id=%d", input.ArticleID)
		return jsonResult(article)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "articles_mark_read",
		Description: "Mark an article as read. Call this after discussing or presenting an article to the user.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input articleIDInput) (*mcp.CallToolResult, any, error) {
		if input.ArticleID == 0 {
			return errResult("article_id parameter is required")
		}
		userID := hs.resolveUser(ptrStr(input.Speaker))
		if err := hs.engine.MarkArticleRead(userID, input.ArticleID); err != nil {
			return errResult("%v", err)
		}
		log.Printf("articles_mark_read: id=%d", input.ArticleID)
		return textResult("Article %d marked as read.", input.ArticleID)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "feeds_list",
		Description: "List all subscribed RSS/Atom feeds with their titles, URLs, and last fetch times.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input speakerOnlyInput) (*mcp.CallToolResult, any, error) {
		userID := hs.resolveUser(ptrStr(input.Speaker))
		feeds, err := hs.engine.GetUserFeeds(userID)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("feeds_list: %d feeds", len(feeds))
		return jsonResult(feeds)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "feed_subscribe",
		Description: "Subscribe to an RSS/Atom feed by URL. Creates the feed if new, then subscribes the user. Find the feed URL first (e.g. via web search) before calling this.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input feedSubscribeInput) (*mcp.CallToolResult, any, error) {
		if input.URL == "" {
			return errResult("url parameter is required")
		}
		userID := hs.resolveUser(ptrStr(input.Speaker))
		title := ""
		if input.Title != nil {
			title = *input.Title
		}
		if err := hs.engine.SubscribeFeed(userID, input.URL, title); err != nil {
			return errResult("%v", err)
		}
		log.Printf("feed_subscribe: url=%s title=%q", input.URL, title)
		return textResult("Subscribed to %s", input.URL)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "feed_unsubscribe",
		Description: "Unsubscribe from a feed by ID. Use feeds_list to find the feed ID. If no other users subscribe to it, the feed and its articles are deleted.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input feedIDInput) (*mcp.CallToolResult, any, error) {
		if input.FeedID == 0 {
			return errResult("feed_id parameter is required")
		}
		userID := hs.resolveUser(ptrStr(input.Speaker))
		if err := hs.engine.UnsubscribeFeed(userID, input.FeedID); err != nil {
			return errResult("%v", err)
		}
		log.Printf("feed_unsubscribe: feed_id=%d", input.FeedID)
		return textResult("Unsubscribed from feed %d.", input.FeedID)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "feed_rename",
		Description: "Rename a feed's display title. Use feeds_list to find the feed ID.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input feedRenameInput) (*mcp.CallToolResult, any, error) {
		if input.FeedID == 0 {
			return errResult("feed_id parameter is required")
		}
		if input.Title == "" {
			return errResult("title parameter is required")
		}
		if err := hs.engine.RenameFeed(input.FeedID, input.Title); err != nil {
			return errResult("%v", err)
		}
		log.Printf("feed_rename: feed_id=%d title=%q", input.FeedID, input.Title)
		return textResult("Feed %d renamed to %q.", input.FeedID, input.Title)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "article_groups",
		Description: "List article groups (clusters of articles covering the same event or topic). Each group has a topic label, article count, and max interest score. Use this for briefings to present related coverage together.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input speakerOnlyInput) (*mcp.CallToolResult, any, error) {
		userID := hs.resolveUser(ptrStr(input.Speaker))
		groups, err := hs.engine.GetUserGroups(userID)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("article_groups: %d groups", len(groups))
		return jsonResult(groups)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "article_group_get",
		Description: "Get a specific article group with all its articles and scores. Use this to drill into a topic cluster and see all related coverage.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input articleGroupGetInput) (*mcp.CallToolResult, any, error) {
		if input.GroupID == 0 {
			return errResult("group_id parameter is required")
		}
		group, err := hs.engine.GetGroupArticles(input.GroupID)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("article_group_get: id=%d", input.GroupID)
		return jsonResult(group)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "feed_stats",
		Description: "Get article statistics per feed and totals: total articles, unread count, and unsummarized count. Use this to understand pipeline health and coverage.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input speakerOnlyInput) (*mcp.CallToolResult, any, error) {
		userID := hs.resolveUser(ptrStr(input.Speaker))
		stats, err := hs.engine.GetFeedStats(userID)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("feed_stats")
		return jsonResult(stats)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "poll_now",
		Description: "Trigger an immediate feed poll cycle: fetch all feeds, score new articles through the AI pipeline, and return results. Only available when the server is running with --poll. Use this when the user asks to check for new articles right now.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input emptyInput) (*mcp.CallToolResult, any, error) {
		if hs.poller == nil {
			return errResult("polling is not enabled (start with --poll)")
		}
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		result, err := hs.poller.poll(pollCtx)
		if err != nil {
			return errResult("poll failed: %v", err)
		}
		log.Printf("poll_now: %d/%d feeds, %d new, %d scored, %d high-interest",
			result.FeedsDownloaded, result.FeedsTotal,
			result.NewArticles, result.ProcessedCount, result.HighInterest)
		return jsonResult(result)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "preferences_get",
		Description: "Get all user preferences as structured JSON. Returns keywords, interest threshold, and notification settings.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input speakerOnlyInput) (*mcp.CallToolResult, any, error) {
		userID := hs.resolveUser(ptrStr(input.Speaker))
		prefs, err := hs.engine.GetPreferences(userID)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("preferences_get")
		return jsonResult(prefs)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "preference_set",
		Description: "Set a single user preference by key. Valid keys: keywords (JSON array), interest_threshold (number), notify_when (\"present\"|\"always\"|\"queue\"), notify_min_score (number).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input preferenceSetInput) (*mcp.CallToolResult, any, error) {
		if input.Key == "" {
			return errResult("key parameter is required")
		}
		if input.Value == "" {
			return errResult("value parameter is required")
		}
		userID := hs.resolveUser(ptrStr(input.Speaker))
		if err := hs.engine.SetPreference(userID, input.Key, input.Value); err != nil {
			return errResult("%v", err)
		}
		log.Printf("preference_set: %s=%s", input.Key, input.Value)
		return textResult("Preference %q set.", input.Key)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "prompts_list",
		Description: "List all customizable prompt types with their current status (custom or default) and temperature.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input speakerOnlyInput) (*mcp.CallToolResult, any, error) {
		userID := hs.resolveUser(ptrStr(input.Speaker))
		prompts, err := hs.engine.ListPrompts(userID)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("prompts_list: %d types", len(prompts))
		return jsonResult(prompts)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "prompt_get",
		Description: "Get the active prompt template and temperature for a given type. Prompt types: curation, summarization, group_summary, related_groups.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input promptTypeInput) (*mcp.CallToolResult, any, error) {
		if input.PromptType == "" {
			return errResult("prompt_type parameter is required")
		}
		if input.PromptType == "security" {
			return errResult("the security prompt type cannot be viewed or modified")
		}
		userID := hs.resolveUser(ptrStr(input.Speaker))
		detail, err := hs.engine.GetPrompt(userID, input.PromptType)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("prompt_get: type=%s custom=%v", input.PromptType, detail.IsCustom)
		return jsonResult(detail)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "prompt_set",
		Description: "Customize a prompt template and/or temperature. At least one of template or temperature must be provided. Prompt types: curation, summarization, group_summary, related_groups.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input promptSetInput) (*mcp.CallToolResult, any, error) {
		if input.PromptType == "" {
			return errResult("prompt_type parameter is required")
		}
		if input.PromptType == "security" {
			return errResult("the security prompt type cannot be viewed or modified")
		}
		template := ""
		if input.Template != nil {
			template = *input.Template
		}
		if template == "" && input.Temperature == nil {
			return errResult("at least one of template or temperature is required")
		}
		userID := hs.resolveUser(ptrStr(input.Speaker))
		if err := hs.engine.SetPrompt(userID, input.PromptType, template, input.Temperature); err != nil {
			return errResult("%v", err)
		}
		log.Printf("prompt_set: type=%s", input.PromptType)
		return textResult("Prompt %q updated.", input.PromptType)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "prompt_reset",
		Description: "Revert a prompt type to its embedded default. Deletes any custom template and temperature.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input promptTypeInput) (*mcp.CallToolResult, any, error) {
		if input.PromptType == "" {
			return errResult("prompt_type parameter is required")
		}
		if input.PromptType == "security" {
			return errResult("the security prompt type cannot be viewed or modified")
		}
		userID := hs.resolveUser(ptrStr(input.Speaker))
		if err := hs.engine.ResetPrompt(userID, input.PromptType); err != nil {
			return errResult("%v", err)
		}
		log.Printf("prompt_reset: type=%s", input.PromptType)
		return textResult("Prompt %q reset to default.", input.PromptType)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "briefing",
		Description: "Generate a markdown briefing from high-interest unread articles. Includes titles, scores, URLs, and AI summaries.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input speakerOnlyInput) (*mcp.CallToolResult, any, error) {
		userID := hs.resolveUser(ptrStr(input.Speaker))
		briefing, err := hs.engine.GenerateBriefing(userID)
		if err != nil {
			return errResult("%v", err)
		}
		if briefing == "" {
			return textResult("No high-interest unread articles for a briefing.")
		}
		log.Printf("briefing: generated")
		return textResult("%s", briefing)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "article_star",
		Description: "Set or clear the starred flag on an article. Starred articles are saved for later reference.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input articleStarInput) (*mcp.CallToolResult, any, error) {
		if input.ArticleID == 0 {
			return errResult("article_id parameter is required")
		}
		if input.Starred == nil {
			return errResult("starred parameter is required")
		}
		userID := hs.resolveUser(ptrStr(input.Speaker))
		if err := hs.engine.StarArticle(userID, input.ArticleID, *input.Starred); err != nil {
			return errResult("%v", err)
		}
		action := "starred"
		if !*input.Starred {
			action = "unstarred"
		}
		log.Printf("article_star: id=%d %s", input.ArticleID, action)
		return textResult("Article %d %s.", input.ArticleID, action)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "filter_rules_list",
		Description: "List filter rules for the user. Optionally filter by feed_id to see rules scoped to a specific feed plus global rules.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input filterRulesListInput) (*mcp.CallToolResult, any, error) {
		userID := hs.resolveUser(ptrStr(input.Speaker))
		rules, err := hs.engine.GetFilterRules(userID, input.FeedID)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("filter_rules_list: %d rules", len(rules))
		return jsonResult(rules)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "filter_rule_add",
		Description: "Add a filter rule. Rules score articles by author, category, or tag. Positive scores boost, negative penalize. Use feed_metadata to discover available values first.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input filterRuleAddInput) (*mcp.CallToolResult, any, error) {
		if input.Axis == "" {
			return errResult("axis parameter is required")
		}
		if input.Value == "" {
			return errResult("value parameter is required")
		}
		userID := hs.resolveUser(ptrStr(input.Speaker))
		rule := herald.FilterRule{
			FeedID: input.FeedID,
			Axis:   input.Axis,
			Value:  input.Value,
			Score:  input.Score,
		}
		id, err := hs.engine.AddFilterRule(userID, rule)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("filter_rule_add: id=%d axis=%s value=%q score=%d", id, input.Axis, input.Value, input.Score)
		return jsonResult(map[string]any{"id": id, "axis": input.Axis, "value": input.Value, "score": input.Score})
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "filter_rule_update",
		Description: "Update the score of an existing filter rule by ID. Use filter_rules_list to find rule IDs.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input filterRuleUpdateInput) (*mcp.CallToolResult, any, error) {
		if input.RuleID == 0 {
			return errResult("rule_id parameter is required")
		}
		if err := hs.engine.UpdateFilterRule(input.RuleID, input.Score); err != nil {
			return errResult("%v", err)
		}
		log.Printf("filter_rule_update: id=%d score=%d", input.RuleID, input.Score)
		return textResult("Filter rule %d updated to score %d.", input.RuleID, input.Score)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "filter_rule_delete",
		Description: "Delete a filter rule by ID. Use filter_rules_list to find rule IDs.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input filterRuleDeleteInput) (*mcp.CallToolResult, any, error) {
		if input.RuleID == 0 {
			return errResult("rule_id parameter is required")
		}
		if err := hs.engine.DeleteFilterRule(input.RuleID); err != nil {
			return errResult("%v", err)
		}
		log.Printf("filter_rule_delete: id=%d", input.RuleID)
		return textResult("Filter rule %d deleted.", input.RuleID)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "feed_metadata",
		Description: "Discover authors and categories from a feed's articles. Use this to find values for creating filter rules. Requires feed_id from feeds_list.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input feedMetadataInput) (*mcp.CallToolResult, any, error) {
		if input.FeedID == 0 {
			return errResult("feed_id parameter is required")
		}
		meta, err := hs.engine.GetFeedMetadata(input.FeedID)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("feed_metadata: feed_id=%d authors=%d categories=%d", input.FeedID, len(meta.Authors), len(meta.Categories))
		return jsonResult(meta)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "user_register",
		Description: "Register a speaker name as a herald user. Returns the new user's ID. Use this when a new household member wants their own feeds, preferences, and read state.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input userRegisterInput) (*mcp.CallToolResult, any, error) {
		if input.Name == "" {
			return errResult("name parameter is required")
		}
		id, err := hs.engine.RegisterUser(input.Name)
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("user_register: name=%q id=%d", input.Name, id)
		return jsonResult(map[string]any{"id": id, "name": input.Name})
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "user_list",
		Description: "List all registered herald users.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input emptyInput) (*mcp.CallToolResult, any, error) {
		users, err := hs.engine.ListUsers()
		if err != nil {
			return errResult("%v", err)
		}
		log.Printf("user_list: %d users", len(users))
		return jsonResult(users)
	})
}
