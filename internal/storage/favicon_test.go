package storage

import (
	"context"
	"testing"
)

func faviconEligible(t *testing.T, store Store, feedID int64) bool {
	t.Helper()
	feeds, err := store.GetSubscribedFeedsWithoutFavicons()
	if err != nil {
		t.Fatalf("GetSubscribedFeedsWithoutFavicons: %v", err)
	}
	for _, f := range feeds {
		if f.ID == feedID {
			return true
		}
	}
	return false
}

// TestFaviconFailureBackoff covers the negative cache (#112): a recorded failure
// removes a feed from the favicon retry set until its backoff window elapses,
// clearing the failure restores eligibility, and a cached favicon excludes the
// feed regardless of failure state.
func TestFaviconFailureBackoff(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
		if err := store.SubscribeUserToFeed(1, feedID); err != nil {
			t.Fatal(err)
		}

		// No favicon, no failure -> eligible for a fetch attempt.
		if !faviconEligible(t, store, feedID) {
			t.Fatal("feed without favicon or failure should be eligible")
		}

		// A recorded failure removes it from the retry set within the window.
		if err := store.RecordFaviconFailure(feedID, "permanent"); err != nil {
			t.Fatalf("RecordFaviconFailure: %v", err)
		}
		if faviconEligible(t, store, feedID) {
			t.Error("permanently-failed favicon must be skipped within its backoff window")
		}

		// A transient failure is likewise skipped within its (shorter) window.
		if err := store.RecordFaviconFailure(feedID, "transient"); err != nil {
			t.Fatalf("RecordFaviconFailure(transient): %v", err)
		}
		if faviconEligible(t, store, feedID) {
			t.Error("transient failure must be skipped within its backoff window")
		}

		// Backdate the failure past the transient window (6h): it becomes
		// eligible again. Backdate is test-only; the production path just waits.
		pg, ok := store.(*PostgresStore)
		if !ok {
			t.Fatalf("expected *PostgresStore, got %T", store)
		}
		if _, err := pg.pool.Exec(context.Background(),
			"UPDATE feeds SET favicon_failed_at = NOW() - INTERVAL '7 hours' WHERE id = $1", feedID); err != nil {
			t.Fatalf("backdate: %v", err)
		}
		if !faviconEligible(t, store, feedID) {
			t.Error("transient failure older than its window should be retried")
		}

		// Clearing the failure restores eligibility immediately.
		if err := store.RecordFaviconFailure(feedID, "permanent"); err != nil {
			t.Fatalf("RecordFaviconFailure: %v", err)
		}
		if err := store.ClearFaviconFailure(feedID); err != nil {
			t.Fatalf("ClearFaviconFailure: %v", err)
		}
		if !faviconEligible(t, store, feedID) {
			t.Error("cleared failure should make the feed eligible again")
		}

		// A cached favicon excludes the feed regardless of any failure state.
		if err := store.StoreFeedFavicon(feedID, []byte{0x89, 0x50, 0x4e, 0x47}, "image/png"); err != nil {
			t.Fatalf("StoreFeedFavicon: %v", err)
		}
		if faviconEligible(t, store, feedID) {
			t.Error("feed with a cached favicon must never be in the retry set")
		}
	})
}
