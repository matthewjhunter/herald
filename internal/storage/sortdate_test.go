package storage

import (
	"testing"
	"time"
)

// A feed whose <pubDate> carries no timezone is parsed as UTC, so its articles
// arrive stamped hours in the past. Ordering the reader's list on that stamp
// files a six-minute-old article four hours down the list, underneath entries
// the reader has already been through -- it never appears at the top, and the
// reader finds it days later or not at all.
//
// sort_date is the ordering key that keeps both dates in view: an article that
// is fresh when we fetch it sorts by when it reached us, however old the
// publisher claims it is. Genuinely old articles -- a backfill, a digest --
// still sort by publication, so subscribing to a feed does not dump its archive
// on top of today's news.
func TestSortDateOrdersByArrivalForFreshArticles(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		uid, err := store.CreateUser("reader")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		feedID, err := store.AddFeed("https://ace.example/index.xml", "Ace", "")
		if err != nil {
			t.Fatalf("AddFeed: %v", err)
		}
		if err := store.SubscribeUserToFeed(uid, feedID); err != nil {
			t.Fatalf("SubscribeUserToFeed: %v", err)
		}

		// One reference point, so every case is stated relative to "now".
		now := time.Now().UTC().Truncate(time.Second)
		at := func(d time.Duration) time.Time { return now.Add(d) }
		ptr := func(tm time.Time) *time.Time { return &tm }

		cases := []struct {
			title     string
			published *time.Time
			fetched   time.Time
		}{
			// The bug: stamped 4h stale by a missing timezone, fetched 6
			// minutes after it really went out. Belongs at the top.
			{"stale timezone", ptr(at(-4 * time.Hour)), at(-6 * time.Minute)},
			// An honest feed, published and fetched an hour ago.
			{"honest hour old", ptr(at(-time.Hour)), at(-58 * time.Minute)},
			// No date at all: arrival is all we know.
			{"undated", nil, at(-2 * time.Hour)},
			// Future-dated, which must not pin it above everything.
			{"future dated", ptr(at(3 * time.Hour)), at(-3 * time.Hour)},
			// A month-old post arriving in a backfill: genuinely old, and it
			// must not displace today's news.
			{"backfill", ptr(at(-30 * 24 * time.Hour)), at(-time.Minute)},
		}
		for _, tc := range cases {
			if _, err := store.AddArticle(&Article{
				FeedID: feedID, GUID: tc.title, Title: tc.title,
				URL:           "https://ace.example/" + tc.title,
				PublishedDate: tc.published,
				FetchedDate:   tc.fetched,
			}); err != nil {
				t.Fatalf("AddArticle %q: %v", tc.title, err)
			}
		}

		got, err := store.GetUnreadArticlesForUser(uid, 10, 0, nil, false)
		if err != nil {
			t.Fatalf("GetUnreadArticlesForUser: %v", err)
		}
		want := []string{
			"stale timezone",  // arrived 6m ago
			"honest hour old", // arrived 58m ago
			"undated",         // arrived 2h ago
			"future dated",    // arrived 3h ago, claim of the future ignored
			"backfill",        // published a month ago, sorts as a month old
		}
		if diff := titles(got); !equalStrings(diff, want) {
			t.Errorf("order:\n got %v\nwant %v", diff, want)
		}
	})
}

// Within one poll every article shares a fetch time, so the ordering key falls
// through to publication date. A feed lists newest first and we insert in that
// order, so ordering on the row id alone would stand the batch on its head.
func TestSortDateBreaksBatchTiesByPublication(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		uid, err := store.CreateUser("reader")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		feedID, err := store.AddFeed("https://batch.example/feed", "Batch", "")
		if err != nil {
			t.Fatalf("AddFeed: %v", err)
		}
		if err := store.SubscribeUserToFeed(uid, feedID); err != nil {
			t.Fatalf("SubscribeUserToFeed: %v", err)
		}

		// One fetch, three articles, newest first as the feed lists them.
		fetched := time.Now().UTC().Truncate(time.Second)
		for i, title := range []string{"newest", "middle", "oldest"} {
			published := fetched.Add(-time.Duration(i+1) * time.Hour)
			if _, err := store.AddArticle(&Article{
				FeedID: feedID, GUID: title, Title: title,
				URL:           "https://batch.example/" + title,
				PublishedDate: &published,
				FetchedDate:   fetched,
			}); err != nil {
				t.Fatalf("AddArticle %q: %v", title, err)
			}
		}

		got, err := store.GetUnreadArticlesForUser(uid, 10, 0, nil, false)
		if err != nil {
			t.Fatalf("GetUnreadArticlesForUser: %v", err)
		}
		want := []string{"newest", "middle", "oldest"}
		if diff := titles(got); !equalStrings(diff, want) {
			t.Errorf("batch order:\n got %v\nwant %v", diff, want)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
