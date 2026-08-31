package feeds

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/matthewjhunter/herald/internal/storagetest"
)

func init() {
	fullTextFetchDelay = 0 // disable random delays in tests
}

// --- isTruncated tests ---

func TestIsTruncated_ShortContent(t *testing.T) {
	// Short content that ends mid-sentence (no terminal punctuation) is truncated.
	if !isTruncated("Short summary with no ending") {
		t.Error("expected short content without terminal punctuation to be truncated")
	}
}

func TestIsTruncated_ShortCompletePost(t *testing.T) {
	// Short posts that end with terminal punctuation are intentional, not truncated.
	cases := []string{
		"Dammit. 'Hey, I can fix that fast'",
		"Well, that's unfortunate.",
		"This is fine!",
		"Really?",
		`He said "goodbye."`,
	}
	for _, c := range cases {
		if isTruncated(c) {
			t.Errorf("expected short complete post %q not to be truncated", c)
		}
	}
}

func TestIsTruncated_EmptyContent(t *testing.T) {
	if !isTruncated("") {
		t.Error("expected empty content to be truncated")
	}
}

func TestIsTruncated_Ellipsis(t *testing.T) {
	long := repeatStr("word ", 120) // > 500 chars, but ends with ellipsis
	for _, suffix := range []string{"...", "…", "[…]"} {
		content := long + suffix
		if !isTruncated(content) {
			t.Errorf("expected content ending in %q to be truncated", suffix)
		}
	}
}

func TestIsTruncated_FullContent(t *testing.T) {
	// A real article body: long and does not end in ellipsis.
	content := repeatStr("This is a complete sentence with real content. ", 15)
	if isTruncated(content) {
		t.Error("expected full content not to be truncated")
	}
}

func TestIsTruncated_HTMLContent(t *testing.T) {
	// Short HTML content that ends mid-sentence (no terminal punctuation) is truncated.
	htmlShort := "<p><strong>Summary:</strong> Just a little bit of text that keeps going</p>"
	if !isTruncated(htmlShort) {
		t.Error("expected short HTML content without terminal punctuation to be truncated")
	}
}

func TestIsTruncated_HTMLFull(t *testing.T) {
	para := "<p>" + repeatStr("Full paragraph text here. ", 25) + "</p>"
	if isTruncated(para) {
		t.Error("expected full HTML article not to be truncated")
	}
}

// --- syndication-footer tests ---

// Excerpt-only WordPress feeds append a "The post X appeared first on Y."
// footer, which ends in a period and so used to be credited as an intentional
// short post -- leaving the reader with nothing but the footer and the AI
// stages summarizing a syndication notice (an editorial-cartoon post whose
// whole body is an image is the worst case). Content carrying that footer with
// little else is an excerpt, and must be fetched in full.
func TestIsTruncated_SyndicationFooterOnly(t *testing.T) {
	cases := map[string]string{
		"yoast the post": `<p>"Everywhere you go, Big Brother government is watching."</p>` +
			`<p>The post <a href="https://texasscorecard.com/opinion/youre-not-paranoid/">You're Not Paranoid ...</a>` +
			` appeared first on <a href="https://texasscorecard.com">Texas Scorecard</a>.</p>`,
		"the article":      `<p>A single line of setup.</p><p>The article <a href="https://example.com/a">Some Title</a> appeared first on <a href="https://example.com">Example</a>.</p>`,
		"first appeared":   `<p>A single line of setup.</p><p>This post first appeared on Example News.</p>`,
		"continue reading": `<p>A single line of setup.</p><p><a href="https://example.com/a">Continue reading &rarr;</a></p>`,
		"read more":        `<p>A single line of setup.</p><p><a href="https://example.com/a">Read more...</a></p>`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if !isTruncated(content) {
				t.Errorf("expected excerpt with syndication footer to be truncated: %q", content)
			}
		})
	}
}

// A full-content feed also carries the Yoast footer. It must not be dragged
// into a pointless full-text fetch just because the footer is present.
func TestIsTruncated_SyndicationFooterOnFullPost(t *testing.T) {
	content := "<p>" + repeatStr("Full paragraph text here. ", 25) + "</p>" +
		`<p>The post <a href="https://example.com/a">Some Title</a> appeared first on <a href="https://example.com">Example</a>.</p>`
	if isTruncated(content) {
		t.Error("expected full article with syndication footer not to be truncated")
	}
}

