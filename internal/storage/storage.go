package storage

import (
	"fmt"
	"strings"
	"time"
)

type Feed struct {
	ID                int64
	URL               string
	Title             string
	Description       string
	SiteURL           string // blog homepage URL, populated from feed metadata
	LastFetched       *time.Time
	LastError         *string
	ETag              string
	LastModified      string
	Enabled           bool
	CreatedAt         time.Time
	ConsecutiveErrors int
	NextFetchAt       *time.Time
	Status            string // "active" or "dead"
}

type Article struct {
	ID              int64
	FeedID          int64
	GUID            string
	Title           string
	URL             string
	Content         string
	Summary         string
	Author          string
	PublishedDate   *time.Time
	FetchedDate     time.Time
	LinkedURL       string // outbound link extracted from a link-blog post
	LinkedContent   string // readability content fetched from LinkedURL
	SecurityFlagged bool   // true when article passed the medium security threshold but not the full threshold
	Read            bool   // requesting user's read state; only set by queries that select read_state.read
	Starred         bool   // requesting user's starred state; only set by queries that select read_state.starred
}

type ArticleSummary struct {
	ArticleID   int64
	AISummary   string
	GeneratedAt time.Time
}

type ReadState struct {
	ArticleID     int64
	Read          bool
	Starred       bool
	InterestScore *float64
	SecurityScore *float64
	ReadDate      *time.Time
}

