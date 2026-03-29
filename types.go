package herald

import "time"

// EngineConfig configures the Herald content engine.
type EngineConfig struct {
	DBPath            string
	OllamaBaseURL     string
	SecurityModel     string
	CurationModel     string
	InterestThreshold float64
	SecurityThreshold float64
	Keywords          []string // user interest keywords for curation scoring
	UserID            int64    // primary user ID; DB preferences override CLI flags
	ReadOnly          bool     // when true, skip AI processor and fetcher creation
	MaxParallel       int      // max concurrent AI pipeline workers; 0 or 1 = serial
}

// User represents a registered household member.
type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Article represents a feed article.
type Article struct {
	ID            int64      `json:"id"`
	FeedID        int64      `json:"feed_id"`
	Title         string     `json:"title"`
	URL           string     `json:"url"`
	Content       string     `json:"content"`
	Summary       string     `json:"summary"`
	AISummary     string     `json:"ai_summary,omitempty"`
	Author        string     `json:"author"`
	PublishedDate *time.Time `json:"published_date,omitempty"`
	FetchedDate   time.Time  `json:"fetched_date"`
	LinkedURL     string     `json:"linked_url,omitempty"`
	LinkedContent string     `json:"linked_content,omitempty"`
}

// Feed represents an RSS/Atom feed subscription.
type Feed struct {
	ID          int64      `json:"id"`
	URL         string     `json:"url"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	SiteURL     string     `json:"site_url,omitempty"`
	LastFetched *time.Time `json:"last_fetched,omitempty"`
	LastError   *string    `json:"last_error,omitempty"`
	Enabled     bool       `json:"enabled"`
	CreatedAt   time.Time  `json:"created_at"`
}

// SearchResult holds a single search hit with match metadata.
type SearchResult struct {
	Article
	MatchType string  `json:"match_type"` // "fts", "semantic", or "both"
	Score     float64 `json:"score"`      // normalized relevance score (0-1)
}

// ScoredArticle is an article with its AI-generated scores.
type ScoredArticle struct {
	Article
	InterestScore float64 `json:"interest_score"`
	SecurityScore float64 `json:"security_score"`
	Safe          bool    `json:"safe"`
}

// ArticleGroup represents a cluster of articles covering the same topic/event.
type ArticleGroup struct {
	ID          int64     `json:"id"`
	UserID      int64     `json:"user_id"`
	Topic       string    `json:"topic"`
	DisplayName string    `json:"display_name,omitempty"`
	Muted       bool      `json:"muted"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Headline    string    `json:"headline,omitempty"`
	Summary     string    `json:"summary,omitempty"`
	Articles    []Article `json:"articles,omitempty"`
	Scores      []float64 `json:"scores,omitempty"`
	MaxScore    float64   `json:"max_score,omitempty"`
	Count       int       `json:"count"`
}

// GroupStats holds sidebar display data for an article group.
type GroupStats struct {
	GroupID        int64  `json:"group_id"`
	DisplayName    string `json:"display_name"`
	UnreadArticles int    `json:"unread_articles"`
}

// FeedStats holds article counts for a single feed.
type FeedStats struct {
	FeedID               int64      `json:"feed_id"`
	FeedTitle            string     `json:"feed_title"`
	TotalArticles        int        `json:"total_articles"`
	UnreadArticles       int        `json:"unread_articles"`
	UnsummarizedArticles int        `json:"unsummarized_articles"`
	LastPostDate         *time.Time `json:"last_post_date,omitempty"`
}

// FeedStatsResult contains per-feed stats and an aggregate total.
type FeedStatsResult struct {
	Feeds []FeedStats `json:"feeds"`
	Total FeedStats   `json:"total"`
}

// FeedScoreStats holds AI scoring breakdown for a single feed.
// Security buckets: Pass (>=7), Borderline (>=4,<7), Fail (<4).
// Interest buckets count only security-passed articles: High (>=8), Medium (>=5,<8), Low (<5).
type FeedScoreStats struct {
	FeedID        int64
	FeedTitle     string
	TotalScored   int
	SecPass       int
	SecBorderline int
	SecFail       int
	IntHigh       int
	IntMedium     int
	IntLow        int
}

