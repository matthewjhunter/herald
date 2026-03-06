package feeds

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	// maxImageBytes is the largest image we'll fetch and store.
	maxImageBytes = 2 * 1024 * 1024 // 2 MB

	// maxImageDim is the maximum width or height of a stored image.
	// Images larger than this are resized before storage.
	maxImageDim = 1200

	// maxImagesPerArticle caps how many images we process per article to
	// guard against pathological feeds with hundreds of tiny images.
	maxImagesPerArticle = 20
)

// CacheArticleImages fetches and stores images referenced in article content
// for articles that haven't been processed yet. Each article is marked
// images_cached = 1 exactly once regardless of outcome, so failures on
// individual images are logged and skipped without blocking future cycles.
//
// Returns the total number of images newly stored.
func (f *Fetcher) CacheArticleImages(ctx context.Context) (int, error) {
	articles, err := f.store.GetArticlesNeedingImageCache(50)
	if err != nil {
		return 0, fmt.Errorf("get articles needing image cache: %w", err)
	}

	totalStored := 0
	for _, article := range articles {
		if ctx.Err() != nil {
			break
		}
		stored := f.cacheImagesForArticle(ctx, article.ID, article.URL, article.Content)
		totalStored += stored
		f.store.MarkArticleImagesCached(article.ID) //nolint:errcheck
	}
	return totalStored, nil
}

// picTwitterHrefRe matches href attribute values pointing to pic.twitter.com or pic.x.com.
var picTwitterHrefRe = regexp.MustCompile(`(?i)href="((?:https?://)?pic\.(?:twitter|x)\.com/[^"]+)"`)

// resolveTwitterPics finds pic.twitter.com anchor links in HTML content and
// attempts to replace them with <img> tags by following the redirect chain
// and extracting the og:image from the resulting Twitter page. This is purely
// best-effort: any URL that can't be resolved is left unchanged.
func resolveTwitterPics(ctx context.Context, client *http.Client, content string) (string, bool) {
	lower := strings.ToLower(content)
	if !strings.Contains(lower, "pic.twitter.com") && !strings.Contains(lower, "pic.x.com") {
		return content, false
	}
	matches := picTwitterHrefRe.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return content, false
	}

	changed := false
	for _, m := range matches {
		originalHref := m[1]
		picURL := originalHref
		if !strings.HasPrefix(strings.ToLower(picURL), "http") {
			picURL = "https://" + picURL
		}
		imageURL, err := resolveToImageURL(ctx, client, picURL)
		if err != nil {
			continue
		}
		// Replace <a ...href="ORIGINAL"...>...</a> with <img src="RESOLVED">.
		anchorRe := regexp.MustCompile(`(?is)<a\b[^>]*href="` + regexp.QuoteMeta(originalHref) + `"[^>]*>.*?</a>`)
		newContent := anchorRe.ReplaceAllString(content, `<img src="`+imageURL+`" alt="tweet image">`)
		if newContent != content {
			content = newContent
			changed = true
		}
	}
	return content, changed
}

// resolveToImageURL follows redirects from a pic.twitter.com URL and returns
// the actual CDN image URL. It checks for a direct image response first, then
// falls back to extracting og:image from the HTML head (which pbs.twimg.com
// populates in Twitter page meta tags when served without JS).
func resolveToImageURL(ctx context.Context, client *http.Client, picURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, picURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Herald/1.0)")

	httpClient := client
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	// If redirects landed us directly on an image (e.g. a CDN URL), use it.
	if strings.HasPrefix(ct, "image/") {
		return resp.Request.URL.String(), nil
	}
	// For HTML pages (Twitter), look for og:image pointing to pbs.twimg.com.
	if strings.Contains(ct, "text/html") {
		return extractOGImage(io.LimitReader(resp.Body, 64*1024))
	}
	return "", fmt.Errorf("unresolvable content type: %s", ct)
}

