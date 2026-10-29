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

func TestClassifyFaviconFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"404", &faviconHTTPError{status: 404}, "permanent"},
		{"403", &faviconHTTPError{status: 403}, "permanent"},
		{"410", &faviconHTTPError{status: 410}, "permanent"},
		{"451", &faviconHTTPError{status: 451}, "permanent"},
		{"401", &faviconHTTPError{status: 401}, "permanent"},
		{"500", &faviconHTTPError{status: 500}, "transient"},
		{"503", &faviconHTTPError{status: 503}, "transient"},
		{"429", &faviconHTTPError{status: 429}, "transient"},
		{"empty", fmt.Errorf("%w at https://x/favicon.ico", errEmptyFavicon), "permanent"},
		{"wrapped-404", fmt.Errorf("fetch: %w", &faviconHTTPError{status: 404}), "permanent"},
		{"network", fmt.Errorf("dial tcp: i/o timeout"), "transient"},
		{"not an image", fmt.Errorf("favicon: %w", errUnsupportedImage), "permanent"},
	}
	for _, c := range cases {
		if got := classifyFaviconFailure(c.err); got != c.want {
			t.Errorf("%s: classifyFaviconFailure = %q, want %q", c.name, got, c.want)
		}
	}
}

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
	if mime != "image/webp" {
		t.Errorf("mime=%q, want image/webp", mime)
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

// A site whose icon link points at HTML or SVG gets no favicon, and the
// failure is permanent so it is not re-fetched every cycle (#344).
func TestFetchFavicon_RejectsNonImages(t *testing.T) {
	for name, tc := range map[string]struct{ contentType, body string }{
		"html": {"text/html", htmlWithScript},
		"svg":  {"image/svg+xml", svgWithScript},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/":
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, `<html><head><link rel="icon" href="/icon"></head></html>`)
				case "/icon":
					w.Header().Set("Content-Type", tc.contentType)
					fmt.Fprint(w, tc.body)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			client := &http.Client{Timeout: 5 * time.Second}
			_, mime, err := fetchFavicon(context.Background(), client, srv.URL+"/feed.xml")
			if err == nil {
				t.Fatalf("stored favicon as %q, want rejection", mime)
			}
			if kind := classifyFaviconFailure(err); kind != "permanent" {
				t.Errorf("failure kind = %q, want permanent", kind)
			}
		})
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
	if fav.MimeType != "image/webp" {
		t.Errorf("mime=%q, want image/webp", fav.MimeType)
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
	for y := range h {
		for x := range w {
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
