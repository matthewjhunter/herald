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

	// No self-host: nothing is filtered as same-site.
	got := extractExternalLinks("", body, summary)

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

// TestExtractExternalLinks_DropsSameHost covers the sidebar/archive pollution
// fix: a link-blog's own navigation points back into its own host on every page,
// so links whose host matches the article's own host are dropped, leaving only
// the editorial outbound citations. www. is stripped on both sides first.
func TestExtractExternalLinks_DropsSameHost(t *testing.T) {
	body := `<p>Today's post links
	         <a href="https://wire.example/2026/06/story">an external story</a>.
	         Sidebar: <a href="https://newsblog.example/archive/may">May archive</a>
	         <a href="https://www.newsblog.example/about">About</a></p>`

	got := extractExternalLinks("newsblog.example", body)

	want := []string{"wire.example/2026/06/story"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("got %v, want %v (same-host self-links should be dropped)", got, want)
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

// TestExtractLinksForArticles_DropsSelfLinks is the Postgres counterpart to the
// sidebar-pollution fix: an article's links back into its own host (the link
// blog's nav/archive widgets, which appear on every page) are never indexed, so
// the feed doesn't show up as linking to itself. The one genuine outbound link
// still is. Exercises the url column threaded through the extraction query.
func TestExtractLinksForArticles_DropsSelfLinks(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	feedID, err := store.AddFeed("https://aceofspades.example/feed", "Ace of Spades", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := store.SubscribeUserToFeed(1, feedID); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}
	if _, err := store.AddArticle(&storage.Article{
		FeedID: feedID, GUID: "ont", Title: "The ONT",
		URL: "https://aceofspades.example/2026/06/the-ont",
		Content: `<p>Read this <a href="https://wire.example/crocodile-story">elsewhere</a>.</p>
		          <aside>Recent: <a href="https://aceofspades.example/2026/06/morning-report">Morning Report</a>
		          <a href="https://www.aceofspades.example/archive">Archive</a></aside>`,
	}); err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	f := NewFetcher(store)
	if _, err := f.ExtractLinksForArticles(context.Background()); err != nil {
		t.Fatalf("ExtractLinksForArticles: %v", err)
	}

	// The genuine outbound link is indexed.
	if got, err := store.GetArticleBacklinks(1, 0, "wire.example/crocodile-story", 50); err != nil {
		t.Fatalf("GetArticleBacklinks(external): %v", err)
	} else if len(got) != 1 {
		t.Fatalf("expected 1 backlink to the external story, got %+v", got)
	}

	// The same-host sidebar self-links are not.
	for _, self := range []string{"aceofspades.example/2026/06/morning-report", "aceofspades.example/archive"} {
		if got, err := store.GetArticleBacklinks(1, 0, self, 50); err != nil {
			t.Fatalf("GetArticleBacklinks(%q): %v", self, err)
		} else if len(got) != 0 {
			t.Fatalf("self-link %q should not be indexed, got %+v", self, got)
		}
	}
}