// extractOGImage parses an HTML document and returns the og:image meta content
// value if it points to pbs.twimg.com (Twitter's public image CDN).
func extractOGImage(r io.Reader) (string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	var found string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != "" {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Meta {
			if nodeAttr(n, "property") == "og:image" {
				if content := nodeAttr(n, "content"); strings.HasPrefix(content, "https://pbs.twimg.com/") {
					found = content
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if found == "" {
		return "", fmt.Errorf("no pbs.twimg.com og:image found")
	}
	return found, nil
}

// cacheImagesForArticle extracts and stores all cacheable images from content.
func (f *Fetcher) cacheImagesForArticle(ctx context.Context, articleID int64, articleURL, content string) int {
	if content == "" {
		return 0
	}

	// Best-effort: resolve pic.twitter.com anchor links to actual CDN images.
	if resolved, changed := resolveTwitterPics(ctx, f.client, content); changed {
		content = resolved
		if err := f.store.UpdateArticleContent(articleID, content); err != nil {
			log.Printf("herald: twitter pic resolution: content update failed for article %d: %v", articleID, err)
		}
	}

	var base *url.URL
	if articleURL != "" {
		base, _ = url.Parse(articleURL)
	}

	urls := extractImageURLs(content, base)
	stored := 0
	for _, imgURL := range urls {
		if ctx.Err() != nil {
			break
		}
		data, mimeType, w, h, err := fetchAndNormalizeImage(ctx, f.client, imgURL)
		if err != nil {
			log.Printf("herald: image cache failed for article %d (%s): %v", articleID, imgURL, err)
			continue
		}
		if _, err := f.store.StoreArticleImage(articleID, imgURL, data, mimeType, w, h); err != nil {
			log.Printf("herald: failed to store image for article %d: %v", articleID, err)
			continue
		}
		stored++
	}
	return stored
}

// extractImageURLs parses HTML and returns unique, absolute image src URLs.
// Data URLs (data:...) are skipped — they're already inline and don't need caching.
// At most maxImagesPerArticle URLs are returned.
func extractImageURLs(content string, base *url.URL) []string {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return nil
	}

	seen := make(map[string]bool)
	var urls []string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(urls) >= maxImagesPerArticle {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Img {
			src := nodeAttr(n, "src")
			if src != "" && !strings.HasPrefix(src, "data:") {
				resolved := src
				if base != nil {
					if ref, err := base.Parse(src); err == nil {
						resolved = ref.String()
					}
				}
				if !seen[resolved] && (strings.HasPrefix(resolved, "http://") || strings.HasPrefix(resolved, "https://")) {
					seen[resolved] = true
					urls = append(urls, resolved)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return urls
}

// fetchAndNormalizeImage fetches an image URL and normalizes it:
//   - PNG/JPEG: decode, resize to maxImageDim if needed, re-encode as same format
//   - GIF: store as-is (preserves animation)
//   - Other formats: store as-is if under maxImageBytes
//
// Returns (data, mimeType, width, height, error).
func fetchAndNormalizeImage(ctx context.Context, client *http.Client, imgURL string) ([]byte, string, int, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imgURL, nil)
	if err != nil {
		return nil, "", 0, 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Herald/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", 0, 0, fmt.Errorf("HTTP %d for %s", resp.StatusCode, imgURL)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("read image: %w", err)
	}
	if len(data) > maxImageBytes {
		return nil, "", 0, 0, fmt.Errorf("image at %s exceeds %d byte limit", imgURL, maxImageBytes)
	}
	if len(data) == 0 {
		return nil, "", 0, 0, fmt.Errorf("empty image at %s", imgURL)
	}

	ct := normalizeMIME(resp.Header.Get("Content-Type"), data)

	switch ct {
	case "image/png":
		out, w, h, err := resizeIfNeeded(data, ct)
		if err == nil {
			return out, "image/png", w, h, nil
		}
		// Decode failed — store raw.
	case "image/jpeg":
		out, w, h, err := resizeIfNeeded(data, ct)
		if err == nil {
			return out, "image/jpeg", w, h, nil
		}
		// Decode failed — store raw.
	}

	// GIF, WebP, SVG, ICO, or decode failure: store raw.
	return data, ct, 0, 0, nil
}

// resizeIfNeeded decodes the image, resizes if any dimension exceeds maxImageDim,
// and re-encodes in the same format. Returns (data, width, height, error).
func resizeIfNeeded(data []byte, mimeType string) ([]byte, int, int, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, err
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	if w <= maxImageDim && h <= maxImageDim {
		// Already fits; re-encode to strip metadata and normalize.
		var buf bytes.Buffer
		if mimeType == "image/jpeg" {
			err = jpeg.Encode(&buf, src, &jpeg.Options{Quality: 85})
		} else {
			err = png.Encode(&buf, src)
		}
		if err != nil {
			return nil, 0, 0, err
		}
		return buf.Bytes(), w, h, nil
	}

	// Scale proportionally to fit within maxImageDim × maxImageDim.
	if w > h {
		h = h * maxImageDim / w
		w = maxImageDim
	} else {
		w = w * maxImageDim / h
		h = maxImageDim
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	drawNearest(dst, src, b)

	var buf bytes.Buffer
	if mimeType == "image/jpeg" {
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85})
	} else {
		err = png.Encode(&buf, dst)
	}
	if err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), w, h, nil
}

// drawNearest fills dst with src scaled via nearest-neighbour interpolation.
func drawNearest(dst *image.RGBA, src image.Image, srcBounds image.Rectangle) {
	dw := dst.Bounds().Dx()
	dh := dst.Bounds().Dy()
	sw := srcBounds.Dx()
	sh := srcBounds.Dy()
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			sx := x * sw / dw
			sy := y * sh / dh
			dst.Set(x, y, src.At(srcBounds.Min.X+sx, srcBounds.Min.Y+sy))
		}
	}
}

// normalizeMIME maps Content-Type values to canonical image MIME types,
// falling back to http.DetectContentType when the header is absent or generic.
func normalizeMIME(ct string, data []byte) string {
	ct = strings.ToLower(strings.SplitN(ct, ";", 2)[0])
	ct = strings.TrimSpace(ct)
	switch ct {
	case "image/png":
		return "image/png"
	case "image/jpeg", "image/jpg":
		return "image/jpeg"
	case "image/gif":
		return "image/gif"
	case "image/webp":
		return "image/webp"
	case "image/svg+xml":
		return "image/svg+xml"
	case "image/x-icon", "image/vnd.microsoft.icon":
		return "image/x-icon"
	}
	// Sniff from bytes.
	sniffed := http.DetectContentType(data)
	if strings.HasPrefix(sniffed, "image/") {
		return strings.SplitN(sniffed, ";", 2)[0]
	}
	if ct != "" && ct != "application/octet-stream" {
		return ct
	}
	return "application/octet-stream"
}

// nodeAttr returns the value of the named attribute on an HTML node.
func nodeAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// jpeg import used for image.Decode side-effect registration.
var _ = jpeg.Decode
