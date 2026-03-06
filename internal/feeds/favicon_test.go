package feeds

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

// --- extractIconHref tests ---

func TestExtractIconHref_BasicIcon(t *testing.T) {
	body := []byte(`<html><head>
		<link rel="icon" href="/favicon.png">
	</head><body></body></html>`)
	href := extractIconHref(body, mustParseURL(t, "https://example.com"))
	if href != "https://example.com/favicon.png" {
		t.Errorf("got %q, want https://example.com/favicon.png", href)
	}
}

func TestExtractIconHref_ShortcutIcon(t *testing.T) {
	body := []byte(`<html><head>
		<link rel="shortcut icon" href="/ico/site.ico">
	</head><body></body></html>`)
	href := extractIconHref(body, mustParseURL(t, "https://example.com"))
	if href != "https://example.com/ico/site.ico" {
		t.Errorf("got %q, want https://example.com/ico/site.ico", href)
	}
}

func TestExtractIconHref_AbsoluteHref(t *testing.T) {
	body := []byte(`<html><head>
		<link rel="icon" href="https://cdn.example.com/icon.png">
	</head></html>`)
	href := extractIconHref(body, mustParseURL(t, "https://example.com"))
	if href != "https://cdn.example.com/icon.png" {
		t.Errorf("got %q", href)
	}
}

func TestExtractIconHref_NoIcon(t *testing.T) {
	body := []byte(`<html><head><title>No Icon</title></head><body></body></html>`)
	href := extractIconHref(body, mustParseURL(t, "https://example.com"))
	if href != "" {
		t.Errorf("expected empty, got %q", href)
	}
}

func TestExtractIconHref_IgnoresBodyLinks(t *testing.T) {
	// Links inside <body> should be ignored.
	body := []byte(`<html><head></head><body>
		<link rel="icon" href="/body-icon.png">
	</body></html>`)
	href := extractIconHref(body, mustParseURL(t, "https://example.com"))
	if href != "" {
		t.Errorf("expected empty for body link, got %q", href)
	}
}

// --- normalizeFavicon tests ---

func TestNormalizeFavicon_SmallPNG(t *testing.T) {
	// A 16x16 PNG should be stored as-is (re-encoded as PNG).
	data := makePNG(t, 16, 16)
	out, err := normalizeFavicon(data)
	if err != nil {
		t.Fatalf("normalizeFavicon: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not valid PNG: %v", err)
	}
	if img.Bounds().Dx() != 16 || img.Bounds().Dy() != 16 {
		t.Errorf("expected 16x16, got %dx%d", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestNormalizeFavicon_LargePNG(t *testing.T) {
	// A 512x512 PNG should be resized to 64x64.
	data := makePNG(t, 512, 512)
	out, err := normalizeFavicon(data)
	if err != nil {
		t.Fatalf("normalizeFavicon: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not valid PNG: %v", err)
	}
	if img.Bounds().Dx() > maxFaviconDim || img.Bounds().Dy() > maxFaviconDim {
		t.Errorf("expected max %d, got %dx%d", maxFaviconDim, img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestNormalizeFavicon_NonSquare(t *testing.T) {
	// 128x32 → should fit within 64x64 preserving aspect ratio.
	data := makePNG(t, 128, 32)
	out, err := normalizeFavicon(data)
	if err != nil {
		t.Fatalf("normalizeFavicon: %v", err)
	}
	img, _ := png.Decode(bytes.NewReader(out))
	if img.Bounds().Dx() > maxFaviconDim || img.Bounds().Dy() > maxFaviconDim {
		t.Errorf("dimension exceeded maxFaviconDim: %dx%d", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

func TestNormalizeFavicon_InvalidData(t *testing.T) {
	_, err := normalizeFavicon([]byte("not an image"))
	if err == nil {
		t.Error("expected error for invalid image data")
	}
}

// --- fetchFavicon integration tests ---

func TestFetchFavicon_UsesIconLink(t *testing.T) {
	iconData := makePNG(t, 32, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><head><link rel="icon" href="/icon.png"></head></html>`)
		case "/icon.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(iconData)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	data, mime, err := fetchFavicon(context.Background(), client, srv.URL+"/feed.xml")
	if err != nil {
		t.Fatalf("fetchFavicon: %v", err)
	}
	if mime != "image/png" {
		t.Errorf("mime=%q, want image/png", mime)
	}
	if len(data) == 0 {
		t.Error("expected non-empty data")
	}
}

func TestFetchFavicon_FallsBackToFaviconIco(t *testing.T) {
	iconData := makePNG(t, 16, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			// HTML with no icon link
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><title>No icon</title></head></html>`)
		case "/favicon.ico":
			w.Header().Set("Content-Type", "image/png")
			w.Write(iconData)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	data, _, err := fetchFavicon(context.Background(), client, srv.URL+"/feed.xml")
	if err != nil {
		t.Fatalf("fetchFavicon fallback: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty data from fallback")
	}
}

func TestFetchFavicon_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, _, err := fetchFavicon(context.Background(), client, srv.URL+"/feed.xml")
	if err == nil {
		t.Error("expected error when all favicon sources return 404")
	}
}

// --- FetchFaviconsForFeeds integration ---

func TestFetchFaviconsForFeeds(t *testing.T) {
	iconData := makePNG(t, 16, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><link rel="icon" href="/icon.png"></head></html>`)
		case "/icon.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(iconData)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := newFullTextTestStore(t) // reuse helper from fulltext_test.go
	feedID, _ := store.AddFeed(srv.URL+"/feed.xml", "Test Feed", "")
	store.SubscribeUserToFeed(1, feedID)

	fetcher := NewFetcher(store)
	n, err := fetcher.FetchFaviconsForFeeds(context.Background())
	if err != nil {
		t.Fatalf("FetchFaviconsForFeeds: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 favicon stored, got %d", n)
	}

	fav, err := store.GetFeedFavicon(feedID)
	if err != nil {
		t.Fatalf("GetFeedFavicon: %v", err)
	}
	if fav == nil {
		t.Fatal("expected favicon to be stored")
	}
	if fav.MimeType != "image/png" {
		t.Errorf("mime=%q, want image/png", fav.MimeType)
	}
}

func TestFetchFaviconsForFeeds_DoesNotRefetch(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			callCount++
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(srv.URL+"/feed.xml", "Already Got It", "")
	store.SubscribeUserToFeed(1, feedID)

	// Pre-seed a favicon.
	store.StoreFeedFavicon(feedID, makePNG(t, 16, 16), "image/png")

	fetcher := NewFetcher(store)
	n, _ := fetcher.FetchFaviconsForFeeds(context.Background())
	if n != 0 {
		t.Errorf("expected 0 new favicons, got %d", n)
	}
	if callCount != 0 {
		t.Errorf("expected no HTTP calls for pre-seeded favicon, got %d", callCount)
	}
}

// helpers

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 100, G: 150, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func mustParseURL(t *testing.T, s string) *url.URL { //nolint:unparam
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", s, err)
	}
	return u
}

// storage.SubscribeUserToFeed needs a user to exist in some implementations.
// For SQLite the default user_id=1 requires no prior user row.
var _ = storage.Feed{}
