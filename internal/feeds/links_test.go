package feeds

import (
	"context"
	"testing"

	"github.com/matthewjhunter/herald/internal/storage"
)

func TestExtractExternalLinks(t *testing.T) {
	body := `<p>See <a href="https://example.com/a">a</a> and
	         <a href="https://www.example.com/a/">dup of a</a>.
	         <a href="/relative">rel</a> <a href="mailto:x@y.com">mail</a>
	         <a href="https://other.example/b?utm=1#frag">b</a></p>`
	summary := `<a href="https://example.com/a">a again</a>
	            <a href="https://summary-only.example/c">c</a>`

	got := extractExternalLinks(body, summary)

	// Normalized, deduped (a/www.a/a-again collapse), relative+mailto dropped.
	want := map[string]bool{
		"example.com/a":          true,
		"other.example/b":        true,
		"summary-only.example/c": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d links %v, want %d %v", len(got), got, len(want), want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected link %q", g)
		}
	}
}

// TestExtractLinksForArticles_EndToEnd runs the stage against Postgres: an
// article whose body links a target URL becomes discoverable as a backlink.
func TestExtractLinksForArticles_EndToEnd(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	const target = "https://hollymathnerd.substack.com/p/the-government-sets-the-trap"
	feedID, err := store.AddFeed("https://instapundit.example/feed", "Instapundit", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := store.SubscribeUserToFeed(1, feedID); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}
	if _, err := store.AddArticle(&storage.Article{
		FeedID: feedID, GUID: "post", Title: "Heh",
		URL:     "https://instapundit.example/p/1",
		Content: `<p>Worth a read: <a href="` + target + `?r=wm1qp&triedRedirect=true">link</a></p>`,
	}); err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	f := NewFetcher(store)
	if _, err := f.ExtractLinksForArticles(context.Background()); err != nil {
		t.Fatalf("ExtractLinksForArticles: %v", err)
	}

	// The href carried tracking params; the lookup uses the clean normalized URL.
	got, err := store.GetArticleBacklinks(1, 0, "hollymathnerd.substack.com/p/the-government-sets-the-trap", 50)
	if err != nil {
		t.Fatalf("GetArticleBacklinks: %v", err)
	}
	if len(got) != 1 || got[0].FeedTitle != "Instapundit" {
		t.Fatalf("expected 1 backlink from Instapundit, got %+v", got)
	}
}
