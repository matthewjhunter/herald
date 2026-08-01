package web

import (
	"crypto/md5"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/matthewjhunter/herald/internal/storage"
)

// feedbackKinds returns the kinds recorded for the test user, newest first.
func feedbackKinds(t *testing.T, tf *testFixtures) []string {
	t.Helper()
	events, err := tf.store.ListFeedbackEvents(tf.userID, 50)
	if err != nil {
		t.Fatalf("ListFeedbackEvents: %v", err)
	}
	kinds := make([]string, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	return kinds
}

func onlyEvent(t *testing.T, tf *testFixtures) storage.FeedbackEventRow {
	t.Helper()
	events, err := tf.store.ListFeedbackEvents(tf.userID, 50)
	if err != nil {
		t.Fatalf("ListFeedbackEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events (%v), want exactly 1", len(events), feedbackKinds(t, tf))
	}
	return events[0]
}

// TestFeedbackDistinguishesOpenFromBulkDismiss is the test this whole subsystem
// exists for. Opening an article and clearing the queue both end in
// read_state.read = TRUE, and they mean opposite things. If these two ever
// record the same kind, every downstream model learns that the reader loves the
// articles they ignored.
func TestFeedbackDistinguishesOpenFromBulkDismiss(t *testing.T) {
	t.Run("opening an article records engagement", func(t *testing.T) {
		tf := newTestFixtures(t)

		rr := authedRequest(t, tf, "GET", fmt.Sprintf("/articles/%d?from=web-list&pos=3", tf.articleID), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("article view: status %d", rr.Code)
		}

		ev := onlyEvent(t, tf)
		if ev.Kind != string(storage.FeedbackArticleOpened) {
			t.Errorf("kind = %q, want %q", ev.Kind, storage.FeedbackArticleOpened)
		}
		if ev.Surface != string(storage.SurfaceWebList) {
			t.Errorf("surface = %q, want %q", ev.Surface, storage.SurfaceWebList)
		}
		if ev.ListPosition == nil || *ev.ListPosition != 3 {
			t.Errorf("list_position = %v, want 3 -- position bias is uncorrectable without it", ev.ListPosition)
		}
	})

	t.Run("mark-all-read records dismissal, not engagement", func(t *testing.T) {
		tf := newTestFixtures(t)

		rr := authedRequestForm(t, tf, "POST", "/articles/mark-all-read",
			url.Values{"ids": {fmt.Sprint(tf.articleID)}})
		if rr.Code != http.StatusNoContent {
			t.Fatalf("mark-all-read: status %d", rr.Code)
		}

		ev := onlyEvent(t, tf)
		if ev.Kind != string(storage.FeedbackBulkDismissed) {
			t.Errorf("kind = %q, want %q -- clearing the queue is not engagement", ev.Kind, storage.FeedbackBulkDismissed)
		}
		if ev.ArticleID == nil || *ev.ArticleID != tf.articleID {
			t.Errorf("article_id = %v, want %d: bulk dismissal is recorded per-article", ev.ArticleID, tf.articleID)
		}
	})
}

// TestFeedbackReadToggleKinds: marking read without opening is a dismissal;
// marking unread again is the reader asking for it back. Opposite polarity, so
// they cannot share a kind.
func TestFeedbackReadToggleKinds(t *testing.T) {
	tf := newTestFixtures(t)
	path := fmt.Sprintf("/articles/%d/read", tf.articleID)

	if rr := authedRequestForm(t, tf, "POST", path, url.Values{"read": {"true"}}); rr.Code != http.StatusOK {
		t.Fatalf("mark read: status %d", rr.Code)
	}
	if rr := authedRequestForm(t, tf, "POST", path, url.Values{"read": {"false"}}); rr.Code != http.StatusOK {
		t.Fatalf("mark unread: status %d", rr.Code)
	}

	kinds := feedbackKinds(t, tf)
	if len(kinds) != 2 {
		t.Fatalf("got %d events (%v), want 2", len(kinds), kinds)
	}
	// Newest first.
	if kinds[0] != string(storage.FeedbackReadToggledOff) {
		t.Errorf("second event kind = %q, want %q", kinds[0], storage.FeedbackReadToggledOff)
	}
	if kinds[1] != string(storage.FeedbackReadToggledOn) {
		t.Errorf("first event kind = %q, want %q", kinds[1], storage.FeedbackReadToggledOn)
	}
}

// TestFeedbackStarKinds: a star is a strong positive and an unstar retracts it.
// An unstar is NOT a negative and must not be recorded as one.
func TestFeedbackStarKinds(t *testing.T) {
	tf := newTestFixtures(t)
	path := fmt.Sprintf("/articles/%d/star", tf.articleID)

	if rr := authedRequestForm(t, tf, "POST", path, url.Values{"starred": {"true"}}); rr.Code != http.StatusOK {
		t.Fatalf("star: status %d", rr.Code)
	}
	if rr := authedRequestForm(t, tf, "POST", path, url.Values{"starred": {"false"}}); rr.Code != http.StatusOK {
		t.Fatalf("unstar: status %d", rr.Code)
	}

	kinds := feedbackKinds(t, tf)
	if len(kinds) != 2 {
		t.Fatalf("got %d events (%v), want 2", len(kinds), kinds)
	}
	if kinds[0] != string(storage.FeedbackUnstar) || kinds[1] != string(storage.FeedbackStar) {
		t.Errorf("kinds = %v, want [unstar star] newest-first", kinds)
	}
}

// TestFeedbackFeverReadIsSeparable: Fever clients auto-mark on scroll with
// behavior Herald cannot introspect, so their reads must never be mixed in with
// deliberate opens from the web reader.
func TestFeedbackFeverReadIsSeparable(t *testing.T) {
	tf := newTestFixtures(t)

	const email, password = "tester@example.com", "hunter2"
	if err := tf.engine.SetFeverCredential(tf.userID, email, password); err != nil {
		t.Fatalf("SetFeverCredential: %v", err)
	}
	apiKey := fmt.Sprintf("%x", md5.Sum([]byte(email+":"+password))) //nolint:gosec // Fever protocol defines the key as MD5(email:password); not a security choice

	form := url.Values{
		"api_key": {apiKey},
		"mark":    {"item"},
		"as":      {"read"},
		"id":      {fmt.Sprint(tf.articleID)},
	}
	req := httptest.NewRequest("POST", "/fever/?api", nil)
	req.PostForm = form
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	tf.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("fever mark: status %d", rr.Code)
	}

	ev := onlyEvent(t, tf)
	if ev.Kind != string(storage.FeedbackExternalRead) {
		t.Errorf("kind = %q, want %q -- a Fever read is not the same signal as opening an article",
			ev.Kind, storage.FeedbackExternalRead)
	}
	if ev.Surface != string(storage.SurfaceFever) {
		t.Errorf("surface = %q, want %q", ev.Surface, storage.SurfaceFever)
	}
}

