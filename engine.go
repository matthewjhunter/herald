package herald

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/feeds"
	"github.com/matthewjhunter/herald/internal/storage"
)

// Engine is the public API for herald's content processing pipeline.
// It wraps the internal storage, feed fetcher, and AI processor.
type Engine struct {
	store   storage.Store
	fetcher *feeds.Fetcher
	ai      *ai.AIProcessor
	config  *storage.Config
	mu      sync.RWMutex // protects config fields modified at runtime
}

// NewEngine creates a herald content engine backed by the given SQLite database.
// The AI processor is created eagerly but only contacts Ollama when called.
// If OllamaBaseURL is empty, AI processing is disabled; feed fetching and
// reading still work normally.
func NewEngine(cfg EngineConfig) (*Engine, error) {
	if cfg.SecurityModel == "" {
		cfg.SecurityModel = "gemma3:4b"
	}
	if cfg.CurationModel == "" {
		cfg.CurationModel = "llama3"
	}
	if cfg.InterestThreshold == 0 {
		cfg.InterestThreshold = 8.0
	}
	if cfg.SecurityThreshold == 0 {
		cfg.SecurityThreshold = 7.0
	}

	store, err := storage.NewSQLiteStore(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	storeCfg := storage.DefaultConfig()
	if cfg.OllamaBaseURL != "" {
		storeCfg.Ollama.BaseURL = cfg.OllamaBaseURL
	}
	storeCfg.Ollama.SecurityModel = cfg.SecurityModel
	storeCfg.Ollama.CurationModel = cfg.CurationModel
	storeCfg.Thresholds.InterestScore = cfg.InterestThreshold
	storeCfg.Thresholds.SecurityScore = cfg.SecurityThreshold
	storeCfg.Preferences.Keywords = cfg.Keywords

	// Fetcher is always created; it is a stateless HTTP client wrapper with no
	// background goroutines.  Background polling (FetchAllFeeds) is only called
	// by the daemon process and is separately guarded by ReadOnly.
	fetcher := feeds.NewFetcher(store)

	var processor *ai.AIProcessor
	if !cfg.ReadOnly && cfg.OllamaBaseURL != "" {
		processor, err = ai.NewAIProcessor(
			cfg.OllamaBaseURL, cfg.SecurityModel, cfg.CurationModel,
			store, storeCfg,
		)
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("create AI processor: %w", err)
		}
	}

	e := &Engine{
		store:   store,
		fetcher: fetcher,
		ai:      processor,
		config:  storeCfg,
	}

	// Overlay DB-stored preferences onto config (DB takes precedence over CLI flags).
	if cfg.UserID > 0 {
		if prefs, err := store.GetAllUserPreferences(cfg.UserID); err == nil {
			if v, ok := prefs["keywords"]; ok {
				var kw []string
				if json.Unmarshal([]byte(v), &kw) == nil {
					storeCfg.Preferences.Keywords = kw
				}
			}
			if v, ok := prefs["interest_threshold"]; ok {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					storeCfg.Thresholds.InterestScore = f
				}
			}
		}
	}

	return e, nil
}

// FetchAllFeeds fetches all subscribed feeds and stores new articles.
func (e *Engine) FetchAllFeeds(ctx context.Context) (*FetchResult, error) {
	if e.fetcher == nil {
		return nil, fmt.Errorf("feed fetching not available in read-only mode")
	}
	stats, err := e.fetcher.FetchAllFeeds(ctx)
	if err != nil {
		return nil, err
	}
	return &FetchResult{
		FeedsTotal:       stats.FeedsTotal,
		FeedsDownloaded:  stats.FeedsDownloaded,
		FeedsNotModified: stats.FeedsNotModified,
		FeedsErrored:     stats.FeedsErrored,
		NewArticles:      stats.NewArticles,
	}, nil
}

