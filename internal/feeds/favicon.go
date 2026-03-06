package feeds

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// maxFaviconBytes is the largest raw response we'll accept before giving up.
const maxFaviconBytes = 256 * 1024 // 256 KB

// maxFaviconDim is the maximum width or height of a stored favicon.
// Images larger than this are resized (nearest-neighbour) before storing.
const maxFaviconDim = 64

// FetchFaviconsForFeeds fetches and caches favicons for all subscribed feeds
// that don't yet have a cached favicon. Each feed is processed at most once;
// failures are logged and skipped without retry.
//
// Returns the number of favicons successfully stored.
func (f *Fetcher) FetchFaviconsForFeeds(ctx context.Context) (int, error) {
	feeds, err := f.store.GetSubscribedFeedsWithoutFavicons()
	if err != nil {
		return 0, fmt.Errorf("get feeds without favicons: %w", err)
	}

	stored := 0
	for _, feed := range feeds {
		if ctx.Err() != nil {
			break
		}
		data, mimeType, err := fetchFavicon(ctx, f.client, feed.URL)
		if err != nil {
			log.Printf("herald: favicon fetch failed for feed %d (%s): %v", feed.ID, feed.URL, err)
			continue
		}
		if err := f.store.StoreFeedFavicon(feed.ID, data, mimeType); err != nil {
			log.Printf("herald: failed to store favicon for feed %d: %v", feed.ID, err)
			continue
		}
		stored++
	}
	return stored, nil
}

// fetchFavicon fetches the best favicon for the site at feedURL.
//
// Strategy:
//  1. Derive the site root from feedURL's scheme+host.
//  2. Fetch the site root HTML and look for <link rel="icon"> / <link rel="shortcut icon">.
//  3. If found, fetch that URL.
//  4. If not found or the fetch fails, fall back to scheme://host/favicon.ico.
//
// The returned data is either a resized PNG (for decodable PNG/JPEG sources)
// or the raw bytes for formats we can't decode (ICO, GIF, etc.).
func fetchFavicon(ctx context.Context, client *http.Client, feedURL string) ([]byte, string, error) {
	parsed, err := url.Parse(feedURL)
	if err != nil || parsed.Host == "" {
		return nil, "", fmt.Errorf("invalid feed URL %q: %w", feedURL, err)
	}
	siteRoot := &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}

	// Try to find a <link rel="icon"> in the site homepage.
	iconURL := findIconLinkInPage(ctx, client, siteRoot.String())

	// Fall back to /favicon.ico if we couldn't find a <link rel="icon">.
	if iconURL == "" {
		iconURL = siteRoot.ResolveReference(&url.URL{Path: "/favicon.ico"}).String()
	}

	return fetchAndNormalize(ctx, client, iconURL)
}

// findIconLinkInPage fetches pageURL and returns the href of the first
// <link rel="icon"> or <link rel="shortcut icon"> element, resolved to an
// absolute URL. Returns "" on any error or if no icon link is found.
func findIconLinkInPage(ctx context.Context, client *http.Client, pageURL string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Herald/1.0)")
	req.Header.Set("Accept", "text/html,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(ct, "html") {
		return ""
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return ""
	}

	base, _ := url.Parse(pageURL)
	return extractIconHref(body, base)
}

// extractIconHref parses HTML and returns the href of the first icon <link>.
func extractIconHref(body []byte, base *url.URL) string {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return ""
	}

	var found string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != "" {
			return
		}
		if n.Type == html.ElementNode && n.Data == "body" {
			return // icon links are always in <head>
		}
		if n.Type == html.ElementNode && n.Data == "link" {
			attrs := nodeAttrs(n.Attr)
			rel := strings.ToLower(strings.TrimSpace(attrs["rel"]))
			if rel == "icon" || rel == "shortcut icon" {
				href := strings.TrimSpace(attrs["href"])
				if href != "" && base != nil {
					if ref, err := base.Parse(href); err == nil {
						found = ref.String()
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

// fetchAndNormalize fetches iconURL and returns (data, mimeType).
// PNG and JPEG images are decoded, resized if needed, and re-encoded as PNG.
// Other formats (ICO, GIF, SVG) are stored as raw bytes up to maxFaviconBytes.
func fetchAndNormalize(ctx context.Context, client *http.Client, iconURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, iconURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Herald/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d for %s", resp.StatusCode, iconURL)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFaviconBytes))
	if err != nil {
		return nil, "", fmt.Errorf("read favicon: %w", err)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("empty favicon at %s", iconURL)
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	// Normalize Content-Type; some servers send "image/x-icon" for ICO.
	switch {
	case strings.Contains(ct, "png"):
		ct = "image/png"
	case strings.Contains(ct, "jpeg") || strings.Contains(ct, "jpg"):
		ct = "image/jpeg"
	}

	// For PNG and JPEG we can decode, resize if needed, and re-encode as PNG.
	if ct == "image/png" || ct == "image/jpeg" {
		if normalized, err := normalizeFavicon(data); err == nil {
			return normalized, "image/png", nil
		}
		// Fall through to raw storage if decode/encode fails.
	}

	// Fallback: store raw bytes with sniffed MIME type.
	if ct == "" || ct == "application/octet-stream" {
		ct = http.DetectContentType(data)
	}
	return data, ct, nil
}

// normalizeFavicon decodes img data, resizes to maxFaviconDim if needed,
// and returns PNG-encoded bytes.
func normalizeFavicon(data []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxFaviconDim && h <= maxFaviconDim {
		// Already small enough — just re-encode as PNG.
		var buf bytes.Buffer
		if err := png.Encode(&buf, src); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	// Scale down proportionally.
	if w > h {
		h = h * maxFaviconDim / w
		w = maxFaviconDim
	} else {
		w = w * maxFaviconDim / h
		h = maxFaviconDim
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), nearestNeighbourScaled{src, b, w, h}, image.Point{}, draw.Src)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// nearestNeighbourScaled implements image.Image by mapping destination
// pixels back to the source using nearest-neighbour interpolation.
type nearestNeighbourScaled struct {
	src        image.Image
	srcBounds  image.Rectangle
	dstW, dstH int
}

func (n nearestNeighbourScaled) ColorModel() color.Model {
	return n.src.ColorModel()
}

func (n nearestNeighbourScaled) Bounds() image.Rectangle {
	return image.Rect(0, 0, n.dstW, n.dstH)
}

func (n nearestNeighbourScaled) At(x, y int) color.Color {
	srcX := x * n.srcBounds.Dx() / n.dstW
	srcY := y * n.srcBounds.Dy() / n.dstH
	return n.src.At(n.srcBounds.Min.X+srcX, n.srcBounds.Min.Y+srcY)
}

// jpeg import used for image.Decode side-effect registration.
var _ = jpeg.Decode
