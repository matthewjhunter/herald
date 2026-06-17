package storage

import "time"

// FeedScoreStats holds AI scoring breakdown for a single feed (storage-internal type).
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

// ScoreStatsResult holds aggregate and per-feed AI scoring stats (storage-internal type).
type ScoreStatsResult struct {
	Feeds []FeedScoreStats
	Total FeedScoreStats
}

// ProcessingStats is an aggregate snapshot of where a user's articles sit in the
// AI pipeline (fetch -> security -> summarize -> curate). Unlike ScoreStatsResult
// it counts pipeline *state* (how much is done vs pending), not score outcomes,
// and is not broken down by feed. (storage-internal type)
type ProcessingStats struct {
	TotalArticles    int // articles in the user's subscribed feeds
	Scored           int // read_state.ai_scored = true
	Pending          int // unscored, ai_retries < maxRetries -- the real backlog
	Stuck            int // unscored, ai_retries >= maxRetries -- given up, needs attention
	SecurityPassed   int // security_score >= securityPassThreshold
	SecurityRejected int // security_score present and below the pass threshold
	SecuritySkipped  int // scored but no security score (no content / too short)
	Summarized       int // has a real (non-skipped) AI summary
	SummarizeSkipped int // summary row marked skipped (deterministic rejection)
	Curated          int // interest_score present
	FeedsTotal       int // feeds the user subscribes to
	FeedsErroring    int // feeds whose latest fetch failed (consecutive_errors > 0)
}

// CycleStats is one completed run of the fetch+process daemon cycle. Persisted so
// the web UI can show throughput and backend health without access to the
// daemon's in-memory state. (storage-internal type)
type CycleStats struct {
	ID                 int64
	CompletedAt        time.Time
	DurationMs         int64
	FeedsTotal         int
	FeedsDownloaded    int
	FeedsNotModified   int
	FeedsErrored       int
	NewArticles        int
	Processed          int
	HighInterest       int
	AIBackendAvailable bool
}

