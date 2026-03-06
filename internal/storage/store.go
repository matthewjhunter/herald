package storage

import "time"

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
	UpdateReadState(userID, articleID int64, read bool, interestScore, securityScore *float64) error

	// Feeds
	AddFeed(url, title, description string) (int64, error)
	GetAllFeeds() ([]Feed, error)
	UpdateFeedError(feedID int64, errMsg string) error
	ClearFeedError(feedID int64) error
	MarkFeedFetched(feedID int64) error
	UpdateFeedCacheHeaders(feedID int64, etag, lastModified string) error
	UpdateFeedLastFetched(feedID int64) error
	RenameFeed(feedID int64, title string) error

	// Articles
	AddArticle(article *Article) (int64, error)
	FindDuplicateArticle(title string, publishedDate *time.Time) (int64, error)
	GetUnreadArticles(limit int) ([]Article, error)
	GetArticle(articleID int64) (*Article, error)
	GetArticlesByInterestScore(userID int64, threshold float64, limit, offset int, filterThreshold *int) ([]Article, []float64, error)
	GetUnreadArticlesForUser(userID int64, limit, offset int, filterThreshold *int) ([]Article, error)
	GetUnreadArticlesByFeed(userID, feedID int64, limit, offset int, filterThreshold *int) ([]Article, error)
	GetUnscoredArticlesForUser(userID int64, limit int) ([]Article, error)
	GetUnscoredArticleCount(userID int64) (int, error)
	GetUnsummarizedArticleCount(userID int64) (int, error)
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
	UpdateFilterRuleScore(ruleID int64, score int) error
	DeleteFilterRule(ruleID int64) error
	HasFilterRules(userID int64) (bool, error)

	// Article summaries
	UpdateArticleAISummary(userID, articleID int64, aiSummary string) error
	GetArticleSummary(userID, articleID int64) (*ArticleSummary, error)

	// Feed stats
	GetFeedStats(userID int64) ([]FeedStats, error)

	// Article groups
	CreateArticleGroup(userID int64, topic string) (int64, error)
	AddArticleToGroup(groupID, articleID int64) error
	GetGroupArticles(groupID int64) ([]Article, error)
	UpdateGroupSummary(groupID int64, summary string, articleCount int, maxInterestScore *float64) error
	GetGroupSummary(groupID int64) (*GroupSummary, error)
	GetUserGroups(userID int64) ([]ArticleGroup, error)
	GetGroup(groupID int64) (*ArticleGroup, error)
	FindArticleGroup(articleID, userID int64) (*int64, error)

	// Embedding-based group operations
	UpdateGroupEmbedding(groupID int64, embedding []byte) error
	GetGroupsWithEmbeddings(userID int64) ([]ArticleGroupWithEmbedding, error)
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
