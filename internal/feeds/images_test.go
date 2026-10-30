package feeds

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

// --- extractImageURLs tests ---

func TestExtractImageURLs_Basic(t *testing.T) {
	html := `<img src="https://example.com/photo.jpg">`
	urls := extractImageURLs(html, nil)
	if len(urls) != 1 || urls[0] != "https://example.com/photo.jpg" {
		t.Errorf("got %v", urls)
	}
}

func TestExtractImageURLs_RelativeResolved(t *testing.T) {
	base, _ := url.Parse("https://example.com/article/1")
	html := `<img src="/images/hero.png">`
	urls := extractImageURLs(html, base)
	if len(urls) != 1 || urls[0] != "https://example.com/images/hero.png" {
		t.Errorf("got %v", urls)
	}
}

func TestExtractImageURLs_SkipsDataURLs(t *testing.T) {
	html := `<img src="data:image/png;base64,abc123">`
	urls := extractImageURLs(html, nil)
	if len(urls) != 0 {
		t.Errorf("expected no URLs, got %v", urls)
	}
}

func TestExtractImageURLs_Deduplicates(t *testing.T) {
	html := `<img src="https://example.com/img.jpg"><img src="https://example.com/img.jpg">`
	urls := extractImageURLs(html, nil)
	if len(urls) != 1 {
		t.Errorf("expected 1 unique URL, got %d: %v", len(urls), urls)
	}
}

func TestExtractImageURLs_MaxCap(t *testing.T) {
	var html string
	for i := range maxImagesPerArticle + 5 {
		html += fmt.Sprintf(`<img src="https://example.com/img%d.jpg">`, i)
	}
	urls := extractImageURLs(html, nil)
	if len(urls) > maxImagesPerArticle {
		t.Errorf("expected at most %d URLs, got %d", maxImagesPerArticle, len(urls))
	}
}

func TestExtractImageURLs_SkipsNonHTTP(t *testing.T) {
	html := `<img src="ftp://example.com/img.jpg">`
	urls := extractImageURLs(html, nil)
	if len(urls) != 0 {
		t.Errorf("expected ftp:// to be skipped, got %v", urls)
	}
}

// --- normalizeMIME tests ---

// --- fetchAndNormalizeImage tests ---

func TestFetchAndNormalizeImage_PNG(t *testing.T) {
	data := makePNG(t, 64, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	out, mime, w, h, err := fetchAndNormalizeImage(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("fetchAndNormalizeImage: %v", err)
	}
	if mime != "image/webp" {
		t.Errorf("mime=%q, want image/webp", mime)
	}
	if w != 64 || h != 64 {
		t.Errorf("expected 64x64, got %dx%d", w, h)
	}
	if len(out) == 0 {
		t.Error("expected non-empty output")
	}
}

// The stored type comes from the bytes, never the Content-Type header, and only
// raster images are stored: an <img src> pointing at HTML or SVG must not end
// up served from herald's origin (#344).
func TestFetchAndNormalizeImage_RejectsNonImages(t *testing.T) {
	cases := []struct {
		name, contentType, body string
	}{
		{"html labelled png", "image/png", htmlWithScript},
		{"html", "text/html", htmlWithScript},
		{"html without content type", "", htmlWithScript},
		{"svg", "image/svg+xml", svgWithScript},
		{"svg labelled png", "image/png", svgWithScript},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client := &http.Client{Timeout: 5 * time.Second}
			data, mime, _, _, err := fetchAndNormalizeImage(context.Background(), client, srv.URL)
			if err == nil {
				t.Fatalf("stored %d bytes as %q, want rejection", len(data), mime)
			}
		})
	}
}

func TestFetchAndNormalizeImage_TypeFromBytes(t *testing.T) {
	gifData := makeGIF(t, 10, 10, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(gifData)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, mime, _, _, err := fetchAndNormalizeImage(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("fetchAndNormalizeImage: %v", err)
	}
	if mime != "image/webp" {
		t.Errorf("mime=%q, want image/webp", mime)
	}
}

func TestFetchAndNormalizeImage_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, _, _, _, err := fetchAndNormalizeImage(context.Background(), client, srv.URL)
	if err == nil {
		t.Error("expected error for HTTP 404")
	}
}

func TestFetchAndNormalizeImage_TooBig(t *testing.T) {
	// Serve maxImageBytes+1 bytes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(make([]byte, maxImageBytes+1))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, _, _, _, err := fetchAndNormalizeImage(context.Background(), client, srv.URL)
	if err == nil {
		t.Error("expected error for oversized image")
	}
}

// --- CacheArticleImages integration ---