// TestFeedbackUnsubscribeSnapshotsFeedHealth: the unsubscribe event must carry
// the feed's error state, so a consumer can tell "this feed is dead" from "I no
// longer care about this subject" and not downrank the feed's back catalogue
// for a server outage.
func TestFeedbackUnsubscribeSnapshotsFeedHealth(t *testing.T) {
	tf := newTestFixtures(t)

	for range 3 {
		if err := tf.store.UpdateFeedError(tf.feedID, "dial tcp: no such host"); err != nil {
			t.Fatalf("UpdateFeedError: %v", err)
		}
	}

	rr := authedRequest(t, tf, "DELETE", fmt.Sprintf("/feeds/%d", tf.feedID), nil)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe: status %d", rr.Code)
	}

	events, err := tf.store.ListFeedbackEvents(tf.userID, 50)
	if err != nil {
		t.Fatalf("ListFeedbackEvents: %v", err)
	}
	var unsub *storage.FeedbackEventRow
	for i := range events {
		if events[i].Kind == string(storage.FeedbackFeedUnsubscribe) {
			unsub = &events[i]
		}
	}
	if unsub == nil {
		t.Fatalf("no unsubscribe event recorded (kinds: %v)", feedbackKinds(t, tf))
	}
	if unsub.FeedErrors == nil || *unsub.FeedErrors < 3 {
		t.Errorf("feed_errors = %v, want >= 3: without it a dead feed is mined as a content negative", unsub.FeedErrors)
	}
}

// TestFeedbackClickthroughBeacon: leaving for the original site is a positive
// signal, but only interpretable against how much text the reader already had,
// so the event must carry the content covariates.
func TestFeedbackClickthroughBeacon(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "POST", fmt.Sprintf("/articles/%d/visit", tf.articleID), nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("visit beacon: status %d", rr.Code)
	}

	ev := onlyEvent(t, tf)
	if ev.Kind != string(storage.FeedbackClickthrough) {
		t.Errorf("kind = %q, want %q", ev.Kind, storage.FeedbackClickthrough)
	}
	if ev.ContentLength == nil {
		t.Error("content_length not captured: a clickthrough cannot be weighted without it")
	}
	if ev.HasFullText == nil {
		t.Error("has_full_text not captured")
	}
}

// TestFeedbackClickthroughRejectsOtherUsersArticle: the beacon takes an article
// ID from the request, so it must not let a reader write articles they do not
// subscribe to into their own corpus.
func TestFeedbackClickthroughRejectsOtherUsersArticle(t *testing.T) {
	tf := newTestFixtures(t)
	otherUserID, otherToken := secondTestUser(t, tf)

	rr := authedRequestAs(t, tf, otherToken, "POST", fmt.Sprintf("/articles/%d/visit", tf.articleID))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("visit beacon: status %d", rr.Code)
	}

	events, err := tf.store.ListFeedbackEvents(otherUserID, 10)
	if err != nil {
		t.Fatalf("ListFeedbackEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events for an article the user does not subscribe to, want none", len(events))
	}
}

// TestOpenSurfaceRejectsForgedValues: the surface and position ride in on query
// parameters, which are reader-controlled. Unknown values must not be written
// through into the corpus.
func TestOpenSurfaceRejectsForgedValues(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  storage.FeedbackSurface
	}{
		{"", storage.SurfaceWebArticle},
		{"?from=web-list", storage.SurfaceWebList},
		{"?from=web-search", storage.SurfaceWebSearch},
		{"?from=%27+OR+1%3D1--", storage.SurfaceWebArticle},
		{"?from=totally-made-up", storage.SurfaceWebArticle},
	} {
		req := httptest.NewRequest("GET", "/articles/1"+tc.query, nil)
		if got := openSurface(req); got != tc.want {
			t.Errorf("openSurface(%q) = %q, want %q", tc.query, got, tc.want)
		}
	}

	for _, tc := range []struct {
		query string
		want  *int
	}{
		{"", nil},
		{"?pos=0", nil},
		{"?pos=-5", nil},
		{"?pos=99999999", nil},
		{"?pos=notanumber", nil},
	} {
		req := httptest.NewRequest("GET", "/articles/1"+tc.query, nil)
		if got := openPosition(req); got != nil {
			t.Errorf("openPosition(%q) = %d, want nil", tc.query, *got)
		}
		_ = tc.want
	}

	req := httptest.NewRequest("GET", "/articles/1?pos=7", nil)
	if got := openPosition(req); got == nil || *got != 7 {
		t.Errorf("openPosition(?pos=7) = %v, want 7", got)
	}
}
