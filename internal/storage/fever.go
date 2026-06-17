package storage

// FeverItemRow is an article enriched with per-user read/starred state,
// as needed by the Fever API items endpoint.
type FeverItemRow struct {
	Article
	IsRead    bool
	IsStarred bool
}

// FeverLink is an article group represented as a Fever hot link.
type FeverLink struct {
	GroupID     int64
	FeedID      int64
	ItemID      int64  // primary article ID
	IsSaved     int    // 1 if primary article is starred
	Temperature int    // 0-100
	Title       string // primary article title
	URL         string // primary article URL
	ItemIDs     string // comma-separated article IDs in the group
}
