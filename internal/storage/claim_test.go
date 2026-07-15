package storage

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// seedUnscreened adds n unscreened articles to a fresh subscribed feed and
// returns their IDs.
func seedUnscreened(t *testing.T, store Store, n int) []int64 {
	t.Helper()
	feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
	if err := store.SubscribeUserToFeed(1, feedID); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	now := time.Now()
	ids := make([]int64, n)
	for i := range ids {
		id, err := store.AddArticle(&Article{
			FeedID: feedID, GUID: string(rune('a' + i)), Title: "t",
			URL: "https://example.com/" + string(rune('a'+i)), PublishedDate: &now,
		})
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		ids[i] = id
	}
	return ids
}

func idSet(arts []Article) map[int64]bool {
	m := make(map[int64]bool, len(arts))
	for _, a := range arts {
		m[a.ID] = true
	}
	return m
}

// TestClaimUnscreenedArticles_HoldsAndReclaims verifies the claim keeps a row off
// other workers' queues until the lease expires, and that an expired lease makes
// it claimable again -- bounded by the attempts cap (#233).
func TestClaimUnscreenedArticles_HoldsAndReclaims(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ids := seedUnscreened(t, store, 3)

		// A live-lease claim holds its rows: successive claims return the rest,
		// never a row already held.
		first, err := store.ClaimUnscreenedArticles(2, 600)
		if err != nil {
			t.Fatalf("claim 1: %v", err)
		}
		if len(first) != 2 {
			t.Fatalf("claim 1 returned %d, want 2", len(first))
		}
		second, err := store.ClaimUnscreenedArticles(2, 600)
		if err != nil {
			t.Fatalf("claim 2: %v", err)
		}
		if len(second) != 1 {
			t.Fatalf("claim 2 returned %d, want 1 (two still held)", len(second))
		}
		for id := range idSet(second) {
			if idSet(first)[id] {
				t.Errorf("article %d claimed twice under a live lease", id)
			}
		}
		if got := len(first) + len(second); got != len(ids) {
			t.Fatalf("claimed %d distinct, want %d", got, len(ids))
		}
		// Nothing left to claim while all leases are live.
		if again, _ := store.ClaimUnscreenedArticles(10, 600); len(again) != 0 {
			t.Fatalf("claim under live leases returned %d, want 0", len(again))
		}

		// A zero-second lease treats every claim as expired -> reclaimable, but
		// each reclaim spends an attempt, so after the cap (3) the row is gone.
		// The rows already have 1 attempt from above; two more reclaims exhaust it.
		for round := 0; round < 2; round++ {
			got, err := store.ClaimUnscreenedArticles(10, 0)
			if err != nil {
				t.Fatalf("reclaim round %d: %v", round, err)
			}
			if len(got) != len(ids) {
				t.Fatalf("reclaim round %d returned %d, want %d", round, len(got), len(ids))
			}
		}
		if got, _ := store.ClaimUnscreenedArticles(10, 0); len(got) != 0 {
			t.Fatalf("claim after attempts exhausted returned %d, want 0", len(got))
		}
	})
}

// TestRefundSecurityClaim_RestoresAttempt verifies a refund returns the budget so
// a backend outage does not permanently retire an article.
func TestRefundSecurityClaim_RestoresAttempt(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		id := seedUnscreened(t, store, 1)[0]
		// Exhaust the budget: three reclaims with an expired lease.
		for i := 0; i < 3; i++ {
			store.ClaimUnscreenedArticles(10, 0) //nolint:errcheck
		}
		if got, _ := store.ClaimUnscreenedArticles(10, 0); len(got) != 0 {
			t.Fatalf("precondition: expected exhausted, got %d claimable", len(got))
		}
		if err := store.RefundSecurityClaim(id); err != nil {
			t.Fatalf("refund: %v", err)
		}
		got, err := store.ClaimUnscreenedArticles(10, 0)
		if err != nil {
			t.Fatalf("claim after refund: %v", err)
		}
		if len(got) != 1 || got[0].ID != id {
			t.Fatalf("after refund expected article %d claimable, got %v", id, idSet(got))
		}
	})
}

// TestReleaseSecurityClaim_ImmediateRetry verifies releasing a claim makes the
// row claimable again without waiting for the lease to expire.
func TestReleaseSecurityClaim_ImmediateRetry(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		id := seedUnscreened(t, store, 1)[0]
		if got, _ := store.ClaimUnscreenedArticles(1, 600); len(got) != 1 {
			t.Fatalf("initial claim returned %d, want 1", len(got))
		}
		// Held under a live lease.
		if got, _ := store.ClaimUnscreenedArticles(1, 600); len(got) != 0 {
			t.Fatalf("expected held under live lease, got %d", len(got))
		}
		if err := store.ReleaseSecurityClaim(id); err != nil {
			t.Fatalf("release: %v", err)
		}
		if got, _ := store.ClaimUnscreenedArticles(1, 600); len(got) != 1 {
			t.Fatalf("expected reclaimable after release, got %d", len(got))
		}
	})
}

// TestScreenArticleSecurity_LeavesClaimQueue verifies a recorded verdict removes
// the row from the claim queue (and clears its claim).
func TestScreenArticleSecurity_LeavesClaimQueue(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		id := seedUnscreened(t, store, 1)[0]
		if got, _ := store.ClaimUnscreenedArticles(1, 600); len(got) != 1 {
			t.Fatalf("claim returned %d, want 1", len(got))
		}
		if err := store.ScreenArticleSecurity(id, 1, "none", false, false); err != nil {
			t.Fatalf("screen: %v", err)
		}
		if got, _ := store.ClaimUnscreenedArticles(10, 0); len(got) != 0 {
			t.Fatalf("screened article still claimable: %d", len(got))
		}
	})
}

// TestClaimUnscreenedArticles_ConcurrentDisjoint verifies FOR UPDATE SKIP LOCKED:
// two workers claiming at once never grab the same article.
func TestClaimUnscreenedArticles_ConcurrentDisjoint(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ids := seedUnscreened(t, store, 20)

		var wg sync.WaitGroup
		results := make([][]Article, 4)
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				got, err := store.ClaimUnscreenedArticles(10, 600)
				if err != nil {
					t.Errorf("worker %d claim: %v", w, err)
					return
				}
				results[w] = got
			}(w)
		}
		wg.Wait()

		seen := map[int64]int{}
		total := 0
		for _, r := range results {
			total += len(r)
			for _, a := range r {
				seen[a.ID]++
			}
		}
		for id, c := range seen {
			if c > 1 {
				t.Errorf("article %d claimed by %d workers concurrently", id, c)
			}
		}
		if total != len(ids) {
			// All 20 within budget should be claimed exactly once across workers.
			claimed := make([]int64, 0, len(seen))
			for id := range seen {
				claimed = append(claimed, id)
			}
			sort.Slice(claimed, func(i, j int) bool { return claimed[i] < claimed[j] })
			t.Errorf("claimed %d total across workers, want %d (distinct=%d)", total, len(ids), len(seen))
		}
	})
}
