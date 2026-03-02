package storage

import "fmt"

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

// GetDBStats returns article counts per feed and overall DB totals.
func (s *SQLiteStore) GetDBStats() (DBStats, error) {
	var stats DBStats

	rows, err := s.db.Query(`
		SELECT
			f.id, f.title, f.url, f.status,
			COUNT(DISTINCT a.id)      AS articles,
			COUNT(DISTINCT uf.user_id) AS subscribers
		FROM feeds f
		LEFT JOIN articles   a  ON a.feed_id  = f.id
		LEFT JOIN user_feeds uf ON uf.feed_id = f.id
		GROUP BY f.id
		ORDER BY articles DESC
	`)
	if err != nil {
		return stats, fmt.Errorf("failed to query feed stats: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var fs FeedStat
		if err := rows.Scan(&fs.ID, &fs.Title, &fs.URL, &fs.Status, &fs.Articles, &fs.Subscribers); err != nil {
			return stats, fmt.Errorf("failed to scan feed stat: %w", err)
		}
		stats.Feeds = append(stats.Feeds, fs)
		stats.TotalArticles += fs.Articles
		stats.TotalFeeds++
	}

	if err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&stats.TotalUsers); err != nil {
		return stats, fmt.Errorf("failed to count users: %w", err)
	}

	return stats, nil
}