// ProcessNewArticles runs the AI pipeline (summarize, security check, interest
// scoring) on unscored articles for the given user. Returns scored articles.
// Articles that fail individual AI steps are skipped, not fatal.
func (e *Engine) ProcessNewArticles(ctx context.Context, userID int64) ([]ScoredArticle, error) {
	if e.ai == nil {
		return nil, nil
	}
	articles, err := e.store.GetUnscoredArticlesForUser(userID, 100)
	if err != nil {
		return nil, fmt.Errorf("get unscored articles: %w", err)
	}

	var scored []ScoredArticle
	for _, article := range articles {
		content := article.Content
		if content == "" {
			content = article.Summary
		}

		// Summarize (cached per-user)
		existing, _ := e.store.GetArticleSummary(userID, article.ID)
		if existing == nil {
			summary, err := e.ai.SummarizeArticle(ctx, userID, article.Title, content)
			if err != nil {
				log.Printf("herald: summarization failed for article %d: %v", article.ID, err)
				continue
			}
			e.store.UpdateArticleAISummary(userID, article.ID, summary)
		}

		// Security check
		secResult, err := e.ai.SecurityCheck(ctx, userID, article.Title, content)
		if err != nil {
			log.Printf("herald: security check failed for article %d: %v", article.ID, err)
			continue
		}

		if !secResult.Safe || secResult.Score < e.config.Thresholds.SecurityScore {
			secScore := secResult.Score
			zero := 0.0
			e.store.UpdateReadState(userID, article.ID, false, &zero, &secScore)
			continue
		}

		// Interest scoring
		curResult, err := e.ai.CurateArticle(ctx, userID, article.Title, content, e.config.Preferences.Keywords)
		if err != nil {
			log.Printf("herald: curation failed for article %d: %v", article.ID, err)
			continue
		}

		secScore := secResult.Score
		interestScore := curResult.InterestScore
		e.store.UpdateReadState(userID, article.ID, false, &interestScore, &secScore)

		// Group management
		userGroups, _ := e.store.GetUserGroups(userID)
		relatedGroupIDs, _ := e.ai.FindRelatedGroups(ctx, userID, article, userGroups, e.store)
		if len(relatedGroupIDs) > 0 {
			e.store.AddArticleToGroup(relatedGroupIDs[0], article.ID)
		} else {
			topic := article.Title
			if len(topic) > 100 {
				topic = topic[:100]
			}
			if newGroupID, err := e.store.CreateArticleGroup(userID, topic); err == nil {
				e.store.AddArticleToGroup(newGroupID, article.ID)
			}
		}

		scored = append(scored, ScoredArticle{
			Article:       articleFromInternal(article),
			InterestScore: interestScore,
			SecurityScore: secScore,
			Safe:          true,
		})
	}

	return scored, nil
}

// GetUnreadArticles returns unread articles for a user, up to limit starting at offset.
func (e *Engine) GetUnreadArticles(userID int64, limit, offset int) ([]Article, error) {
	articles, err := e.store.GetUnreadArticlesForUser(userID, limit, offset, e.resolveFilterThreshold(userID))
	if err != nil {
		return nil, err
	}
	return articlesFromInternal(articles), nil
}

// GetStarredArticles returns starred articles for a user.
func (e *Engine) GetStarredArticles(userID int64, limit, offset int) ([]Article, error) {
	articles, err := e.store.GetStarredArticles(userID, limit, offset, e.resolveFilterThreshold(userID))
	if err != nil {
		return nil, err
	}
	return articlesFromInternal(articles), nil
}

// GetUnreadArticlesByFeed returns unread articles for a user filtered to a specific feed.
func (e *Engine) GetUnreadArticlesByFeed(userID, feedID int64, limit, offset int) ([]Article, error) {
	articles, err := e.store.GetUnreadArticlesByFeed(userID, feedID, limit, offset, e.resolveFilterThreshold(userID))
	if err != nil {
		return nil, err
	}
	return articlesFromInternal(articles), nil
}

// GetArticle returns a single article by ID.
func (e *Engine) GetArticle(articleID int64) (*Article, error) {
	a, err := e.store.GetArticle(articleID)
	if err != nil {
		return nil, err
	}
	result := articleFromInternal(*a)
	return &result, nil
}

// GetArticleForUser returns a single article enriched with its AI summary for the given user.
func (e *Engine) GetArticleForUser(userID, articleID int64) (*Article, error) {
	a, err := e.store.GetArticle(articleID)
	if err != nil {
		return nil, err
	}
	result := articleFromInternal(*a)
	if summary, err := e.store.GetArticleSummary(userID, articleID); err == nil && summary != nil {
		result.AISummary = summary.AISummary
	}
	return &result, nil
}

