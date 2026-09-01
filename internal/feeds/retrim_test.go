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
	if err := store.MarkArticleFullTextFetched(id); err != nil {
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

	scanned, changed, err := RetrimStoredExtractions(context.Background(), store, false)
	if err != nil {
		t.Fatalf("RetrimStoredExtractions: %v", err)
	}
	if scanned != 2 {
		t.Errorf("scanned = %d, want 2", scanned)
	}
	if changed != 1 {
		t.Errorf("changed = %d, want 1", changed)
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

	if _, changed, err := RetrimStoredExtractions(context.Background(), store, false); err != nil || changed != 1 {
		t.Fatalf("first pass: changed = %d, err = %v", changed, err)
	}
	if _, changed, err := RetrimStoredExtractions(context.Background(), store, false); err != nil || changed != 0 {
		t.Fatalf("second pass: changed = %d, err = %v; want no further rewrites", changed, err)
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

	_, changed, err := RetrimStoredExtractions(context.Background(), store, true)
	if err != nil {
		t.Fatalf("RetrimStoredExtractions: %v", err)
	}
	if changed != 1 {
		t.Errorf("changed = %d, want 1", changed)
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

	scanned, changed, err := RetrimStoredExtractions(context.Background(), store, false)
	if err != nil {
		t.Fatalf("RetrimStoredExtractions: %v", err)
	}
	if scanned != 0 || changed != 0 {
		t.Errorf("scanned = %d, changed = %d; want 0, 0", scanned, changed)
	}
}
