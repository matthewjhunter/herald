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
	Keywords []string // user interest keywords for curation scoring
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
