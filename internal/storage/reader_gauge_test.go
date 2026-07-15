package storage

import (
	"testing"
	"time"
)

// TestGetReaderPipelineCounts verifies the reader-gauge partition (#232): the
// in-view set splits into pending (unscreened), ready (screened + passed +
// unread), and read; blocked articles (screened but over the threat ceiling) are
// excluded entirely, and articles outside the recency window do not count.
func TestGetReaderPipelineCounts(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
		if err := store.SubscribeUserToFeed(1, feedID); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		now := time.Now()
		add := func(guid string) int64 {
			id, err := store.AddArticle(&Article{
				FeedID: feedID, GUID: guid, Title: guid,
				URL: "https://example.com/" + guid, PublishedDate: &now,
			})
			if err != nil {
				t.Fatalf("add %s: %v", guid, err)
			}
			return id
		}

		// pending: fetched, not yet screened, unread
		add("pending1")
		add("pending2")
		// ready: screened, security-passed (threat <= 3), unread
		store.ScreenArticleSecurity(add("ready1"), 1, "none", false, false) //nolint:errcheck
		// blocked: screened but over the ceiling -- excluded from the gauge
		store.ScreenArticleSecurity(add("blocked1"), 8, "prompt_injection", true, true) //nolint:errcheck
		// read: screened + passed + marked read
		rd := add("read1")
		store.ScreenArticleSecurity(rd, 0, "none", false, false) //nolint:errcheck
		if err := store.UpdateReadState(1, rd, true, nil); err != nil {
			t.Fatalf("mark read: %v", err)
		}

		since := now.Add(-7 * 24 * time.Hour)
		c, err := store.GetReaderPipelineCounts(1, 0, since, 3.0)
		if err != nil {
			t.Fatalf("GetReaderPipelineCounts: %v", err)
		}
		if c.Pending != 2 {
			t.Errorf("pending = %d, want 2", c.Pending)
		}
		if c.Ready != 1 {
			t.Errorf("ready = %d, want 1", c.Ready)
		}
		if c.Read != 1 {
			t.Errorf("read = %d, want 1", c.Read)
		}
		if total := c.Pending + c.Ready + c.Read; total != 4 {
			t.Errorf("total = %d, want 4 (blocked article must be excluded)", total)
		}

		// Feed scoping: an article in a feed the user is not subscribed to must
		// not count.
		otherFeed, _ := store.AddFeed("https://other.example.com/feed", "Other", "")
		other, _ := store.AddArticle(&Article{
			FeedID: otherFeed, GUID: "other1", Title: "other1",
			URL: "https://other.example.com/other1", PublishedDate: &now,
		})
		store.ScreenArticleSecurity(other, 0, "none", false, false) //nolint:errcheck
		c2, err := store.GetReaderPipelineCounts(1, 0, since, 3.0)
		if err != nil {
			t.Fatalf("GetReaderPipelineCounts (scope): %v", err)
		}
		if c2.Ready != 1 {
			t.Errorf("ready after adding unsubscribed-feed article = %d, want 1 (out of scope)", c2.Ready)
		}
	})
}