// Ordinary prose must not be mistaken for a footer: the stripper keys on the
// full "appeared first on" construction, not on the words alone.
func TestStripSyndicationFooter_LeavesProse(t *testing.T) {
	prose := "The post office appeared first on the left, then the courthouse."
	if got := stripSyndicationFooter(prose); got != prose {
		t.Errorf("stripSyndicationFooter rewrote ordinary prose:\n got %q\nwant %q", got, prose)
	}
}

// --- isLinkPost tests ---

func TestIsLinkPost_ExternalLink(t *testing.T) {
	// Instapundit-style: short content that IS an outbound link.
	content := `<a href="https://freebeacon.com/some-article/">Kennedy Scion Had No Earned Income</a>`
	if !isLinkPost(content, "https://instapundit.com/780696/") {
		t.Error("expected short content with external link to be a link post")
	}
}

func TestIsLinkPost_SameHost(t *testing.T) {
	// Link pointing back to the same host is not a link post.
	content := `<a href="https://example.com/other-article">Related article</a>`
	if isLinkPost(content, "https://example.com/post/123") {
		t.Error("same-host link should not be treated as a link post")
	}
}

func TestIsLinkPost_LongContent(t *testing.T) {
	// Long content is never a link post regardless of links.
	content := `<p>` + repeatStr("Long article body. ", 20) + `</p><a href="https://other.com/x">source</a>`
	if isLinkPost(content, "https://blog.com/post") {
		t.Error("long content should not be treated as a link post")
	}
}

func TestIsLinkPost_NoLinks(t *testing.T) {
	// Short content with no links is just truncated, not a link post.
	if isLinkPost("Short summary with no links.", "https://example.com/post") {
		t.Error("short content without links should not be a link post")
	}
}

func TestIsLinkPost_ImageURL(t *testing.T) {
	// A link to an image should not be treated as a link post.
	cases := []string{
		`<a href="https://cdn.example.com/photo.jpeg">image</a>`,
		`<a href="https://cdn.example.com/photo.jpg?w=800">image</a>`,
		`<a href="https://cdn.example.com/photo.png">image</a>`,
		`<a href="https://cdn.example.com/photo.webp">image</a>`,
		`<a href="https://cdn.example.com/photo.gif">image</a>`,
	}
	for _, c := range cases {
		if isLinkPost(c, "https://blog.example.com/post") {
			t.Errorf("image URL link should not be a link post: %s", c)
		}
	}
}

func TestImageURLRe(t *testing.T) {
	match := []string{
		"https://cdn.example.com/photo.jpeg",
		"https://cdn.example.com/photo.jpg",
		"https://cdn.example.com/photo.png",
		"https://cdn.example.com/photo.gif",
		"https://cdn.example.com/photo.webp",
		"https://cdn.example.com/photo.svg",
		"https://cdn.example.com/photo.avif",
		"https://cdn.example.com/photo.JPG",
		"https://cdn.example.com/photo.jpg?w=800&q=auto",
	}
	noMatch := []string{
		"https://example.com/article",
		"https://example.com/page.html",
		"https://example.com/image-gallery",
		"",
	}
	for _, u := range match {
		if !imageURLRe.MatchString(u) {
			t.Errorf("expected %q to match image URL pattern", u)
		}
	}
	for _, u := range noMatch {
		if imageURLRe.MatchString(u) {
			t.Errorf("expected %q NOT to match image URL pattern", u)
		}
	}
}

// --- textLength tests ---

func TestTextLength_PlainText(t *testing.T) {
	n := textLength("hello world")
	if n != 10 { // "helloworld" = 10 non-space
		t.Errorf("textLength = %d, want 10", n)
	}
}

func TestTextLength_HTMLTags(t *testing.T) {
	n := textLength("<p>hello</p>")
	if n != 5 { // "hello"
		t.Errorf("textLength of <p>hello</p> = %d, want 5", n)
	}
}

func TestTextLength_Empty(t *testing.T) {
	if n := textLength(""); n != 0 {
		t.Errorf("textLength('') = %d, want 0", n)
	}
}

// --- fetchReadableContent / FetchFullTextForArticles integration ---

var fullArticleHTML = `<!DOCTYPE html>
<html>
<head><title>Full Article</title></head>
<body>
  <header><nav><a href="/">Home</a></nav></header>
  <article>
    <h1>Full Article Title</h1>
    <p>` + repeatStr("This is a full paragraph with meaningful content. ", 20) + `</p>
    <p>` + repeatStr("Another paragraph extending the article body substantially. ", 15) + `</p>
  </article>
  <footer>Footer noise</footer>
</body>
</html>`

func TestFetchReadableContent_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, fullArticleHTML)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	content, err := fetchReadableContent(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("fetchReadableContent error: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("expected non-empty content")
	}
}