// GetHighInterestArticles returns unread articles scored above the threshold.
func (e *Engine) GetHighInterestArticles(userID int64, threshold float64, limit, offset int) ([]Article, []float64, error) {
	articles, scores, err := e.store.GetArticlesByInterestScore(userID, threshold, limit, offset, e.resolveFilterThreshold(userID))
	if err != nil {
		return nil, nil, err
	}
	return articlesFromInternal(articles), scores, nil
}

// MarkArticleRead marks an article as read.
func (e *Engine) MarkArticleRead(userID, articleID int64) error {
	return e.store.UpdateReadState(userID, articleID, true, nil, nil)
}

// MarkArticlesRead marks a list of articles as read.
func (e *Engine) MarkArticlesRead(userID int64, articleIDs []int64) error {
	for _, id := range articleIDs {
		if err := e.store.UpdateReadState(userID, id, true, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// ImportOPML imports feeds from an OPML file and subscribes the user.
func (e *Engine) ImportOPML(path string, userID int64) error {
	return e.fetcher.ImportOPML(path, userID)
}

// ImportOPMLReader imports feeds from an OPML reader and subscribes the user.
func (e *Engine) ImportOPMLReader(r io.Reader, userID int64) error {
	return e.fetcher.ImportOPMLReader(r, userID)
}

// GetUserFeeds returns all feeds a user is subscribed to.
func (e *Engine) GetUserFeeds(userID int64) ([]Feed, error) {
	feeds, err := e.store.GetUserFeeds(userID)
	if err != nil {
		return nil, err
	}
	return feedsFromInternal(feeds), nil
}

// SubscribeFeed adds a feed and subscribes the user to it.
// Validates the URL by fetching the feed first; returns an error if the URL
// is unreachable or not a valid RSS/Atom feed.
func (e *Engine) SubscribeFeed(userID int64, url, title string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Validate by fetching — catches bad URLs, non-feed pages, timeouts.
	// No cache headers for a brand-new subscription.
	result, err := e.fetcher.FetchFeed(ctx, storage.Feed{URL: url})
	if err != nil {
		return fmt.Errorf("validate feed: %w", err)
	}

	// Use the feed's own title if none provided
	if title == "" && result.Feed.Title != "" {
		title = result.Feed.Title
	}
	if title == "" {
		title = url
	}

	feedID, err := e.store.AddFeed(url, title, result.Feed.Description)
	if err != nil {
		return fmt.Errorf("add feed: %w", err)
	}

	// Store the initial articles we already fetched
	if stored, err := e.fetcher.StoreArticles(feedID, result.Feed); err == nil && stored > 0 {
		log.Printf("herald: stored %d initial articles from %s", stored, url)
	}

	// Persist cache headers for next conditional request
	if result.ETag != "" || result.LastModified != "" {
		e.store.UpdateFeedCacheHeaders(feedID, result.ETag, result.LastModified)
	}

	e.store.MarkFeedFetched(feedID)

	return e.store.SubscribeUserToFeed(userID, feedID)
}

// DiscoverFeeds fetches pageURL and returns any feeds found via standard
// autodiscovery (<link rel="alternate"> in HTML <head>). If pageURL is
// itself a valid feed it is returned as the sole result. Returns an empty
// slice (not an error) when no feeds are found.
func (e *Engine) DiscoverFeeds(ctx context.Context, pageURL string) ([]DiscoveredFeed, error) {
	internal, err := e.fetcher.DiscoverFeeds(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	out := make([]DiscoveredFeed, len(internal))
	for i, f := range internal {
		out[i] = DiscoveredFeed{URL: f.URL, Title: f.Title, Type: f.Type}
	}
	return out, nil
}

// opml XML types for feed export.
type opmlExport struct {
	XMLName xml.Name `xml:"opml"`
	Version string   `xml:"version,attr"`
	Head    opmlHead `xml:"head"`
	Body    opmlBody `xml:"body"`
}

type opmlHead struct {
	Title       string `xml:"title"`
	DateCreated string `xml:"dateCreated"`
}

type opmlBody struct {
	Outlines []opmlOutline `xml:"outline"`
}

type opmlOutline struct {
	Text   string `xml:"text,attr"`
	Title  string `xml:"title,attr"`
	Type   string `xml:"type,attr"`
	XMLURL string `xml:"xmlUrl,attr"`
}

func marshalOPML(title string, feeds []storage.Feed) ([]byte, error) {
	outlines := make([]opmlOutline, len(feeds))
	for i, f := range feeds {
		outlines[i] = opmlOutline{
			Text:   f.Title,
			Title:  f.Title,
			Type:   "rss",
			XMLURL: f.URL,
		}
	}
	doc := opmlExport{
		Version: "2.0",
		Head:    opmlHead{Title: title, DateCreated: time.Now().UTC().Format(time.RFC1123)},
		Body:    opmlBody{Outlines: outlines},
	}
	data, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), data...), nil
}

// ExportOPML returns an OPML 2.0 document containing all feeds the given user
// is subscribed to.
func (e *Engine) ExportOPML(userID int64) ([]byte, error) {
	feeds, err := e.store.GetUserFeeds(userID)
	if err != nil {
		return nil, fmt.Errorf("get user feeds: %w", err)
	}
	return marshalOPML("Herald Subscriptions", feeds)
}

// ExportAllFeedsOPML returns an OPML 2.0 document containing every feed
// subscribed to by any user. Intended for admin use.
func (e *Engine) ExportAllFeedsOPML() ([]byte, error) {
	feeds, err := e.store.GetAllActiveSubscribedFeeds()
	if err != nil {
		return nil, fmt.Errorf("get all feeds: %w", err)
	}
	return marshalOPML("Herald - All Subscriptions", feeds)
}

// UnsubscribeFeed removes a user's subscription to a feed. If no subscribers
// remain, the feed and its articles are deleted (via FK CASCADE).
func (e *Engine) UnsubscribeFeed(userID, feedID int64) error {
	if err := e.store.UnsubscribeUserFromFeed(userID, feedID); err != nil {
		return fmt.Errorf("unsubscribe: %w", err)
	}
	if deleted, err := e.store.DeleteFeedIfOrphaned(feedID); err != nil {
		return fmt.Errorf("cleanup orphaned feed: %w", err)
	} else if deleted {
		log.Printf("herald: deleted orphaned feed %d", feedID)
	}
	return nil
}

// RenameFeed updates the display title of a feed.
func (e *Engine) RenameFeed(feedID int64, title string) error {
	return e.store.RenameFeed(feedID, title)
}

// GetUserGroups returns all article groups for a user.
func (e *Engine) GetUserGroups(userID int64) ([]ArticleGroup, error) {
	groups, err := e.store.GetUserGroups(userID)
	if err != nil {
		return nil, err
	}
	var result []ArticleGroup
	for _, g := range groups {
		ag := ArticleGroup{
			ID:        g.ID,
			UserID:    g.UserID,
			Topic:     g.Topic,
			CreatedAt: g.CreatedAt,
			UpdatedAt: g.UpdatedAt,
		}
		// Attach summary if available
		if gs, err := e.store.GetGroupSummary(g.ID); err == nil && gs != nil {
			ag.Summary = gs.Summary
			ag.Count = gs.ArticleCount
			if gs.MaxInterestScore != nil {
				ag.MaxScore = *gs.MaxInterestScore
			}
		}
		result = append(result, ag)
	}
	return result, nil
}

// GetGroupArticles returns the articles in a specific group with their scores.
func (e *Engine) GetGroupArticles(groupID int64) (*ArticleGroup, error) {
	group, err := e.store.GetGroup(groupID)
	if err != nil {
		return nil, fmt.Errorf("get group: %w", err)
	}

	articles, err := e.store.GetGroupArticles(groupID)
	if err != nil {
		return nil, fmt.Errorf("get group articles: %w", err)
	}

	ag := &ArticleGroup{
		Articles: articlesFromInternal(articles),
		Count:    len(articles),
	}

	if group != nil {
		ag.ID = group.ID
		ag.UserID = group.UserID
		ag.Topic = group.Topic
		ag.CreatedAt = group.CreatedAt
		ag.UpdatedAt = group.UpdatedAt
	}

	// Attach summary
	if gs, err := e.store.GetGroupSummary(groupID); err == nil && gs != nil {
		ag.Summary = gs.Summary
		if gs.MaxInterestScore != nil {
			ag.MaxScore = *gs.MaxInterestScore
		}
	}

	return ag, nil
}

// GenerateBriefing creates a text briefing from high-interest unread articles.
func (e *Engine) GenerateBriefing(userID int64) (string, error) {
	if e.ai == nil {
		return "", nil
	}
	articles, scores, err := e.store.GetArticlesByInterestScore(
		userID, e.config.Thresholds.InterestScore, 20, 0, nil)
	if err != nil {
		return "", fmt.Errorf("get high-interest articles: %w", err)
	}
	if len(articles) == 0 {
		return "", nil
	}

	var briefing strings.Builder
	for i, article := range articles {
		score := 0.0
		if i < len(scores) {
			score = scores[i]
		}

		fmt.Fprintf(&briefing, "## %s (%.1f/10)\n", article.Title, score)
		fmt.Fprintf(&briefing, "%s\n", article.URL)

		if summary, err := e.store.GetArticleSummary(userID, article.ID); err == nil && summary != nil {
			fmt.Fprintf(&briefing, "%s\n", summary.AISummary)
		} else if article.Summary != "" {
			fmt.Fprintf(&briefing, "%s\n", article.Summary)
		}
		briefing.WriteString("\n")
	}

	return briefing.String(), nil
}

// GetFeedStats returns per-feed article counts and an aggregate total for a user.
func (e *Engine) GetFeedStats(userID int64) (*FeedStatsResult, error) {
	internal, err := e.store.GetFeedStats(userID)
	if err != nil {
		return nil, err
	}
	result := &FeedStatsResult{
		Feeds: make([]FeedStats, len(internal)),
	}
	for i, fs := range internal {
		result.Feeds[i] = FeedStats{
			FeedID:               fs.FeedID,
			FeedTitle:            fs.FeedTitle,
			TotalArticles:        fs.TotalArticles,
			UnreadArticles:       fs.UnreadArticles,
			UnsummarizedArticles: fs.UnsummarizedArticles,
			LastPostDate:         fs.LastPostDate,
		}
		result.Total.TotalArticles += fs.TotalArticles
		result.Total.UnreadArticles += fs.UnreadArticles
		result.Total.UnsummarizedArticles += fs.UnsummarizedArticles
	}
	return result, nil
}

// PendingCounts returns the number of articles awaiting AI processing.
func (e *Engine) PendingCounts(userID int64) (unsummarized, unscored int, err error) {
	unsummarized, err = e.store.GetUnsummarizedArticleCount(userID)
	if err != nil {
		return 0, 0, err
	}
	unscored, err = e.store.GetUnscoredArticleCount(userID)
	if err != nil {
		return 0, 0, err
	}
	return unsummarized, unscored, nil
}

// allowedPromptTypes lists prompt types that can be read/written via MCP.
// "security" is intentionally excluded — the LLM must not weaken content safety.
var allowedPromptTypes = map[string]bool{
	"curation":       true,
	"summarization":  true,
	"group_summary":  true,
	"related_groups": true,
}

// GetUserPreference returns a single raw preference value for a user.
func (e *Engine) GetUserPreference(userID int64, key string) (string, error) {
	return e.store.GetUserPreference(userID, key)
}

// SetUserPreference stores a single raw preference value for a user.
// Unlike SetPreference, this bypasses the MCP allowed-keys restriction
// and is intended for internal/system preferences like opml_sync_token.
func (e *Engine) SetUserPreference(userID int64, key, value string) error {
	return e.store.SetUserPreference(userID, key, value)
}

// allowedPreferenceKeys lists preference keys that can be set via MCP.
var allowedPreferenceKeys = map[string]bool{
	"keywords":           true,
	"interest_threshold": true,
	"filter_threshold":   true,
	"notify_when":        true,
	"notify_min_score":   true,
}

// allowedFilterAxes are the valid axis values for filter rules.
var allowedFilterAxes = map[string]bool{
	"author":   true,
	"category": true,
	"tag":      true,
}

// GetPreferences returns all user preferences, merging DB values over config defaults.
func (e *Engine) GetPreferences(userID int64) (*UserPreferences, error) {
	prefs := &UserPreferences{
		InterestThreshold: e.config.Thresholds.InterestScore,
		NotifyWhen:        "present",
		NotifyMinScore:    7.0,
	}

	e.mu.RLock()
	prefs.Keywords = append([]string{}, e.config.Preferences.Keywords...)
	e.mu.RUnlock()

	dbPrefs, err := e.store.GetAllUserPreferences(userID)
	if err != nil {
		return prefs, nil // return defaults on error
	}

	if v, ok := dbPrefs["keywords"]; ok {
		var kw []string
		if json.Unmarshal([]byte(v), &kw) == nil {
			prefs.Keywords = kw
		}
	}
	if v, ok := dbPrefs["interest_threshold"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			prefs.InterestThreshold = f
		}
	}
	if v, ok := dbPrefs["filter_threshold"]; ok {
		if i, err := strconv.Atoi(v); err == nil {
			prefs.FilterThreshold = i
		}
	}
	if v, ok := dbPrefs["notify_when"]; ok {
		prefs.NotifyWhen = v
	}
	if v, ok := dbPrefs["notify_min_score"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			prefs.NotifyMinScore = f
		}
	}

	return prefs, nil
}