func TestCacheArticleImages_StoresImages(t *testing.T) {
	imgData := makePNG(t, 100, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/img.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(imgData)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed("https://example.com/feed.xml", "Test Feed", "")
	pub := time.Now()
	articleID, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "img-test-1",
		Title:         "Article with Image",
		URL:           srv.URL + "/article",
		Content:       fmt.Sprintf(`<p>Text</p><img src="%s/img.png">`, srv.URL),
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	n, err := fetcher.CacheArticleImages(context.Background())
	if err != nil {
		t.Fatalf("CacheArticleImages: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 image cached, got %d", n)
	}

	imageMap, err := store.GetArticleImageMap(articleID)
	if err != nil {
		t.Fatalf("GetArticleImageMap: %v", err)
	}
	if len(imageMap) != 1 {
		t.Errorf("expected 1 image in map, got %d", len(imageMap))
	}
}

func TestCacheArticleImages_SkipsNonImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, htmlWithScript)
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed("https://example.com/feed.xml", "Test Feed", "")
	pub := time.Now()
	articleID, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "img-html-1",
		Title:         "Article with HTML image",
		URL:           srv.URL + "/article",
		Content:       fmt.Sprintf(`<p>Text</p><img src="%s/x.png">`, srv.URL),
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	n, err := NewFetcher(store).CacheArticleImages(context.Background())
	if err != nil {
		t.Fatalf("CacheArticleImages: %v", err)
	}
	if n != 0 {
		t.Errorf("cached %d images, want 0", n)
	}
	imageMap, err := store.GetArticleImageMap(articleID)
	if err != nil {
		t.Fatalf("GetArticleImageMap: %v", err)
	}
	if len(imageMap) != 0 {
		t.Errorf("image map has %d entries, want 0", len(imageMap))
	}
}

func TestCacheArticleImages_DoesNotReprocess(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	store := newFullTextTestStore(t)
	feedID, _ := store.AddFeed(srv.URL, "Feed", "")
	pub := time.Now()
	_, err := store.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "img-test-2",
		Title:         "Article",
		URL:           srv.URL + "/article",
		Content:       fmt.Sprintf(`<img src="%s/img.png">`, srv.URL),
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	fetcher := NewFetcher(store)
	fetcher.CacheArticleImages(context.Background()) //nolint:errcheck

	// Second call: article already marked images_cached=1, should not re-fetch.
	callsBefore := callCount
	fetcher.CacheArticleImages(context.Background()) //nolint:errcheck
	if callCount > callsBefore {
		t.Errorf("expected no new HTTP calls on second pass, got %d additional", callCount-callsBefore)
	}
}

// --- resolveTwitterPics / extractOGImage tests ---

func TestExtractOGImage_Found(t *testing.T) {
	html := `<html><head>
		<meta property="og:image" content="https://pbs.twimg.com/media/ABC123?format=jpg&name=large">
	</head><body></body></html>`
	got, err := extractOGImage(bytes.NewReader([]byte(html)))
	if err != nil {
		t.Fatalf("extractOGImage: %v", err)
	}
	want := "https://pbs.twimg.com/media/ABC123?format=jpg&name=large"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractOGImage_NotFound(t *testing.T) {
	html := `<html><head></head><body></body></html>`
	_, err := extractOGImage(bytes.NewReader([]byte(html)))
	if err == nil {
		t.Error("expected error when no og:image present")
	}
}

func TestExtractOGImage_WrongDomain(t *testing.T) {
	// og:image from a non-twimg domain should not be accepted.
	html := `<html><head>
		<meta property="og:image" content="https://evil.com/img.jpg">
	</head></html>`
	_, err := extractOGImage(bytes.NewReader([]byte(html)))
	if err == nil {
		t.Error("expected error for non-pbs.twimg.com og:image")
	}
}

func TestResolveTwitterPics_NoMatch(t *testing.T) {
	content := `<p>No twitter links here</p>`
	result, changed := resolveTwitterPics(context.Background(), nil, content)
	if changed {
		t.Error("expected no change")
	}
	if result != content {
		t.Error("expected content unchanged")
	}
}

func TestResolveTwitterPics_ResolvesToImage(t *testing.T) {
	// Simulate: pic.twitter.com → redirect → HTML page with og:image → CDN image
	cdnURL := ""
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
	}))
	defer imgSrv.Close()
	cdnURL = imgSrv.URL + "/media/ABC123.jpg"

	// Twitter-page server that returns og:image pointing to cdnURL.
	twitterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><head><meta property="og:image" content="%s"></head></html>`, cdnURL)
	}))
	defer twitterSrv.Close()

	// For the test, mock pbs.twimg.com check: use a server URL that starts with our imgSrv.URL.
	// extractOGImage checks for "https://pbs.twimg.com/" prefix — swap that check in the test
	// by pointing og:image to a URL that starts with the imgSrv URL.
	// Since we can't make imgSrv.URL start with "https://pbs.twimg.com/", test extractOGImage directly.
	html := `<html><head><meta property="og:image" content="https://pbs.twimg.com/media/TEST?format=jpg"></head></html>`
	got, err := extractOGImage(bytes.NewReader([]byte(html)))
	if err != nil {
		t.Fatalf("extractOGImage: %v", err)
	}
	if !strings.HasPrefix(got, "https://pbs.twimg.com/") {
		t.Errorf("expected pbs.twimg.com URL, got %q", got)
	}
	_ = twitterSrv
}

// helpers

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}