func (f FeedScoreStats) SecTotal() int { return f.SecPass + f.SecBorderline + f.SecFail }
func (f FeedScoreStats) IntTotal() int { return f.IntHigh + f.IntMedium + f.IntLow }
func (f FeedScoreStats) SecPassPct() float64 {
	if f.SecTotal() == 0 {
		return 0
	}
	return float64(f.SecPass) / float64(f.SecTotal()) * 100
}
func (f FeedScoreStats) SecBorderlinePct() float64 {
	if f.SecTotal() == 0 {
		return 0
	}
	return float64(f.SecBorderline) / float64(f.SecTotal()) * 100
}
func (f FeedScoreStats) SecFailPct() float64 {
	if f.SecTotal() == 0 {
		return 0
	}
	return float64(f.SecFail) / float64(f.SecTotal()) * 100
}
func (f FeedScoreStats) IntHighPct() float64 {
	if f.IntTotal() == 0 {
		return 0
	}
	return float64(f.IntHigh) / float64(f.IntTotal()) * 100
}
func (f FeedScoreStats) IntMediumPct() float64 {
	if f.IntTotal() == 0 {
		return 0
	}
	return float64(f.IntMedium) / float64(f.IntTotal()) * 100
}
func (f FeedScoreStats) IntLowPct() float64 {
	if f.IntTotal() == 0 {
		return 0
	}
	return float64(f.IntLow) / float64(f.IntTotal()) * 100
}

// ScoreStatsResult holds aggregate and per-feed AI scoring stats.
type ScoreStatsResult struct {
	Feeds []FeedScoreStats
	Total FeedScoreStats
}

// UserPreferences holds all user-configurable preference values.
type UserPreferences struct {
	Keywords          []string `json:"keywords"`
	InterestThreshold float64  `json:"interest_threshold"`
	FilterThreshold   int      `json:"filter_threshold"`
	NotifyWhen        string   `json:"notify_when"` // "present", "always", "queue"
	NotifyMinScore    float64  `json:"notify_min_score"`
}

// FilterRule represents a user-defined scoring rule for article filtering.
type FilterRule struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	FeedID    *int64    `json:"feed_id,omitempty"`
	Axis      string    `json:"axis"`
	Value     string    `json:"value"`
	Score     int       `json:"score"`
	CreatedAt time.Time `json:"created_at"`
}

// DiscoveredFeed represents a feed found via autodiscovery on a web page.
type DiscoveredFeed struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	Type  string `json:"type"` // "rss", "atom", or "json"
}

// FeedMetadata holds discoverable metadata for a feed's articles.
type FeedMetadata struct {
	FeedID     int64    `json:"feed_id"`
	Authors    []string `json:"authors"`
	Categories []string `json:"categories"`
}

// PromptInfo summarizes a prompt type's current status.
type PromptInfo struct {
	Type        string  `json:"type"`
	Status      string  `json:"status"` // "custom" or "default"
	Temperature float64 `json:"temperature"`
}

// PromptDetail contains the full prompt template and metadata.
type PromptDetail struct {
	Type        string  `json:"type"`
	Template    string  `json:"template"`
	Temperature float64 `json:"temperature"`
	Model       string  `json:"model"`
	IsCustom    bool    `json:"is_custom"`
}

// FetchResult summarizes a feed polling cycle.
type FetchResult struct {
	FeedsTotal       int      `json:"feeds_total"`
	FeedsDownloaded  int      `json:"feeds_downloaded"`
	FeedsNotModified int      `json:"feeds_not_modified"`
	FeedsErrored     int      `json:"feeds_errored"`
	NewArticles      int      `json:"new_articles"`
	ProcessedCount   int      `json:"processed"`
	HighInterest     int      `json:"high_interest_count"`
	Errors           []string `json:"errors,omitempty"`
}