// SetPreference validates and stores a single preference, updating runtime config
// for keys that affect scoring (keywords, interest_threshold).
func (e *Engine) SetPreference(userID int64, key, value string) error {
	if !allowedPreferenceKeys[key] {
		return fmt.Errorf("unknown preference key: %q", key)
	}

	// Validate value by type
	switch key {
	case "keywords":
		var kw []string
		if err := json.Unmarshal([]byte(value), &kw); err != nil {
			return fmt.Errorf("keywords must be a JSON array of strings: %w", err)
		}
	case "interest_threshold", "notify_min_score":
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return fmt.Errorf("%s must be a number: %w", key, err)
		}
	case "filter_threshold":
		if _, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("filter_threshold must be an integer: %w", err)
		}
	case "notify_when":
		switch value {
		case "present", "always", "queue":
		default:
			return fmt.Errorf("notify_when must be \"present\", \"always\", or \"queue\"")
		}
	}

	if err := e.store.SetUserPreference(userID, key, value); err != nil {
		return err
	}

	// Update runtime config for scoring-affecting keys
	e.mu.Lock()
	defer e.mu.Unlock()
	switch key {
	case "keywords":
		var kw []string
		json.Unmarshal([]byte(value), &kw) // already validated above
		e.config.Preferences.Keywords = kw
	case "interest_threshold":
		f, _ := strconv.ParseFloat(value, 64) // already validated above
		e.config.Thresholds.InterestScore = f
	}

	return nil
}

