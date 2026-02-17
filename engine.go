package herald

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/feeds"
	"github.com/matthewjhunter/herald/internal/storage"
)

// Engine is the public API for herald's content processing pipeline.
// It wraps the internal storage, feed fetcher, and AI processor.
type Engine struct {
	store   *storage.Store
	fetcher *feeds.Fetcher
	ai      *ai.AIProcessor
	config  *storage.Config
}

// NewEngine creates a herald content engine backed by the given SQLite database.
// The AI processor is created eagerly but only contacts Ollama when called.
func NewEngine(cfg EngineConfig) (*Engine, error) {
	if cfg.OllamaBaseURL == "" {
		cfg.OllamaBaseURL = "http://localhost:11434"
	}
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

	store, err := storage.NewStore(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	storeCfg := storage.DefaultConfig()
	storeCfg.Ollama.BaseURL = cfg.OllamaBaseURL
	storeCfg.Ollama.SecurityModel = cfg.SecurityModel
	storeCfg.Ollama.CurationModel = cfg.CurationModel
	storeCfg.Thresholds.InterestScore = cfg.InterestThreshold
	storeCfg.Thresholds.SecurityScore = cfg.SecurityThreshold
	storeCfg.Preferences.Keywords = cfg.Keywords

	fetcher := feeds.NewFetcher(store)

	processor, err := ai.NewAIProcessor(
		cfg.OllamaBaseURL, cfg.SecurityModel, cfg.CurationModel,
		store, storeCfg,
	)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("create AI processor: %w", err)
	}

	return &Engine{
		store:   store,
		fetcher: fetcher,
		ai:      processor,
		config:  storeCfg,
	}, nil
}

// FetchAllFeeds fetches all subscribed feeds and stores new articles.
func (e *Engine) FetchAllFeeds(ctx context.Context) (*FetchResult, error) {
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
			e.store.UpdateReadState(article.ID, false, &zero, &secScore)
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
		e.store.UpdateReadState(article.ID, false, &interestScore, &secScore)

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
	articles, err := e.store.GetUnreadArticlesForUser(userID, limit, offset)
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
	articles, scores, err := e.store.GetArticlesByInterestScore(threshold, limit, offset)
	if err != nil {
		return nil, nil, err
	}
	return articlesFromInternal(articles), scores, nil
}

// MarkArticleRead marks an article as read.
func (e *Engine) MarkArticleRead(articleID int64) error {
	return e.store.UpdateReadState(articleID, true, nil, nil)
}

// ImportOPML imports feeds from an OPML file and subscribes the user.
func (e *Engine) ImportOPML(path string, userID int64) error {
	return e.fetcher.ImportOPML(path, userID)
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

	e.store.ClearFeedError(feedID)

	return e.store.SubscribeUserToFeed(userID, feedID)
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
	// Get group metadata
	groups, err := e.store.GetUserGroups(0) // search all users
	if err != nil {
		return nil, fmt.Errorf("get groups: %w", err)
	}
	var group *storage.ArticleGroup
	for _, g := range groups {
		if g.ID == groupID {
			group = &g
			break
		}
	}

	// Fall back to querying articles directly if group metadata lookup failed
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
	articles, scores, err := e.store.GetArticlesByInterestScore(
		e.config.Thresholds.InterestScore, 20, 0)
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

		briefing.WriteString(fmt.Sprintf("## %s (%.1f/10)\n", article.Title, score))
		briefing.WriteString(fmt.Sprintf("%s\n", article.URL))

		if summary, err := e.store.GetArticleSummary(userID, article.ID); err == nil && summary != nil {
			briefing.WriteString(fmt.Sprintf("%s\n", summary.AISummary))
		} else if article.Summary != "" {
			briefing.WriteString(fmt.Sprintf("%s\n", article.Summary))
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
		}
		result.Total.TotalArticles += fs.TotalArticles
		result.Total.UnreadArticles += fs.UnreadArticles
		result.Total.UnsummarizedArticles += fs.UnsummarizedArticles
	}
	return result, nil
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