func TestFetchReadableContent_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := fetchReadableContent(context.Background(), client, srv.URL)
	if err == nil {
		t.Fatal("expected error for HTTP 403")
	}
}

func TestFetchReadableContent_RejectsNonHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("\xff\xd8\xff\xe0")) //nolint:errcheck
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := fetchReadableContent(context.Background(), client, srv.URL)
	if err == nil {
		t.Fatal("expected error for non-HTML content-type")
	}
	if !strings.Contains(err.Error(), "non-HTML") {
		t.Errorf("expected non-HTML error, got: %v", err)
	}
}

func TestFetchReadableContent_InvalidURL(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	_, err := fetchReadableContent(context.Background(), client, "://bad-url")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

// --- FetchFullTextForArticles integration ---

func TestFetchFullTextForArticles_UpdatesTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, fullArticleHTML)
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(srv.URL, "Test Feed", "")

	// Article with a truncated summary pointing at the test server.
	pub := time.Now()
	articleID, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "ft-test-1",
		Title:         "Truncated Article",
		URL:           srv.URL,
		Content:       "Short excerpt...",
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	n, err := fetcher.FetchFullTextForArticles(context.Background())
	if err != nil {
		t.Fatalf("FetchFullTextForArticles: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 article updated, got %d", n)
	}

	// Content should now be replaced with the full article text.
	updated, err := store.GetArticle(articleID)
	if err != nil {
		t.Fatalf("GetArticle: %v", err)
	}
	if len(updated.Content) <= len("Short excerpt...") {
		t.Errorf("expected longer content after full-text fetch, got %d chars", len(updated.Content))
	}
}

func TestFetchFullTextForArticles_SkipsFullContent(t *testing.T) {
	// Article already has full content — server should not be called.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(srv.URL, "Full Feed", "")

	pub := time.Now()
	fullContent := repeatStr("Complete article content here with many words. ", 15)
	_, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "ft-test-full",
		Title:         "Full Article",
		URL:           srv.URL,
		Content:       fullContent,
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	n, err := fetcher.FetchFullTextForArticles(context.Background())
	if err != nil {
		t.Fatalf("FetchFullTextForArticles: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 articles updated, got %d", n)
	}
	if callCount != 0 {
		t.Errorf("expected server not to be called for full content, got %d calls", callCount)
	}
}

func TestFetchFullTextForArticles_DoesNotRetry(t *testing.T) {
	// Server returns 403 — article should be marked fetched and not retried.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(srv.URL, "Blocked Feed", "")

	pub := time.Now()
	_, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "ft-test-retry",
		Title:         "Blocked Article",
		URL:           srv.URL,
		Content:       "Short...",
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	fetcher.FetchFullTextForArticles(context.Background()) //nolint:errcheck

	// Second call should process zero articles (already marked fetched).
	pending, err := store.GetArticlesNeedingFullText(10)
	if err != nil {
		t.Fatalf("GetArticlesNeedingFullText: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 articles pending after first pass, got %d", len(pending))
	}
}

func TestFetchFullTextForArticles_LinkPost(t *testing.T) {
	// Two servers: one for the "blog post" page, one for the "linked article".
	linkedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, fullArticleHTML)
	}))
	defer linkedSrv.Close()

	// The blog post server should NOT be called; fetchs go to linkedSrv.
	postSrvCalled := 0
	postSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		postSrvCalled++
		w.WriteHeader(http.StatusOK)
	}))
	defer postSrv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(postSrv.URL, "Link Blog", "")

	pub := time.Now()
	// RSS content is just a link to the external article.
	linkContent := `<a href="` + linkedSrv.URL + `/article">Headline text goes here</a>`
	articleID, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "ft-link-1",
		Title:         "Headline text goes here",
		URL:           postSrv.URL + "/post/1",
		Content:       linkContent,
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	n, err := fetcher.FetchFullTextForArticles(context.Background())
	if err != nil {
		t.Fatalf("FetchFullTextForArticles: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 article updated, got %d", n)
	}
	if postSrvCalled != 0 {
		t.Errorf("blog post server should not have been called, got %d calls", postSrvCalled)
	}

	updated, err := store.GetArticle(articleID)
	if err != nil {
		t.Fatalf("GetArticle: %v", err)
	}
	// Original post content should be preserved.
	if updated.Content != linkContent {
		t.Errorf("original content should be unchanged, got %q", updated.Content)
	}
	// Linked content should be populated from the external article.
	if len(updated.LinkedContent) == 0 {
		t.Error("expected linked_content to be populated")
	}
	if updated.LinkedURL != linkedSrv.URL+"/article" {
		t.Errorf("linked_url = %q, want %q", updated.LinkedURL, linkedSrv.URL+"/article")
	}
}