// ListPrompts returns all customizable prompt types with their current status.
func (e *Engine) ListPrompts(userID int64) ([]PromptInfo, error) {
	customPrompts, err := e.store.ListUserPrompts(userID)
	if err != nil {
		return nil, err
	}

	customMap := make(map[string]*storage.UserPrompt)
	for i := range customPrompts {
		customMap[customPrompts[i].PromptType] = &customPrompts[i]
	}

	promptLoader := ai.NewPromptLoader(e.store, e.config)

	var result []PromptInfo
	for pt := range allowedPromptTypes {
		info := PromptInfo{
			Type:        pt,
			Status:      "default",
			Temperature: promptLoader.GetTemperature(userID, ai.PromptType(pt)),
		}
		if _, ok := customMap[pt]; ok {
			info.Status = "custom"
		}
		result = append(result, info)
	}

	return result, nil
}

// GetPrompt returns the effective prompt template for a type.
func (e *Engine) GetPrompt(userID int64, promptType string) (*PromptDetail, error) {
	if !allowedPromptTypes[promptType] {
		return nil, fmt.Errorf("unknown or restricted prompt type: %q", promptType)
	}

	promptLoader := ai.NewPromptLoader(e.store, e.config)

	template, err := promptLoader.GetPrompt(userID, ai.PromptType(promptType))
	if err != nil {
		return nil, err
	}

	temperature := promptLoader.GetTemperature(userID, ai.PromptType(promptType))

	// Determine if this is a custom prompt
	isCustom := false
	if _, dbErr := e.store.GetUserPrompt(userID, promptType); dbErr == nil {
		isCustom = true
	}

	return &PromptDetail{
		Type:        promptType,
		Template:    template,
		Temperature: temperature,
		IsCustom:    isCustom,
	}, nil
}

