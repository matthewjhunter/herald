package feeds

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	for i := 0; i < maxImagesPerArticle+5; i++ {
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

func TestNormalizeMIME_PNG(t *testing.T) {
	if got := normalizeMIME("image/png", nil); got != "image/png" {
		t.Errorf("got %q", got)
	}
}

func TestNormalizeMIME_JPEGWithCharset(t *testing.T) {
	if got := normalizeMIME("image/jpeg; charset=utf-8", nil); got != "image/jpeg" {
		t.Errorf("got %q", got)
	}
}

func TestNormalizeMIME_Sniff(t *testing.T) {
	data := makeJPEG(t, 8, 8)
	if got := normalizeMIME("application/octet-stream", data); got != "image/jpeg" {
		t.Errorf("got %q, want image/jpeg", got)
	}
}

// --- resizeIfNeeded tests ---

func TestResizeIfNeeded_SmallPNG(t *testing.T) {
	data := makePNG(t, 32, 32)
	out, w, h, err := resizeIfNeeded(data, "image/png")
	if err != nil {
		t.Fatalf("resizeIfNeeded: %v", err)
	}
	if w != 32 || h != 32 {
		t.Errorf("expected 32x32, got %dx%d", w, h)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("output is not valid PNG: %v", err)
	}
}

func TestResizeIfNeeded_LargePNG(t *testing.T) {
	data := makePNG(t, 2000, 1000)
	out, w, h, err := resizeIfNeeded(data, "image/png")
	if err != nil {
		t.Fatalf("resizeIfNeeded: %v", err)
	}
	if w > maxImageDim || h > maxImageDim {
		t.Errorf("expected max %d, got %dx%d", maxImageDim, w, h)
	}
	if w != maxImageDim {
		t.Errorf("expected width=%d (was wider), got %d", maxImageDim, w)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("output is not valid PNG: %v", err)
	}
}

func TestResizeIfNeeded_JPEG(t *testing.T) {
	data := makeJPEG(t, 1600, 900)
	out, w, h, err := resizeIfNeeded(data, "image/jpeg")
	if err != nil {
		t.Fatalf("resizeIfNeeded JPEG: %v", err)
	}
	if w > maxImageDim || h > maxImageDim {
		t.Errorf("expected max %d, got %dx%d", maxImageDim, w, h)
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("output is not valid JPEG: %v", err)
	}
}

func TestResizeIfNeeded_InvalidData(t *testing.T) {
	_, _, _, err := resizeIfNeeded([]byte("not an image"), "image/png")
	if err == nil {
		t.Error("expected error for invalid image data")
	}
}

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
	if mime != "image/png" {
		t.Errorf("mime=%q, want image/png", mime)
	}
	if w != 64 || h != 64 {
		t.Errorf("expected 64x64, got %dx%d", w, h)
	}
	if len(out) == 0 {
		t.Error("expected non-empty output")
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

// helpers

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}
