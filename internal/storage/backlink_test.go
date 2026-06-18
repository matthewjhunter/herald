package storage

import (
	"testing"
	"time"
)

// TestGetArticleBacklinks covers the "which feeds linked to this article?"
// query: link-blog posts whose linked_url points at the target, matched after
// normalizing away scheme/www/query/fragment/trailing-slash differences so the
// session tracking params Substack appends (?r=...&triedRedirect=true) don't
// cause misses. The target article and unrelated posts are excluded.
func TestGetArticleBacklinks(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		now := time.Now()
		const target = "https://hollymathnerd.substack.com/p/the-government-sets-the-trap"

		// The source article (what others link to), in its own feed.
		srcFeed, _ := store.AddFeed("https://hollymathnerd.substack.com/feed", "Holly Math Nerd", "")
		if err := store.SubscribeUserToFeed(1, srcFeed); err != nil {
			t.Fatal(err)
		}
		srcID, _ := store.AddArticle(&Article{FeedID: srcFeed, GUID: "src", Title: "The Government Sets the Trap",
			URL: target, PublishedDate: &now})

		// Three link-blog posts pointing at the source via different URL forms.
		linkFeed, _ := store.AddFeed("https://instapundit.com/feed", "Instapundit", "")
		if err := store.SubscribeUserToFeed(1, linkFeed); err != nil {
			t.Fatal(err)
		}
		mk := func(guid, linkedURL string) int64 {
			id, err := store.AddArticle(&Article{FeedID: linkFeed, GUID: guid, Title: guid,
				URL: "https://instapundit.com/" + guid, PublishedDate: &now})
			if err != nil {
				t.Fatalf("AddArticle %s: %v", guid, err)
			}
			// linked_url is set post-insert (the full-text stage does this in prod).
			if err := store.UpdateArticleLinkedContent(id, linkedURL, ""); err != nil {
				t.Fatalf("UpdateArticleLinkedContent %s: %v", guid, err)
			}
			return id
		}
		clean := mk("clean", target)
		tracked := mk("tracked", target+"?r=wm1qp&triedRedirect=true")
		variant := mk("variant", "http://www.hollymathnerd.substack.com/p/the-government-sets-the-trap/")
		mk("unrelated", "https://example.com/something-else")

		got, err := store.GetArticleBacklinks(1, srcID, target, 50)
		if err != nil {
			t.Fatalf("GetArticleBacklinks: %v", err)
		}

		ids := map[int64]bool{}
		for _, b := range got {
			ids[b.ArticleID] = true
			if b.FeedTitle != "Instapundit" {
				t.Errorf("backlink %d: feed title = %q, want Instapundit", b.ArticleID, b.FeedTitle)
			}
		}
		for _, want := range []int64{clean, tracked, variant} {
			if !ids[want] {
				t.Errorf("expected backlink for article %d, missing (normalization?)", want)
			}
		}
		if ids[srcID] {
			t.Error("target article must be excluded from its own backlinks")
		}
		if len(got) != 3 {
			t.Errorf("got %d backlinks, want 3 (clean/tracked/variant; unrelated excluded)", len(got))
		}

		// The user may paste the URL with the tracking params (as it appears in
		// the address bar); the target side must normalize too.
		got2, err := store.GetArticleBacklinks(1, srcID, target+"?r=abc&triedRedirect=true", 50)
		if err != nil {
			t.Fatalf("GetArticleBacklinks (tracked target): %v", err)
		}
		if len(got2) != 3 {
			t.Errorf("tracked-target query got %d, want 3", len(got2))
		}
	})
}