// SetPrompt customizes a prompt template and/or temperature.
func (e *Engine) SetPrompt(userID int64, promptType, template string, temp *float64) error {
	if !allowedPromptTypes[promptType] {
		return fmt.Errorf("unknown or restricted prompt type: %q", promptType)
	}

	// If only temperature is being set, we need to fetch the existing template
	if template == "" {
		existing, err := e.store.GetUserPrompt(userID, promptType)
		if err == sql.ErrNoRows || existing == "" {
			// Get the default template
			promptLoader := ai.NewPromptLoader(e.store, e.config)
			template, err = promptLoader.GetPrompt(userID, ai.PromptType(promptType))
			if err != nil {
				return fmt.Errorf("get default prompt: %w", err)
			}
		} else if err != nil {
			return err
		} else {
			template = existing
		}
	}

	// If temperature not specified, preserve existing or use nil
	return e.store.SetUserPrompt(userID, promptType, template, temp)
}

// ResetPrompt reverts a prompt type to its embedded default.
func (e *Engine) ResetPrompt(userID int64, promptType string) error {
	if !allowedPromptTypes[promptType] {
		return fmt.Errorf("unknown or restricted prompt type: %q", promptType)
	}
	return e.store.DeleteUserPrompt(userID, promptType)
}

// DefaultPrompt returns the embedded default prompt template for a type.
func (e *Engine) DefaultPrompt(promptType string) (string, error) {
	if !allowedPromptTypes[promptType] {
		return "", fmt.Errorf("unknown or restricted prompt type: %q", promptType)
	}
	return ai.DefaultPrompt(ai.PromptType(promptType))
}

