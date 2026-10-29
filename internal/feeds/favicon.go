package feeds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// maxFaviconBytes is the largest raw response we'll accept before giving up.
const maxFaviconBytes = 256 * 1024 // 256 KB

// maxFaviconDim is the maximum width or height of a stored favicon.
// Images larger than this are scaled down before storing (ICO is stored as-is).
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
			// Negative-cache the failure so a permanently-broken favicon isn't
			// re-fetched and re-logged every cycle (#112). The retry query gates
			// on the backoff window, so this log line now fires at most once per
			// window per feed instead of every 5 minutes.
			kind := classifyFaviconFailure(err)
			f.log().Warn("favicon fetch failed", "feed", feed.ID, "url", feed.URL, "kind", kind, "err", err)
			if rerr := f.store.RecordFaviconFailure(feed.ID, kind); rerr != nil {
				f.log().Error("recording the favicon failure failed", "feed", feed.ID, "err", rerr)
			}
			continue
		}
		if err := f.store.StoreFeedFavicon(feed.ID, data, mimeType); err != nil {
			f.log().Error("storing the favicon failed", "feed", feed.ID, "err", err)
			continue
		}
		// Clear any prior failure marker now that the favicon is cached.
		if err := f.store.ClearFaviconFailure(feed.ID); err != nil {
			f.log().Error("clearing the favicon failure failed", "feed", feed.ID, "err", err)
		}
		stored++
	}
	return stored, nil
}

// faviconHTTPError carries the HTTP status of a failed favicon fetch so the
// failure can be classified (permanent vs transient) without string-parsing.
type faviconHTTPError struct {
	status int
	url    string
}

func (e *faviconHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d for %s", e.status, e.url)
}

// errEmptyFavicon marks a 200 response with an empty body -- treated as a
// permanent failure (the server stably serves nothing).
var errEmptyFavicon = errors.New("empty favicon")

// classifyFaviconFailure labels a favicon fetch error "permanent" (won't change
// on retry: 404/403/410/451/401, an empty body, or a body that is not a
// supported image) or "transient" (5xx, timeouts, network/DNS errors, parse
// failures) so the negative cache can pick a backoff window.
func classifyFaviconFailure(err error) string {
	if errors.Is(err, errEmptyFavicon) || errors.Is(err, errUnsupportedImage) {
		return "permanent"
	}
	var he *faviconHTTPError
	if errors.As(err, &he) {
		switch he.status {
		case http.StatusNotFound, http.StatusForbidden, http.StatusGone,
			http.StatusUnavailableForLegalReasons, http.StatusUnauthorized:
			return "permanent"
		}
	}
	return "transient"
}

// fetchFavicon fetches the best favicon for the site at feedURL.
//
// Strategy:
//  1. Derive the site root from feedURL's scheme+host.
//  2. Fetch the site root HTML and look for <link rel="icon"> / <link rel="shortcut icon">.
//  3. If found, fetch that URL.
//  4. If not found or the fetch fails, fall back to scheme://host/favicon.ico.
//
// The returned data is a re-encoded raster scaled to fit maxFaviconDim, or a
// raw ICO (see safeFavicon).
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
	req.Header.Set("User-Agent", PageUserAgent)
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

// fetchAndNormalize fetches iconURL and returns (data, mimeType) in a form
// that is safe to serve from herald's origin (see safeFavicon). Anything that is
// not an ICO, PNG, JPEG, GIF, or WebP image is refused, whatever its
// Content-Type says.
func fetchAndNormalize(ctx context.Context, client *http.Client, iconURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, iconURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", PageUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", &faviconHTTPError{status: resp.StatusCode, url: iconURL}
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFaviconBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read favicon: %w", err)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("%w at %s", errEmptyFavicon, iconURL)
	}
	if len(data) > maxFaviconBytes {
		return nil, "", fmt.Errorf("favicon at %s exceeds %d byte limit", iconURL, maxFaviconBytes)
	}

	img, err := safeFavicon(data)
	if err != nil {
		return nil, "", fmt.Errorf("favicon at %s: %w", iconURL, err)
	}
	return img.Data, img.MimeType, nil
}