type ArticleGroup struct {
	ID          int64
	UserID      int64
	Topic       string
	DisplayName string
	Muted       bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ArticleEmbeddingRow holds a single article's embedding vector.
type ArticleEmbeddingRow struct {
	ArticleID int64
	Embedding []float32
}

// GroupMatch is one row of MatchArticlesToGroups: an embedded cohort article and
// the user's nearest existing group centroid within the join threshold. GroupID
// is 0 when the article has a usable embedding but no group is near enough -- a
// leftover that the FORM phase may cluster into a new group.
type GroupMatch struct {
	ArticleID int64
	GroupID   int64
}

// SemanticHit is one result of SemanticSearch: an article whose embedding is
// within the distance threshold of the query vector, with its cosine distance
// (0 = identical, 2 = opposite). The caller turns distance into a similarity
// score (1 - distance) for ranking.
type SemanticHit struct {
	ArticleID int64
	Distance  float64
}

// GroupStats holds sidebar display data for an article group virtual feed.
type GroupStats struct {
	GroupID        int64
	DisplayName    string
	UnreadArticles int
}

type GroupSummary struct {
	GroupID          int64
	Headline         string
	Summary          string
	ArticleCount     int
	MaxInterestScore *float64
	GeneratedAt      time.Time
}

// NewsletterConfig holds the filtering criteria for a newsletter definition.
type NewsletterConfig struct {
	MinInterestScore  float64  `json:"min_interest_score"`
	MinSecurityScore  float64  `json:"min_security_score"`
	IncludeFeeds      []int64  `json:"include_feeds,omitempty"`
	ExcludeFeeds      []int64  `json:"exclude_feeds,omitempty"`
	IncludeCategories []string `json:"include_categories,omitempty"`
	ExcludeCategories []string `json:"exclude_categories,omitempty"`
	// IncludeTags names feed tags this digest follows: at generation time they
	// resolve to whatever feeds currently carry the tag, unioned with IncludeFeeds.
	IncludeTags []string `json:"include_tags,omitempty"`
	MaxArticles int      `json:"max_articles"`
}

// Newsletter represents a user-defined newsletter/digest configuration.
type Newsletter struct {
	ID              int64
	UserID          int64
	Name            string
	Schedule        string // "manual", "hourly", "daily"
	Config          NewsletterConfig
	PromptTemplate  string
	EmailRecipient  string
	Enabled         bool
	LastGeneratedAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NewsletterIssue represents a single generated newsletter edition.
type NewsletterIssue struct {
	ID           int64
	NewsletterID int64
	Headline     string
	ContentHTML  string
	ContentText  string
	ArticleIDs   []int64
	GeneratedAt  time.Time
	SentAt       *time.Time
}

// NewsletterStats holds sidebar display data for a newsletter.
type NewsletterStats struct {
	NewsletterID int64
	Name         string
	IssueCount   int
}

// AISummary is a single on-demand digest of a user's high-interest unread
// backlog, generated in one long-context cloud-model call. Status moves
// generating → done | failed. ArticleIDs records exactly which articles the
// digest covered, so "mark all as read" can mark precisely those.
type AISummary struct {
	ID           int64
	UserID       int64
	NewsletterID *int64 // config that produced it; nil = ad-hoc
	Status       string // "generating", "done", "failed"
	Model        string
	Prompt       string
	Headline     string
	ContentHTML  string
	ArticleIDs   []int64
	ArticleCount int
	InputTokens  int
	OutputTokens int
	Error        string
	CreatedAt    time.Time
	GeneratedAt  *time.Time
}

type UserPrompt struct {
	UserID         int64
	PromptType     string
	PromptTemplate string
	Temperature    *float64
	Model          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ArticleAuthor represents an author extracted from a feed item.
type ArticleAuthor struct {
	Name  string
	Email string
}

// FilterRule represents a user-defined scoring rule for article filtering.
type FilterRule struct {
	ID        int64
	UserID    int64
	FeedID    *int64 // nil = global rule
	Axis      string // "author", "category", "tag"
	Value     string
	Score     int
	CreatedAt time.Time
}

// applyErrorBackoff returns base doubled for each consecutive error, capped at 30 days.
func applyErrorBackoff(base time.Duration, consecutiveErrors int) time.Duration {
	if consecutiveErrors <= 0 {
		return base
	}
	n := min(consecutiveErrors,
		// cap multiplier at 64×
		6)
	backoff := base * time.Duration(1<<n)
	if max := 30 * 24 * time.Hour; backoff > max {
		return max
	}
	return backoff
}

// User represents a registered household member.
type User struct {
	ID        int64
	Name      string
	OIDCSub   *string // OIDC subject claim; nil for users created before OIDC
	Email     *string // email from JWT; may be nil
	CreatedAt time.Time
}

// FeedStats holds per-feed article counts.
type FeedStats struct {
	FeedID               int64
	FeedTitle            string
	TotalArticles        int
	UnreadArticles       int
	UnsummarizedArticles int
	LastPostDate         *time.Time
}

// maxTagLen caps a feed tag's length (runes) to keep the UI and storage sane.
const maxTagLen = 50

// normalizeTag trims surrounding space, collapses internal whitespace runs to a
// single space, and caps the length. Returns "" for an effectively empty tag,
// which callers reject. Case is preserved for display; uniqueness is enforced
// case-insensitively by the schema (COLLATE NOCASE / lower(tag) index).
func normalizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return ""
	}
	tag = strings.Join(strings.Fields(tag), " ")
	if len([]rune(tag)) > maxTagLen {
		tag = string([]rune(tag)[:maxTagLen])
	}
	return tag
}

// normalizeTags normalizes a slice of tags, dropping empties and case-insensitive
// duplicates while preserving order.
func normalizeTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		n := normalizeTag(t)
		if n == "" {
			continue
		}
		k := strings.ToLower(n)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
	}
	return out
}

// --- Feed favicons ---

// FeedFavicon holds a cached favicon for a feed.
type FeedFavicon struct {
	FeedID    int64
	Data      []byte
	MimeType  string
	FetchedAt time.Time
}

// --- Article images ---

// ArticleImage holds a cached image extracted from article content.
type ArticleImage struct {
	ID          int64
	ArticleID   int64
	OriginalURL string
	Data        []byte
	MimeType    string
	Width       int
	Height      int
	FetchedAt   time.Time
}

// NewStore returns a Store backed by PostgreSQL. herald is Postgres-only; the
// DSN must be a "postgres://" or "postgresql://" URL. A bare file path (the old
// SQLite form) is rejected with a clear error so a stale config fails loudly
// instead of silently doing nothing.
func NewStore(dsn string) (Store, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return NewPostgresStore(dsn)
	}
	return nil, fmt.Errorf("storage: DSN %q is not a postgres:// URL; herald is Postgres-only (SQLite support was removed)", dsn)
}