// StarArticle sets or clears the starred flag on an article.
func (e *Engine) StarArticle(userID, articleID int64, starred bool) error {
	return e.store.UpdateStarred(userID, articleID, starred)
}

// RegisterUser creates a new user by name and returns the ID.
func (e *Engine) RegisterUser(name string) (int64, error) {
	return e.store.CreateUser(name)
}

// ResolveUser looks up a user by name and returns the ID.
func (e *Engine) ResolveUser(name string) (int64, error) {
	u, err := e.store.GetUserByName(name)
	if err != nil {
		return 0, err
	}
	return u.ID, nil
}

// ListUsers returns all registered users.
func (e *Engine) ListUsers() ([]User, error) {
	users, err := e.store.ListUsers()
	if err != nil {
		return nil, err
	}
	result := make([]User, len(users))
	for i, u := range users {
		result[i] = userFromStorage(u)
	}
	return result, nil
}

// GetOrProvisionOIDCUser looks up a Herald user by their OIDC subject claim,
// creating one if this is their first login. Email is synced on each login.
func (e *Engine) GetOrProvisionOIDCUser(sub, name, email string) (*User, error) {
	u, err := e.store.GetUserByOIDCSub(sub)
	if err == nil {
		// Existing user — sync email if it changed.
		if email != "" {
			currentEmail := ""
			if u.Email != nil {
				currentEmail = *u.Email
			}
			if email != currentEmail {
				e.store.UpdateUserOIDCEmail(u.ID, email)
			}
		}
		result := userFromStorage(*u)
		return &result, nil
	}

	// First login — auto-provision.
	newU, err := e.store.CreateUserWithOIDC(name, email, sub)
	if err != nil {
		return nil, fmt.Errorf("provision OIDC user: %w", err)
	}
	result := userFromStorage(*newU)
	return &result, nil
}

// userFromStorage converts a storage.User to the public herald.User type.
func userFromStorage(u storage.User) User {
	email := ""
	if u.Email != nil {
		email = *u.Email
	}
	return User{ID: u.ID, Name: u.Name, Email: email, CreatedAt: u.CreatedAt}
}

