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
}

// User represents a registered household member.
type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
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
}

// Feed represents an RSS/Atom feed subscription.
type Feed struct {
	ID          int64      `json:"id"`
	URL         string     `json:"url"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	LastFetched *time.Time `json:"last_fetched,omitempty"`
	LastError   *string    `json:"last_error,omitempty"`
	Enabled     bool       `json:"enabled"`
	CreatedAt   time.Time  `json:"created_at"`
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
	ID        int64      `json:"id"`
	UserID    int64      `json:"user_id"`
	Topic     string     `json:"topic"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Summary   string     `json:"summary,omitempty"`
	Articles  []Article  `json:"articles,omitempty"`
	Scores    []float64  `json:"scores,omitempty"`
	MaxScore  float64    `json:"max_score,omitempty"`
	Count     int        `json:"count"`
}

// FeedStats holds article counts for a single feed.
type FeedStats struct {
	FeedID               int64  `json:"feed_id"`
	FeedTitle            string `json:"feed_title"`
	TotalArticles        int    `json:"total_articles"`
	UnreadArticles       int    `json:"unread_articles"`
	UnsummarizedArticles int    `json:"unsummarized_articles"`
}

// FeedStatsResult contains per-feed stats and an aggregate total.
type FeedStatsResult struct {
	Feeds []FeedStats `json:"feeds"`
	Total FeedStats   `json:"total"`
}

// UserPreferences holds all user-configurable preference values.
type UserPreferences struct {
	Keywords          []string `json:"keywords"`
	InterestThreshold float64  `json:"interest_threshold"`
	FilterThreshold   int      `json:"filter_threshold"`
	NotifyWhen        string   `json:"notify_when"`      // "present", "always", "queue"
	NotifyMinScore    float64  `json:"notify_min_score"`
}

// FilterRule represents a user-defined scoring rule for article filtering.
type FilterRule struct {
	ID        int64      `json:"id"`
	UserID    int64      `json:"user_id"`
	FeedID    *int64     `json:"feed_id,omitempty"`
	Axis      string     `json:"axis"`
	Value     string     `json:"value"`
	Score     int        `json:"score"`
	CreatedAt time.Time  `json:"created_at"`
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
	Status      string  `json:"status"`      // "custom" or "default"
	Temperature float64 `json:"temperature"`
}

// PromptDetail contains the full prompt template and metadata.
type PromptDetail struct {
	Type        string  `json:"type"`
	Template    string  `json:"template"`
	Temperature float64 `json:"temperature"`
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
