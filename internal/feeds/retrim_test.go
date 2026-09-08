package feeds

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

// addFetchedArticle stores an article and marks it as one the full-text pass
// has already been through, which is the population the repair pass reads.
func addFetchedArticle(t *testing.T, store storage.Store, feedID int64, guid, content string) int64 {
	t.Helper()
	pub := time.Now()
	id, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          guid,
		Title:         "Article " + guid,
		URL:           "https://example.com/" + guid,
		Content:       content,
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}
	if err := store.MarkArticleFullTextFetched(id, resultReplaced); err != nil {
		t.Fatalf("MarkArticleFullTextFetched: %v", err)
	}
	return id
}

func TestRetrimStoredExtractions_RewritesSidebarBodies(t *testing.T) {
	store := newFullTextTestStore(t)
	feedID, err := store.AddFeed("https://example.com/feed", "Test Feed", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}

	dirty := addFetchedArticle(t, store, feedID, "dirty", aceOfSpadesExtraction)
	clean := `<div id="readability-page-1" class="page"><div>` +
		`<p>The conflict continues as the president stated that the bombing will stop if new leadership emerges, a position his advisers spent the week softening.</p>` +
		`</div></div>`
	cleanID := addFetchedArticle(t, store, feedID, "clean", clean)

	report, err := RetrimStoredExtractions(context.Background(), store, false, 0)
	if err != nil {
		t.Fatalf("RetrimStoredExtractions: %v", err)
	}
	if report.Scanned != 2 {
		t.Errorf("scanned = %d, want 2", report.Scanned)
	}
	if report.Changed != 1 {
		t.Errorf("changed = %d, want 1", report.Changed)
	}

	got, err := store.GetArticle(dirty)
	if err != nil {
		t.Fatalf("GetArticle: %v", err)
	}
	if strings.Contains(got.Content, "aceofspadeshq") {
		t.Errorf("contact block survived the repair:\n%s", got.Content)
	}
	if !strings.Contains(got.Content, "Warner Bros/Discovery") {
		t.Errorf("article body lost in the repair:\n%s", got.Content)
	}

	untouched, err := store.GetArticle(cleanID)
	if err != nil {
		t.Fatalf("GetArticle: %v", err)
	}
	if untouched.Content != clean {
		t.Errorf("clean article was rewritten:\n%s", untouched.Content)
	}
}