// --- Filter rules ---

// AddFilterRule validates and stores a new filter rule. Returns the rule ID.
func (e *Engine) AddFilterRule(userID int64, rule FilterRule) (int64, error) {
	if !allowedFilterAxes[rule.Axis] {
		return 0, fmt.Errorf("invalid filter axis: %q (must be author, category, or tag)", rule.Axis)
	}
	if rule.Value == "" {
		return 0, fmt.Errorf("filter rule value cannot be empty")
	}
	sr := &storage.FilterRule{
		UserID: userID,
		FeedID: rule.FeedID,
		Axis:   rule.Axis,
		Value:  rule.Value,
		Score:  rule.Score,
	}
	return e.store.AddFilterRule(sr)
}

// GetFilterRules returns filter rules for a user, optionally scoped to a feed.
func (e *Engine) GetFilterRules(userID int64, feedID *int64) ([]FilterRule, error) {
	rules, err := e.store.GetFilterRules(userID, feedID)
	if err != nil {
		return nil, err
	}
	result := make([]FilterRule, len(rules))
	for i, r := range rules {
		result[i] = FilterRule{
			ID:        r.ID,
			UserID:    r.UserID,
			FeedID:    r.FeedID,
			Axis:      r.Axis,
			Value:     r.Value,
			Score:     r.Score,
			CreatedAt: r.CreatedAt,
		}
	}
	return result, nil
}

// UpdateFilterRule updates the score of an existing filter rule.
func (e *Engine) UpdateFilterRule(ruleID int64, score int) error {
	return e.store.UpdateFilterRuleScore(ruleID, score)
}

// DeleteFilterRule deletes a filter rule by ID.
func (e *Engine) DeleteFilterRule(ruleID int64) error {
	return e.store.DeleteFilterRule(ruleID)
}

// GetFeedMetadata returns discoverable authors and categories for a feed.
func (e *Engine) GetFeedMetadata(feedID int64) (*FeedMetadata, error) {
	authors, err := e.store.GetFeedAuthors(feedID)
	if err != nil {
		return nil, err
	}
	categories, err := e.store.GetFeedCategories(feedID)
	if err != nil {
		return nil, err
	}
	return &FeedMetadata{
		FeedID:     feedID,
		Authors:    authors,
		Categories: categories,
	}, nil
}

// resolveFilterThreshold returns the user's filter threshold as a pointer
// suitable for passing to Store query methods. Returns nil if the user has
// no filter rules (fast path) or threshold is 0 (disabled).
func (e *Engine) resolveFilterThreshold(userID int64) *int {
	prefs, err := e.GetPreferences(userID)
	if err != nil || prefs.FilterThreshold == 0 {
		return nil
	}
	has, err := e.store.HasFilterRules(userID)
	if err != nil || !has {
		return nil
	}
	return &prefs.FilterThreshold
}

// Close releases all resources held by the engine.
func (e *Engine) Close() error {
	return e.store.Close()
}

// --- internal type conversion helpers ---

func articleFromInternal(a storage.Article) Article {
	return Article{
		ID:            a.ID,
		FeedID:        a.FeedID,
		Title:         a.Title,
		URL:           a.URL,
		Content:       a.Content,
		Summary:       a.Summary,
		Author:        a.Author,
		PublishedDate: a.PublishedDate,
		FetchedDate:   a.FetchedDate,
	}
}

func articlesFromInternal(articles []storage.Article) []Article {
	out := make([]Article, len(articles))
	for i, a := range articles {
		out[i] = articleFromInternal(a)
	}
	return out
}

func feedFromInternal(f storage.Feed) Feed {
	return Feed{
		ID:          f.ID,
		URL:         f.URL,
		Title:       f.Title,
		Description: f.Description,
		LastFetched: f.LastFetched,
		LastError:   f.LastError,
		Enabled:     f.Enabled,
		CreatedAt:   f.CreatedAt,
	}
}

func feedsFromInternal(ff []storage.Feed) []Feed {
	out := make([]Feed, len(ff))
	for i, f := range ff {
		out[i] = feedFromInternal(f)
	}
	return out
}
