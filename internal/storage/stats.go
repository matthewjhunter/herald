package storage

// FeedStat holds per-feed statistics for the admin stats page.
type FeedStat struct {
	ID          int64
	Title       string
	URL         string
	Status      string
	Articles    int
	Subscribers int
}

// DBStats aggregates database-level statistics.
type DBStats struct {
	TotalArticles int
	TotalFeeds    int
	TotalUsers    int
	Feeds         []FeedStat
}