func TestFetchFullTextForArticles_RejectsSidebarContent(t *testing.T) {
	// Simulate a blog where readability extracts sidebar text (quotes, contact
	// info) instead of the actual short article body. The extracted content has
	// no phrase overlap with the feed content, so it should be rejected.
	sidebarHTML := `<!DOCTYPE html>
<html><head><title>Blog</title></head>
<body>
  <div id="sidebar">
    <h2>E-mail me</h2>
    <p>at elmtreeforge at att point net</p>
    <blockquote>Of all tyrannies, a tyranny exercised for the good of its victims
    may be the most oppressive. It may be better to live under robber barons than
    under omnipotent moral busybodies. The robber baron cruelty may sometimes sleep,
    his cupidity may at some point be satiated; but those who torment us for our own
    good will torment us without end, for they do so with the approval of their
    consciences. - C.S. Lewis</blockquote>
    <blockquote>So now I am asking more of you than I have before. Maybe all.
    Sure as I know anything, I know this - they will try again. No more running.
    I aim to misbehave. - Capt. Mal</blockquote>
  </div>
</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, sidebarHTML)
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(srv.URL, "Sidebar Blog", "")

	pub := time.Now()
	articleID, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "sidebar-test-1",
		Title:         "Yes, time for some clearing, including",
		URL:           srv.URL,
		Content:       `<p>this top one, which reminded me of a discussion.</p>`,
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	n, err := fetcher.FetchFullTextForArticles(context.Background())
	if err != nil {
		t.Fatalf("FetchFullTextForArticles: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 articles updated (sidebar rejected), got %d", n)
	}

	// Original feed content should be preserved.
	updated, err := store.GetArticle(articleID)
	if err != nil {
		t.Fatalf("GetArticle: %v", err)
	}
	if !strings.Contains(updated.Content, "reminded me of a discussion") {
		t.Error("expected original feed content to be preserved")
	}
}

func TestLooksLikeContactPage(t *testing.T) {
	// Ace of Spades-style contact sidebar with obfuscated emails.
	sidebar := `Support Contact
Ace: aceofspadeshq at gee mail.com
CBD: cbd at cutjibnewsletter.com
Buck: buck.throckmorton at protonmail.com
joe mannix: mannix2024 at proton.me`
	if !looksLikeContactPage(sidebar) {
		t.Error("expected contact sidebar to be detected as boilerplate")
	}

	// Real article prose with no email addresses.
	article := `<p>The conflict continues as President Trump stated bombing will stop
	if Iran finds new leadership. Iran's response has been defiant, with Mojtaba
	reportedly positioned as the next Supreme Leader.</p>`
	if looksLikeContactPage(article) {
		t.Error("expected article prose not to be flagged as boilerplate")
	}

	// Article with one email address — should not be flagged.
	oneEmail := `<p>Contact the author at author@example.com for corrections.` +
		` The rest of this is a long article body with real prose content.</p>`
	if looksLikeContactPage(oneEmail) {
		t.Error("expected single-email article not to be flagged as boilerplate")
	}
}

func TestFeedContentOverlaps_Match(t *testing.T) {
	// Readability output contains phrases from the feed content.
	feed := "Hispanic guy said oh yeah it is true and worth it"
	extracted := `<p>Hispanic guy said oh yeah it is true and worth it.
		Other guy: "But that ass!"</p>`
	if !feedContentOverlaps(feed, extracted) {
		t.Error("expected overlap to be detected when feed phrases appear in extracted text")
	}
}

func TestFeedContentOverlaps_NoMatch(t *testing.T) {
	// Readability extracted a sidebar (C.S. Lewis quotes) instead of the article body.
	feed := `<p>this top one, which reminded me of a discussion.
		Hispanic guy: "Oh yeah, it's true, and it's worth it."</p>`
	extracted := `<p>Of all tyrannies, a tyranny exercised for the good of its victims
		may be the most oppressive. It may be better to live under robber barons than
		under omnipotent moral busybodies. - C.S. Lewis</p>
		<p>E-mail me at elmtreeforge at att point net</p>`
	if feedContentOverlaps(feed, extracted) {
		t.Error("expected no overlap between article feed content and sidebar text")
	}
}

func TestFeedContentOverlaps_ShortFeedContent(t *testing.T) {
	// Feed content too short to form a 3-word phrase — allow replacement.
	if !feedContentOverlaps("Short excerpt", "completely different text here") {
		t.Error("expected short feed content to allow replacement (not enough to judge)")
	}
}

func TestFeedContentOverlaps_EmptyFeed(t *testing.T) {
	if !feedContentOverlaps("", "any extracted content here") {
		t.Error("expected empty feed content to allow replacement")
	}
}

func TestFeedContentOverlaps_HTMLTags(t *testing.T) {
	// HTML tags should be stripped before comparison.
	feed := `<p><strong>Breaking news:</strong> the president signed the bill today</p>`
	extracted := `<div>The president signed the bill today in a ceremony at the White House.</div>`
	if !feedContentOverlaps(feed, extracted) {
		t.Error("expected overlap after stripping HTML tags")
	}
}

func TestSkipFullTextRe(t *testing.T) {
	match := []string{
		"https://www.youtube.com/shorts/piM5i-4M2eo",
		"https://youtube.com/shorts/abc123",
		"https://youtu.be/dQw4w9WgXcQ",
	}
	noMatch := []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://www.youtube.com/channel/UC123",
		"https://arstechnica.com/article",
		"",
	}
	for _, u := range match {
		if !skipFullTextRe.MatchString(u) {
			t.Errorf("expected %q to be suppressed", u)
		}
	}
	for _, u := range noMatch {
		if skipFullTextRe.MatchString(u) {
			t.Errorf("expected %q NOT to be suppressed", u)
		}
	}
}

func TestFetchFullTextForArticles_SkipsSuppressedURL(t *testing.T) {
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(srv.URL, "YouTube Feed", "")

	pub := time.Now()
	_, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "yt-shorts-1",
		Title:         "Some Short",
		URL:           "https://www.youtube.com/shorts/piM5i-4M2eo",
		Content:       "short",
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	n, err := fetcher.FetchFullTextForArticles(context.Background())
	if err != nil {
		t.Fatalf("FetchFullTextForArticles: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 articles updated, got %d", n)
	}
	if called != 0 {
		t.Errorf("suppressed URL should not have been fetched, got %d calls", called)
	}
}

// helpers

func newFullTextTestStore(t *testing.T) storage.Store {
	t.Helper()
	store, cleanup := storagetest.NewStore(t)
	t.Cleanup(cleanup)
	return store
}

func repeatStr(s string, n int) string {
	var b string
	for range n {
		b += s
	}
	return b
}

// loremParagraphs returns n paragraphs of filler prose, enough to stand in for
// a real article body without checking a third party's copy into the repo.
func loremParagraphs(n int) string {
	const para = `<p>Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do
	eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim
	veniam, quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo
	consequat. Duis aute irure dolor in reprehenderit in voluptate velit esse cillum
	dolore eu fugiat nulla pariatur, excepteur sint occaecat cupidatat non proident.</p>
`
	return strings.Repeat(para, n)
}

// TestLooksLikeContactPage_ArticleWithContactHeader is the Ace of Spades shape:
// readability prepends the site's contact block to an otherwise complete
// article, so the extraction opens with a run of obfuscated addresses and then
// carries thousands of characters of prose. Counting addresses alone cannot
// tell that apart from a page that is nothing but a staff list, and rejecting
// it costs the article its whole body -- full_text_fetched is one-shot, so the
// feed's teaser is all it will ever have.
func TestLooksLikeContactPage_ArticleWithContactHeader(t *testing.T) {
	header := `Support Contact
Editor: editor at example dot com
Deputy: deputy at example.net
Tips: tips at exampletips.org
Legal: legal at example.net
Weekend: weekend at example.org
Overnight: overnight at example.net
`
	extraction := header + loremParagraphs(20)
	if textLength(extraction) < 3000 {
		t.Fatalf("fixture too small to represent a full article: %d chars", textLength(extraction))
	}
	if looksLikeContactPage(extraction) {
		t.Error("expected an article carrying a prepended contact header to be accepted")
	}
}

// TestLooksLikeContactPage_StaffDirectory holds the true positive: a page whose
// text is mostly addresses stays rejected no matter how many rows it has.
func TestLooksLikeContactPage_StaffDirectory(t *testing.T) {
	var b strings.Builder
	b.WriteString("<h1>Contact Us</h1><p>Reach the desk you need.</p>")
	for i := range 30 {
		fmt.Fprintf(&b, "<p>Desk %d: desk%d at example.org</p>\n", i, i)
	}
	if !looksLikeContactPage(b.String()) {
		t.Error("expected a staff directory to be rejected as boilerplate")
	}
}
