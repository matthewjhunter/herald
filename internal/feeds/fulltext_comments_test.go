package feeds

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

func mustParseHTML(t *testing.T, s string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return doc
}

func renderNode(t *testing.T, n *html.Node) string {
	t.Helper()
	var buf bytes.Buffer
	if err := html.Render(&buf, n); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// shortPostWithCommentsHTML is a WordPress post whose body is one sentence and
// two images, followed by a long wpDiscuz comment thread -- the shape of a
// Last Refuge post that readability extracted as nothing but comments. The
// body is far under readability's 500-character threshold, so its retry pass
// turns off the unlikely-candidate filter and the thread wins on text alone.
func shortPostWithCommentsHTML() string {
	var comments strings.Builder
	for i := range 40 {
		fmt.Fprintf(&comments, `
      <div id="wpd-comm-%d_0" class="wpd-comment wpd_comment_level-1">
        <div class="wpd-comment-wrap">
          <div class="wpd-avatar"><img alt="Reader %d" src="https://secure.gravatar.com/avatar/%d?s=64" height="64" width="64"/></div>
          <div id="comment-%d" class="wpd-comment-right">
            <div class="wpd-comment-header"><div class="wpd-comment-author">Reader %d</div><div class="wpd-comment-date">October 26, 2026 4:%02d pm</div></div>
            <div class="wpd-comment-text"><p>Commenter number %d has a great deal to say about this, at considerable length, with several clauses and an opinion or two about the reward.</p></div>
            <div class="wpd-comment-footer"><div class="wpd-vote-result">%d</div><span>Reply</span></div>
          </div>
        </div>
      </div>`, i, i, i, i, i, i%60, i, i)
	}
	return `<!DOCTYPE html>
<html><head><title>Interesting - Reward for Identifying Voter Fraud</title></head>
<body>
  <div class="site">
    <article class="post-single">
      <div>
        <h1>Interesting - Reward for Identifying Voter Fraud</h1>
        <div class="post-meta"><time>October 26, 2026</time> | <a href="/author/">Sundance</a> | <span class="comments-link"><a href="#comments">93 Comments</a></span></div>
      </div>
      <div class="post-content">
        <p>Not sure how this works, but it's likely details will follow:</p>
        <p><a href="https://example.com/uploads/truth-post.jpg"><img class="aligncenter size-full" src="https://example.com/uploads/truth-post.jpg" alt="" width="749" height="400"/></a></p>
        <p style="text-align: center;">[<a href="https://truthsocial.example/post/1">SOURCE</a>]</p>
        <p><a href="https://example.com/uploads/waving.png"><img class="aligncenter" src="https://example.com/uploads/waving-1024x596.png" alt="" width="749" height="436"/></a></p>
      </div>
      <div class="cat-links">Posted in <a href="/c/1">Donald Trump</a>, <a href="/c/2">Voter Fraud</a></div>
      <div class="post-comments">
        <div id="comments" class="comments-area">
          <div id="wpdcom" class="wpdiscuz_unauth wpd-minimal">
            <div class="wpd-thread-list">` + comments.String() + `
            </div>
          </div>
        </div>
      </div>
    </article>
  </div>
</body></html>`
}

// shortPostFeedContent is what the feed carried for the post: a teaser with a
// lazy-load placeholder thumbnail and the Yoast footer, which repeats the title.
const shortPostFeedContent = `<p><img width="150" height="150" src="data:image/gif;base64,R0lGODlhAQABAAAAACH5BAEKAAEALAAAAAABAAEAAAICTAEAOw==" data-src="https://example.com/uploads/truth-post-150x150.jpg" />Not sure how this works, but it's likely details will follow: [SOURCE] Posted in Donald Trump, Voter Fraud</p>
<p>The post <a rel="nofollow" href="https://example.com/post/">Interesting - Reward for Identifying Voter Fraud</a> appeared first on <a rel="nofollow" href="https://example.com">The Last Refuge</a>.</p>`

func TestFetchReadableContent_ExcludesCommentThread(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, shortPostWithCommentsHTML())
	}))
	defer srv.Close()

	got, err := fetchReadableContent(context.Background(), &http.Client{Timeout: 5 * time.Second}, srv.URL)
	if err != nil {
		t.Fatalf("fetchReadableContent: %v", err)
	}
	if !strings.Contains(got, "Not sure how this works") {
		t.Errorf("article body missing from extraction:\n%s", got)
	}
	for _, img := range []string{"truth-post.jpg", "waving-1024x596.png"} {
		if !strings.Contains(got, img) {
			t.Errorf("article image %s missing from extraction", img)
		}
	}
	for _, leak := range []string{"Commenter number", "gravatar", "Reader 1"} {
		if strings.Contains(got, leak) {
			t.Errorf("comment thread leaked into extraction (found %q)", leak)
		}
	}
}

func TestStripCommentSections(t *testing.T) {
	tests := []struct {
		name string
		html string
		keep []string
		drop []string
	}{
		{
			name: "wordpress comments area",
			html: `<article><p>Body text.</p><div id="comments" class="comments-area"><ol class="comment-list"><li>Nice post</li></ol></div></article>`,
			keep: []string{"Body text."},
			drop: []string{"Nice post"},
		},
		{
			name: "classic commentlist without wrapper",
			html: `<div class="entry"><p>Body text.</p></div><ol class="commentlist"><li>First!</li></ol>`,
			keep: []string{"Body text."},
			drop: []string{"First!"},
		},
		{
			name: "wpdiscuz root",
			html: `<div class="post"><p>Body text.</p></div><div id="wpdcom"><div class="wpd-thread-list">Hot take</div></div>`,
			keep: []string{"Body text."},
			drop: []string{"Hot take"},
		},
		{
			name: "disqus and reply form",
			html: `<p>Body text.</p><div id="disqus_thread">Disqus says</div><div id="respond" class="comment-respond"><form>Leave a reply</form></div>`,
			keep: []string{"Body text."},
			drop: []string{"Disqus says", "Leave a reply"},
		},
		{
			name: "class merely containing the word is kept",
			html: `<div class="post-content no-comments-yet"><p>Body text.</p></div><p class="commentary">An essay of commentary.</p>`,
			keep: []string{"Body text.", "An essay of commentary."},
		},
		{
			name: "body and article are never removed",
			html: `<article id="comments"><p>Body text.</p></article>`,
			keep: []string{"Body text."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := mustParseHTML(t, tt.html)
			stripCommentSections(doc)
			got := renderNode(t, doc)
			for _, k := range tt.keep {
				if !strings.Contains(got, k) {
					t.Errorf("expected %q to survive, got:\n%s", k, got)
				}
			}
			for _, d := range tt.drop {
				if strings.Contains(got, d) {
					t.Errorf("expected %q to be stripped, got:\n%s", d, got)
				}
			}
		})
	}
}

// TestFeedContentOverlaps_IgnoresSyndicationFooter covers the false match that
// let a comment thread through: the Yoast footer repeats the post title, and a
// comment quoting the title then "overlapped" the feed.
func TestFeedContentOverlaps_IgnoresSyndicationFooter(t *testing.T) {
	extracted := `<div><p>Reader 7</p><p>Great news about identifying voter fraud, finally.</p></div>`
	if feedContentOverlaps(shortPostFeedContent, extracted) {
		t.Error("a match on the footer's copy of the title should not count as overlap")
	}
	body := `<div><p>Not sure how this works, but it's likely details will follow:</p></div>`
	if !feedContentOverlaps(shortPostFeedContent, body) {
		t.Error("a match on the teaser text should still count as overlap")
	}
}