// Running twice must not keep rewriting: the trim is idempotent, so the second
// pass has nothing to change. A repair that never converges cannot be run on a
// schedule or re-run after an interruption.
func TestRetrimStoredExtractions_Idempotent(t *testing.T) {
	store := newFullTextTestStore(t)
	feedID, err := store.AddFeed("https://example.com/feed", "Test Feed", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	addFetchedArticle(t, store, feedID, "dirty", aceOfSpadesExtraction)

	first, err := RetrimStoredExtractions(context.Background(), store, false, 0)
	if err != nil || first.Changed != 1 {
		t.Fatalf("first pass: changed = %d, err = %v", first.Changed, err)
	}
	second, err := RetrimStoredExtractions(context.Background(), store, false, 0)
	if err != nil || second.Changed != 0 {
		t.Fatalf("second pass: changed = %d, err = %v; want no further rewrites", second.Changed, err)
	}
}

// A dry run reports the same count but leaves the database alone.
func TestRetrimStoredExtractions_DryRun(t *testing.T) {
	store := newFullTextTestStore(t)
	feedID, err := store.AddFeed("https://example.com/feed", "Test Feed", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	id := addFetchedArticle(t, store, feedID, "dirty", aceOfSpadesExtraction)

	report, err := RetrimStoredExtractions(context.Background(), store, true, 0)
	if err != nil {
		t.Fatalf("RetrimStoredExtractions: %v", err)
	}
	if report.Changed != 1 {
		t.Errorf("changed = %d, want 1", report.Changed)
	}
	got, err := store.GetArticle(id)
	if err != nil {
		t.Fatalf("GetArticle: %v", err)
	}
	if !strings.Contains(got.Content, "aceofspadeshq") {
		t.Error("dry run rewrote the article")
	}
}

// Articles the full-text pass has not touched are outside the population: the
// repair must not read or rewrite feed-provided bodies.
func TestRetrimStoredExtractions_SkipsUnfetched(t *testing.T) {
	store := newFullTextTestStore(t)
	feedID, err := store.AddFeed("https://example.com/feed", "Test Feed", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	pub := time.Now()
	if _, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "unfetched",
		Title:         "Unfetched",
		URL:           "https://example.com/unfetched",
		Content:       aceOfSpadesExtraction,
		PublishedDate: &pub,
	}); err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	report, err := RetrimStoredExtractions(context.Background(), store, false, 0)
	if err != nil {
		t.Fatalf("RetrimStoredExtractions: %v", err)
	}
	if report.Scanned != 0 || report.Changed != 0 {
		t.Errorf("scanned = %d, changed = %d; want 0, 0", report.Scanned, report.Changed)
	}
}

// The whole point of the breakdown is telling "this is the handful of
// table-layout sites" from "this is touching everything", so the per-feed
// numbers have to be right and the untouched feeds have to stay out of the
// list entirely.
func TestRetrimStoredExtractions_PerFeedBreakdown(t *testing.T) {
	store := newFullTextTestStore(t)
	dirtyFeed, err := store.AddFeed("https://example.com/dirty", "Dirty Feed", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	cleanFeed, err := store.AddFeed("https://example.com/clean", "Clean Feed", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}

	addFetchedArticle(t, store, dirtyFeed, "d1", aceOfSpadesExtraction)
	addFetchedArticle(t, store, dirtyFeed, "d2", aceOfSpadesExtraction)
	addFetchedArticle(t, store, cleanFeed, "c1", `<div id="readability-page-1" class="page"><div>`+
		`<p>The conflict continues as the president stated that the bombing will stop if new leadership emerges, a position his advisers spent the week softening in public.</p>`+
		`</div></div>`)

	report, err := RetrimStoredExtractions(context.Background(), store, true, 3)
	if err != nil {
		t.Fatalf("RetrimStoredExtractions: %v", err)
	}
	if report.Scanned != 3 || report.Changed != 2 {
		t.Fatalf("scanned = %d, changed = %d; want 3, 2", report.Scanned, report.Changed)
	}
	if len(report.Feeds) != 1 {
		t.Fatalf("got %d feeds in the breakdown, want only the one that changed", len(report.Feeds))
	}

	stat := report.Feeds[0]
	if stat.FeedID != dirtyFeed {
		t.Errorf("breakdown names feed %d, want %d", stat.FeedID, dirtyFeed)
	}
	if stat.FeedTitle != "Dirty Feed" {
		t.Errorf("feed title = %q, want %q", stat.FeedTitle, "Dirty Feed")
	}
	if stat.Scanned != 2 || stat.Changed != 2 {
		t.Errorf("feed scanned = %d, changed = %d; want 2, 2", stat.Scanned, stat.Changed)
	}
	if stat.CharsRemoved <= 0 || stat.CharsRemoved != report.CharsRemoved {
		t.Errorf("feed chars removed = %d, report total = %d", stat.CharsRemoved, report.CharsRemoved)
	}

	// The samples are the operator's only view of what a run would delete, so
	// they have to carry the text that is actually going away.
	joined := strings.Join(stat.Samples, " | ")
	if !strings.Contains(joined, "aceofspadeshq") {
		t.Errorf("samples do not show the removed contact block: %s", joined)
	}
	if strings.Contains(joined, "Warner Bros/Discovery") {
		t.Errorf("samples show article text that is being kept, not removed: %s", joined)
	}
	if len(stat.Samples) > 3 {
		t.Errorf("kept %d samples, want at most the requested 3", len(stat.Samples))
	}
}

// Samples are off by default: the pass reports on content, and a plain dry run
// should not print article text.
func TestRetrimStoredExtractions_NoSamplesByDefault(t *testing.T) {
	store := newFullTextTestStore(t)
	feedID, err := store.AddFeed("https://example.com/feed", "Test Feed", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	addFetchedArticle(t, store, feedID, "dirty", aceOfSpadesExtraction)

	report, err := RetrimStoredExtractions(context.Background(), store, true, 0)
	if err != nil {
		t.Fatalf("RetrimStoredExtractions: %v", err)
	}
	if len(report.Feeds) != 1 {
		t.Fatalf("got %d feeds, want 1", len(report.Feeds))
	}
	if len(report.Feeds[0].Samples) != 0 {
		t.Errorf("samples returned without being asked for: %v", report.Feeds[0].Samples)
	}
}