// Store defines the storage interface for herald's data layer.
type Store interface {
	Close() error

	// Users
	CreateUser(name string) (int64, error)
	GetUserByName(name string) (*User, error)
	GetUserByOIDCSub(sub string) (*User, error)
	CreateUserWithOIDC(name, email, sub string) (*User, error)
	UpdateUserOIDCEmail(id int64, email string) error
	ListUsers() ([]User, error)
	// DeleteUser removes a user and all rows they own, atomically.
	DeleteUser(userID int64) error

	// Sessions -- server-side OIDC session store. The browser holds only the
	// opaque session id; the access and refresh tokens stay here, sealed at rest.
	// The refresh token rotates on every use, so renewal is a CAS on the version
	// counter (RotateSessionTokens) guarded by an in-process lock in the web layer.
	CreateSession(s *Session) error
	GetSession(id string) (*Session, error)
	RotateSessionTokens(id string, accessToken, newRefreshToken []byte, accessExpiry time.Time, expectVersion int64) (bool, error)
	DeleteSession(id string) error
	DeleteExpiredSessions(now time.Time) (int64, error)

	// User prompts
	GetUserPrompt(userID int64, promptType string) (string, error)
	GetUserPromptTemperature(userID int64, promptType string) (float64, error)
	GetUserPromptModel(userID int64, promptType string) (string, error)
	SetUserPrompt(userID int64, promptType, promptTemplate string, temperature *float64, model *string) error
	DeleteUserPrompt(userID int64, promptType string) error
	ListUserPrompts(userID int64) ([]UserPrompt, error)

	// User preferences
	GetUserPreference(userID int64, key string) (string, error)
	SetUserPreference(userID int64, key, value string) error
	GetAllUserPreferences(userID int64) (map[string]string, error)
	DeleteUserPreference(userID int64, key string) error

	// Read state
	UpdateStarred(userID, articleID int64, starred bool) error
	UpdateReadState(userID, articleID int64, read bool, interestScore, securityScore *float64, securityReason *string, securityFlagged *bool) error
	// Security verdict lives on the article (#141): screened once, shared by all
	// subscribers. ScreenArticleSecurity/SkipArticleSecurity record the verdict;
	// GetUnscreenedArticles drives the global once-per-cycle security pass.
	ScreenArticleSecurity(articleID int64, securityScore float64, securityReason string, securityFlagged bool) error
	SkipArticleSecurity(articleID int64, reason string) error
	IncrementArticleSecurityAttempts(articleID int64) error
	GetUnscreenedArticles(limit int) ([]Article, error)
	SetInterestScore(userID, articleID int64, interestScore float64) error
	IncrementAIRetries(userID, articleID int64) error
	ResetScores(userID int64, securityOnly bool, belowScore float64) (int64, error)
	GetScoreStats(userID int64) (*ScoreStatsResult, error)
	GetProcessingStats(userID int64) (*ProcessingStats, error)
	RecordCycleStats(stats CycleStats) error
	GetRecentCycleStats(limit int) ([]CycleStats, error)

	// Feeds
	AddFeed(url, title, description string) (int64, error)
	GetAllFeeds() ([]Feed, error)
	GetFeed(feedID int64) (*Feed, error)
	UpdateFeedError(feedID int64, errMsg string) error
	ClearFeedError(feedID int64) error
	MarkFeedFetched(feedID int64) error
	UpdateFeedCacheHeaders(feedID int64, etag, lastModified string) error
	UpdateFeedLastFetched(feedID int64) error
	RenameFeed(feedID int64, title string) error
	RenameUserFeed(userID, feedID int64, title string) error
	UpdateFeedSiteURL(feedID int64, siteURL string) error

	// Articles
	AddArticle(article *Article) (int64, error)
	FindDuplicateArticle(title string, publishedDate *time.Time) (int64, error)
	GetUnreadArticles(limit int) ([]Article, error)
	GetArticle(articleID int64) (*Article, error)
	GetArticlesByInterestScore(userID int64, threshold float64, limit, offset int, filterThreshold *int) ([]Article, []float64, error)
	GetUnreadArticlesForUser(userID int64, limit, offset int, filterThreshold *int, includeRead bool) ([]Article, error)
	GetUnreadArticlesByFeed(userID, feedID int64, limit, offset int, filterThreshold *int, includeRead bool) ([]Article, error)
	GetUnscoredArticleCount(userID int64) (int, error)
	GetUnsummarizedArticleCount() (int, error)
	GetUnsummarizedScoredArticles(securityThreshold float64, limit int) ([]Article, error)
	GetUnscoredCurationArticles(userID int64, securityThreshold float64, limit int) ([]Article, error)
	GetUngroupedEmbeddedArticles(userID int64, model string, securityThreshold float64, since time.Time, limit int) ([]Article, error)
	GetArticlesNeedingFullText(limit int) ([]Article, error)
	UpdateArticleContent(articleID int64, content string) error
	UpdateArticleLinkedContent(articleID int64, linkedURL, linkedContent string) error
	MarkArticleFullTextFetched(articleID int64) error

	// Article images
	StoreArticleImage(articleID int64, originalURL string, data []byte, mimeType string, width, height int) (int64, error)
	GetArticleImage(imageID int64) (*ArticleImage, error)
	GetArticleImageMap(articleID int64) (map[string]int64, error)
	GetArticlesNeedingImageCache(limit int) ([]Article, error)
	MarkArticleImagesCached(articleID int64) error

	GetStarredArticles(userID int64, limit, offset int, filterThreshold *int) ([]Article, error)

	// UserSubscribedToArticleFeed reports whether the user is subscribed to the
	// feed that owns the article. Unknown article IDs return false, nil.
	UserSubscribedToArticleFeed(userID, articleID int64) (bool, error)

	// Article metadata
	StoreArticleAuthors(articleID int64, authors []ArticleAuthor) error
	StoreArticleCategories(articleID int64, categories []string) error
	GetArticleAuthors(articleID int64) ([]ArticleAuthor, error)
	GetArticleCategories(articleID int64) ([]string, error)

	// Feed metadata discovery
	GetFeedAuthors(feedID int64) ([]string, error)
	GetFeedCategories(feedID int64) ([]string, error)

	// Filter rules
	AddFilterRule(rule *FilterRule) (int64, error)
	GetFilterRules(userID int64, feedID *int64) ([]FilterRule, error)
	UpdateFilterRuleScore(userID, ruleID int64, score int) error
	DeleteFilterRule(userID, ruleID int64) error
	HasFilterRules(userID int64) (bool, error)

	// Article summaries (per-article, shared by all subscribers — #162)
	UpdateArticleAISummary(articleID int64, aiSummary string) error
	MarkSummarizationSkipped(articleID int64, reason string) error
	GetArticleSummary(articleID int64) (*ArticleSummary, error)

	// Feed stats
	GetFeedStats(userID int64) ([]FeedStats, error)

	// Article groups
	CreateArticleGroup(userID int64, topic string) (int64, error)
	AddArticleToGroup(groupID, articleID int64) error
	GetGroupArticles(groupID int64) ([]Article, error)
	GetArticleInterestScores(userID int64, articleIDs []int64) (map[int64]float64, error)
	GetArticleSecurityScores(articleIDs []int64) (map[int64]float64, error)
	UpdateGroupSummary(groupID int64, headline, summary string, articleCount int, maxInterestScore *float64) error
	GetGroupSummary(groupID int64) (*GroupSummary, error)
	GetUserGroups(userID int64) ([]ArticleGroup, error)
	GetGroup(groupID int64) (*ArticleGroup, error)
	FindArticleGroup(articleID, userID int64) (*int64, error)

	// Group virtual feed operations
	GetUnreadGroupArticles(userID, groupID int64, limit, offset int, filterThreshold *int, includeRead bool) ([]Article, error)
	GetGroupStats(userID int64) ([]GroupStats, error)
	SetGroupMuted(groupID int64, muted bool) error
	IsGroupMuted(groupID int64) (bool, error)
	DisbandGroup(groupID int64) error
	UpdateGroupDisplayName(groupID int64, displayName string) error

	// Search
	SearchArticlesFTS(userID int64, query string, limit, offset int) ([]Article, error)
	StoreArticleEmbedding(articleID int64, embedding []byte, model string) error
	MarkArticleEmbeddingSkipped(articleID int64, model string) error
	MarkArticleEmbeddingFailed(articleID int64, model, errMsg string) error
	GetArticleEmbeddings(userID int64, model string) ([]ArticleEmbeddingRow, error)
	GetArticleEmbeddingsByIDs(articleIDs []int64, model string) ([]ArticleEmbeddingRow, error)
	GetArticlesWithoutEmbeddings(model string, limit int) ([]Article, error)
	ResetAllArticleEmbeddings() (int64, error)
	ResetAllGroupEmbeddings() (int64, error)
	ResetStuckEmbeddings(model, errorPattern string) (int64, error)

	// Newsletters
	CreateNewsletter(n *Newsletter) (int64, error)
	UpdateNewsletter(n *Newsletter) error
	DeleteNewsletter(newsletterID int64) error
	GetNewsletter(newsletterID int64) (*Newsletter, error)
	GetUserNewsletters(userID int64) ([]Newsletter, error)
	GetDueNewsletters(schedule string) ([]Newsletter, error)

	// Newsletter issues
	CreateNewsletterIssue(issue *NewsletterIssue) (int64, error)
	GetNewsletterIssue(issueID int64) (*NewsletterIssue, error)
	GetLatestNewsletterIssue(newsletterID int64) (*NewsletterIssue, error)
	GetNewsletterIssues(newsletterID int64, limit, offset int) ([]NewsletterIssue, error)
	MarkNewsletterIssueSent(issueID int64) error
	UpdateNewsletterLastGenerated(newsletterID int64) error
	GetNewsletterStats(userID int64) ([]NewsletterStats, error)

	// Newsletter article selection
	GetNewsletterArticles(userID int64, config *NewsletterConfig, since *time.Time, limit int) ([]Article, []float64, error)

	// AI summaries
	CreateAISummary(s *AISummary) (int64, error)
	UpdateAISummaryDone(id int64, headline, contentHTML string, articleIDs []int64, inputTokens, outputTokens int) error
	UpdateAISummaryFailed(id int64, errMsg string) error
	GetLatestAISummary(userID int64) (*AISummary, error)
	GetInProgressAISummary(userID int64) (*AISummary, error)
	GetAISummary(userID, id int64) (*AISummary, error)
	GetAISummaries(userID int64, limit int) ([]AISummary, error)
	GetAISummariesForNewsletter(userID, newsletterID int64, limit int) ([]AISummary, error)
	GetUnreadArticlesForSummary(userID int64, minSecurity, minInterest float64, limit int) ([]Article, error)

	// Embedding-based group operations
	UpdateGroupEmbedding(groupID int64, embedding []byte, model string) error
	GetGroupsWithEmbeddings(userID int64, model string) ([]ArticleGroupWithEmbedding, error)
	GetGroupEmbedding(groupID int64) ([]byte, error)
	GetGroupArticleCount(groupID int64) (int, error)
	UpdateGroupTopic(groupID int64, topic string) error

	// Admin stats
	GetDBStats() (DBStats, error)

	// Fever API
	SetFeverCredential(userID int64, apiKey string) error
	GetUserByFeverAPIKey(apiKey string) (*User, error)
	GetFeverAPIKey(userID int64) (string, error)
	DeleteFeverCredential(userID int64) error
	GetFeverItems(userID int64, sinceID, maxID int64, withIDs []int64, limit int) ([]FeverItemRow, error)
	GetFeverItemCount(userID int64) (int, error)
	GetUnreadArticleIDsForUser(userID int64) ([]int64, error)
	GetStarredArticleIDsForUser(userID int64) ([]int64, error)
	MarkFeedArticlesRead(userID, feedID int64, before int64) error
	MarkGroupArticlesRead(userID, groupID int64, before int64) error
	MarkAllArticlesRead(userID int64, before int64) error
	GetFeedGroupMemberships(userID int64) (map[int64][]int64, error)
	GetFeverLinks(userID int64) ([]FeverLink, error)

	// Feed favicons
	StoreFeedFavicon(feedID int64, data []byte, mimeType string) error
	GetFeedFavicon(feedID int64) (*FeedFavicon, error)
	GetAllFeedFavicons() ([]FeedFavicon, error)
	GetSubscribedFeedsWithoutFavicons() ([]Feed, error)

	// Feed tags (per-user, many-to-many)
	AddFeedTag(userID, feedID int64, tag string) error
	RemoveFeedTag(userID, feedID int64, tag string) error
	GetFeedTags(userID, feedID int64) ([]string, error)
	GetAllFeedTags(userID int64) (map[int64][]string, error)
	GetUserTags(userID int64) ([]string, error)
	GetFeedsByTags(userID int64, tags []string) ([]int64, error)

	// Subscriptions
	SubscribeUserToFeed(userID, feedID int64) error
	GetUserFeeds(userID int64) ([]Feed, error)
	GetAllSubscribedFeeds() ([]Feed, error)
	GetAllActiveSubscribedFeeds() ([]Feed, error)
	GetFeedSubscribers(feedID int64) ([]int64, error)
	UnsubscribeUserFromFeed(userID, feedID int64) error
	DeleteFeedIfOrphaned(feedID int64) (bool, error)
	GetAllSubscribingUsers() ([]int64, error)
}
