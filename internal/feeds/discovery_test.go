package feeds

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testHTMLWithFeeds = `<!DOCTYPE html>
<html><head>
    <title>My Blog</title>
    <link rel="alternate" type="application/rss+xml" title="My Blog RSS" href="/rss.xml">
    <link rel="alternate" type="application/atom+xml" title="My Blog Atom" href="/atom.xml">
</head><body><p>Hello</p></body></html>`

const testHTMLNoFeeds = `<!DOCTYPE html>
<html><head><title>No Feeds Here</title></head><body><p>Nothing</p></body></html>`

func TestDiscoverFeeds_HTMLLinks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, testHTMLWithFeeds)
	}))
	defer srv.Close()

	store, cleanup := newTestStore(t)
	defer cleanup()

	fetcher := NewFetcher(store)
	results, err := fetcher.DiscoverFeeds(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("DiscoverFeeds: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 feeds, got %d", len(results))
	}

	rssFound, atomFound := false, false
	for _, r := range results {
		switch r.Type {
		case "rss":
			rssFound = true
			if r.Title != "My Blog RSS" {
				t.Errorf("RSS title=%q, want %q", r.Title, "My Blog RSS")
			}
		case "atom":
			atomFound = true
			if r.Title != "My Blog Atom" {
				t.Errorf("Atom title=%q, want %q", r.Title, "My Blog Atom")
			}
		}
	}
	if !rssFound {
		t.Error("RSS feed not found in results")
	}
	if !atomFound {
		t.Error("Atom feed not found in results")
	}
}

func TestDiscoverFeeds_RelativeURLResolution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><head>
    <link rel="alternate" type="application/rss+xml" href="/rss.xml">
</head></html>`)
	}))
	defer srv.Close()

	store, cleanup := newTestStore(t)
	defer cleanup()

	fetcher := NewFetcher(store)
	results, err := fetcher.DiscoverFeeds(context.Background(), srv.URL+"/blog/post")
	if err != nil {
		t.Fatalf("DiscoverFeeds: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 feed, got %d", len(results))
	}
	want := srv.URL + "/rss.xml"
	if results[0].URL != want {
		t.Errorf("URL=%q, want %q", results[0].URL, want)
	}
}

func TestDiscoverFeeds_DirectFeedURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, testRSS)
	}))
	defer srv.Close()

	store, cleanup := newTestStore(t)
	defer cleanup()

	fetcher := NewFetcher(store)
	results, err := fetcher.DiscoverFeeds(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("DiscoverFeeds: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for direct feed URL, got %d", len(results))
	}
	if results[0].URL != srv.URL {
		t.Errorf("URL=%q, want %q", results[0].URL, srv.URL)
	}
	if results[0].Type != "rss" {
		t.Errorf("Type=%q, want rss", results[0].Type)
	}
}

func TestDiscoverFeeds_NoFeedsInHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// All paths — including common feed probe paths — return plain HTML.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, testHTMLNoFeeds)
	}))
	defer srv.Close()

	store, cleanup := newTestStore(t)
	defer cleanup()

	fetcher := NewFetcher(store)
	results, err := fetcher.DiscoverFeeds(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("DiscoverFeeds: %v", err)
	}
	// HTML autodiscovery finds nothing; probe paths return unparseable HTML.
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestDiscoverFeeds_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	store, cleanup := newTestStore(t)
	defer cleanup()

	fetcher := NewFetcher(store)
	_, err := fetcher.DiscoverFeeds(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}
}

func TestExtractFeedLinks_Deduplication(t *testing.T) {
	body := []byte(`<html><head>
    <link rel="alternate" type="application/rss+xml" href="/feed.xml" title="Feed">
    <link rel="alternate" type="application/rss+xml" href="/feed.xml" title="Dupe">
</head></html>`)
	results := extractFeedLinks(body, nil)
	if len(results) != 1 {
		t.Errorf("expected 1 result after dedup, got %d", len(results))
	}
}

func TestExtractFeedLinks_SkipsBodyLinks(t *testing.T) {
	body := []byte(`<html><head>
    <link rel="alternate" type="application/rss+xml" href="/head-feed.xml">
</head><body>
    <link rel="alternate" type="application/rss+xml" href="/body-feed.xml">
</body></html>`)
	results := extractFeedLinks(body, nil)
	if len(results) != 1 {
		t.Errorf("expected 1 result (head only), got %d", len(results))
	}
	if results[0].URL != "/head-feed.xml" {
		t.Errorf("URL=%q, want /head-feed.xml", results[0].URL)
	}
}

func TestIsFeedContentType(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"application/rss+xml; charset=utf-8", true},
		{"application/atom+xml", true},
		{"application/rdf+xml", true},
		{"text/xml; charset=utf-8", true},
		{"application/xml", true},
		{"application/json", true},
		{"text/html; charset=utf-8", false},
		{"text/plain", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isFeedContentType(tc.ct); got != tc.want {
			t.Errorf("isFeedContentType(%q) = %v, want %v", tc.ct, got, tc.want)
		}
	}
}
