package storage

import (
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/urlnorm"
)

// TestGetArticleBacklinks covers the "which feeds linked to this?" query over
// the article_links index: articles whose stored outbound links include the
// normalized target are returned (with their feed), the target article itself
// is excluded, and unrelated articles are not returned.
func TestGetArticleBacklinks(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		now := time.Now()
		const target = "https://hollymathnerd.substack.com/p/the-government-sets-the-trap"
		norm := urlnorm.Normalize(target)

		srcFeed, _ := store.AddFeed("https://hollymathnerd.substack.com/feed", "Holly Math Nerd", "")
		if err := store.SubscribeUserToFeed(1, srcFeed); err != nil {
			t.Fatal(err)
		}
		srcID, _ := store.AddArticle(&Article{FeedID: srcFeed, GUID: "src", Title: "The Trap",
			URL: target, PublishedDate: &now})

		linkFeed, _ := store.AddFeed("https://instapundit.com/feed", "Instapundit", "")
		if err := store.SubscribeUserToFeed(1, linkFeed); err != nil {
			t.Fatal(err)
		}
		mk := func(guid string, links ...string) int64 {
			id, err := store.AddArticle(&Article{FeedID: linkFeed, GUID: guid, Title: guid,
				URL: "https://instapundit.com/" + guid, PublishedDate: &now})
			if err != nil {
				t.Fatalf("AddArticle %s: %v", guid, err)
			}
			if err := store.StoreArticleLinks(id, links); err != nil {
				t.Fatalf("StoreArticleLinks %s: %v", guid, err)
			}
			return id
		}
		// Two posts that link the target (among other links), one that doesn't.
		a := mk("a", urlnorm.Normalize("https://example.com/x"), norm)
		b := mk("b", norm)
		mk("c", urlnorm.Normalize("https://example.com/y"))

		// The source article also references itself; must be excluded by id.
		if err := store.StoreArticleLinks(srcID, []string{norm}); err != nil {
			t.Fatal(err)
		}

		got, err := store.GetArticleBacklinks(1, srcID, norm, 50)
		if err != nil {
			t.Fatalf("GetArticleBacklinks: %v", err)
		}
		ids := map[int64]bool{}
		for _, bl := range got {
			ids[bl.ArticleID] = true
			if bl.FeedTitle != "Instapundit" {
				t.Errorf("backlink %d feed = %q, want Instapundit", bl.ArticleID, bl.FeedTitle)
			}
		}
		if !ids[a] || !ids[b] {
			t.Errorf("expected backlinks from a(%d) and b(%d); got %v", a, b, ids)
		}
		if ids[srcID] {
			t.Error("target article must be excluded from its own backlinks")
		}
		if len(got) != 2 {
			t.Errorf("got %d backlinks, want 2 (a, b; c unrelated, src excluded)", len(got))
		}

		// Bare-domain prefix: "all links to substack.com" matches both substack
		// links (a, b) regardless of path; the example.com linker (c) does not.
		dom, err := store.GetArticleBacklinks(1, srcID, "hollymathnerd.substack.com", 50)
		if err != nil {
			t.Fatalf("GetArticleBacklinks (domain): %v", err)
		}
		if len(dom) != 2 {
			t.Errorf("domain prefix got %d, want 2 (a, b)", len(dom))
		}
	})
}

// TestGetArticleBacklinks_CaseInsensitivePrefix proves matching is
// case-insensitive: a mixed-case link is found by a lower-cased prefix (the
// form urlnorm.QueryKey always produces).
func TestGetArticleBacklinks_CaseInsensitivePrefix(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		now := time.Now()
		feedID, _ := store.AddFeed("https://blog.example/feed", "Blog", "")
		if err := store.SubscribeUserToFeed(1, feedID); err != nil {
			t.Fatal(err)
		}
		id, _ := store.AddArticle(&Article{FeedID: feedID, GUID: "p", Title: "p",
			URL: "https://blog.example/p", PublishedDate: &now})
		// Stored via Normalize, which lower-cases an upper-case source path.
		if err := store.StoreArticleLinks(id, []string{urlnorm.Normalize("https://Example.com/Foo/Bar")}); err != nil {
			t.Fatal(err)
		}
		got, err := store.GetArticleBacklinks(1, 0, "example.com/foo", 50)
		if err != nil {
			t.Fatalf("GetArticleBacklinks: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("case-insensitive prefix got %d, want 1", len(got))
		}
	})
}
