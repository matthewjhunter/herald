package feeds

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/mmcdole/gofeed"
)

// sanitizeText strips null bytes and invalid UTF-8 sequences from feed/web
// content. PostgreSQL rejects text containing 0x00 even though it is a valid
// UTF-8 byte; SQLite accepts it silently, so the problem only surfaces on PG.
func sanitizeText(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.ReplaceAll(s, "\x00", "")
}

type Fetcher struct {
	parser *gofeed.Parser
	client *http.Client
	store  storage.Store
}

// OPML structures for parsing
type OPML struct {
	XMLName xml.Name `xml:"opml"`
	Body    OPMLBody `xml:"body"`
}

type OPMLBody struct {
	Outlines []OPMLOutline `xml:"outline"`
}

type OPMLOutline struct {
	Text     string        `xml:"text,attr"`
	Title    string        `xml:"title,attr"`
	Type     string        `xml:"type,attr"`
	XMLURL   string        `xml:"xmlUrl,attr"`
	HTMLURL  string        `xml:"htmlUrl,attr"`
	Outlines []OPMLOutline `xml:"outline"`
}

// NewFetcher creates a new feed fetcher
func NewFetcher(store storage.Store) *Fetcher {
	parser := gofeed.NewParser()
	parser.UserAgent = FeedUserAgent
	return &Fetcher{
		parser: parser,
		client: &http.Client{},
		store:  store,
	}
}

// FetchResult holds the outcome of a conditional feed fetch.
type FetchResult struct {
	Feed         *gofeed.Feed // nil when NotModified is true
	ETag         string       // ETag from response (empty if absent)
	LastModified string       // Last-Modified from response (empty if absent)
	NotModified  bool         // true when server returned 304
}

// FetchFeed fetches and parses a single feed using conditional HTTP requests.
// If the feed has stored ETag or Last-Modified values, they are sent as
// If-None-Match / If-Modified-Since headers. A 304 response skips parsing
// entirely and returns NotModified=true.
func (f *Fetcher) FetchFeed(ctx context.Context, feed storage.Feed) (*FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feed.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for %s: %w", feed.URL, err)
	}
	req.Header.Set("User-Agent", FeedUserAgent)
	if feed.ETag != "" {
		req.Header.Set("If-None-Match", feed.ETag)
	}
	if feed.LastModified != "" {
		req.Header.Set("If-Modified-Since", feed.LastModified)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch feed %s: %w", feed.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return &FetchResult{NotModified: true}, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("feed %s returned status %d", feed.URL, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read feed %s: %w", feed.URL, err)
	}

	parsed, err := f.parser.ParseString(string(body))
	if err != nil {
		return nil, fmt.Errorf("failed to parse feed %s: %w", feed.URL, err)
	}

	return &FetchResult{
		Feed:         parsed,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}, nil
}

// ImportOPMLReader imports feeds from an OPML reader and subscribes user to them.
func (f *Fetcher) ImportOPMLReader(r io.Reader, userID int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("failed to read OPML: %w", err)
	}
	return f.importOPMLBytes(data, userID)
}

// ImportOPML imports feeds from an OPML file and subscribes user to them
func (f *Fetcher) ImportOPML(opmlPath string, userID int64) error {
	data, err := os.ReadFile(opmlPath)
	if err != nil {
		return fmt.Errorf("failed to read OPML file: %w", err)
	}
	return f.importOPMLBytes(data, userID)
}

func (f *Fetcher) importOPMLBytes(data []byte, userID int64) error {
	var opml OPML
	if err := xml.Unmarshal(data, &opml); err != nil {
		return fmt.Errorf("failed to parse OPML: %w", err)
	}

	// Process outlines recursively
	added := 0
	var processOutlines func(outlines []OPMLOutline)
	processOutlines = func(outlines []OPMLOutline) {
		for _, outline := range outlines {
			// If this outline has a feed URL, add it
			if outline.XMLURL != "" {
				title := outline.Title
				if title == "" {
					title = outline.Text
				}
				if title == "" {
					title = outline.XMLURL
				}

				feedID, err := f.store.AddFeed(outline.XMLURL, title, "")
				if err != nil {
					// Feed might already exist, try to get it
					feeds, err2 := f.store.GetAllFeeds()
					if err2 == nil {
						for _, existingFeed := range feeds {
							if existingFeed.URL == outline.XMLURL {
								feedID = existingFeed.ID
								break
							}
						}
					}
					if feedID == 0 {
						fmt.Fprintf(os.Stderr, "Warning: failed to add feed %s: %v\n", outline.XMLURL, err)
						continue
					}
				}

				// Subscribe user to this feed
				if err := f.store.SubscribeUserToFeed(userID, feedID); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to subscribe to feed %s: %v\n", outline.XMLURL, err)
				} else {
					added++
				}
			}

			// Process nested outlines (folders)
			if len(outline.Outlines) > 0 {
				processOutlines(outline.Outlines)
			}
		}
	}

	processOutlines(opml.Body.Outlines)
	fmt.Printf("Added %d feeds from OPML\n", added)
	return nil
}

// StoreArticles stores articles from a feed into the database
func (f *Fetcher) StoreArticles(feedID int64, feed *gofeed.Feed) (int, error) {
	stored := 0
	for _, item := range feed.Items {
		var author string
		if item.Author != nil {
			author = item.Author.Name
		}
		article := &storage.Article{
			FeedID:  feedID,
			GUID:    item.GUID,
			Title:   sanitizeText(item.Title),
			URL:     item.Link,
			Summary: sanitizeText(item.Description),
			Author:  sanitizeText(author),
		}

		// Use content if available, otherwise use description
		if item.Content != "" {
			article.Content = sanitizeText(item.Content)
		} else {
			article.Content = sanitizeText(item.Description)
		}

		// YouTube (and other media feeds) store content in <media:group> extensions.
		// If we still have no content, synthesize HTML from the media group.
		if article.Content == "" {
			if html := mediaGroupHTML(item); html != "" {
				article.Content = html
			}
		}

		// Parse published date
		if item.PublishedParsed != nil {
			article.PublishedDate = item.PublishedParsed
		} else if item.UpdatedParsed != nil {
			article.PublishedDate = item.UpdatedParsed
		}

		// Skip cross-posted duplicates: same title + published date from a different feed
		if dupeID, err := f.store.FindDuplicateArticle(article.Title, article.PublishedDate); err == nil && dupeID > 0 {
			continue
		}

		// Store article (ignore duplicates)
		articleID, err := f.store.AddArticle(article)
		if err == nil && articleID > 0 {
			stored++

			// Store authors from gofeed (plural, non-deprecated)
			if len(item.Authors) > 0 {
				var authors []storage.ArticleAuthor
				for _, a := range item.Authors {
					if a.Name != "" {
						authors = append(authors, storage.ArticleAuthor{Name: a.Name, Email: a.Email})
					}
				}
				if len(authors) > 0 {
					f.store.StoreArticleAuthors(articleID, authors)
				}
			} else if item.Author != nil && item.Author.Name != "" {
				// Fallback to deprecated singular Author
				f.store.StoreArticleAuthors(articleID, []storage.ArticleAuthor{
					{Name: item.Author.Name, Email: item.Author.Email},
				})
			}

			// Store categories
			if len(item.Categories) > 0 {
				f.store.StoreArticleCategories(articleID, item.Categories)
			}
		}
	}

	return stored, nil
}

// mediaGroupHTML extracts a description and thumbnail from a <media:group>
// extension element (used by YouTube Atom feeds) and returns a simple HTML
// snippet, or "" if nothing useful is found.
func mediaGroupHTML(item *gofeed.Item) string {
	media, ok := item.Extensions["media"]
	if !ok {
		return ""
	}

	var thumb, desc string

	// Direct media:thumbnail / media:description at item level
	if thumbs, ok := media["thumbnail"]; ok && len(thumbs) > 0 {
		thumb = thumbs[0].Attrs["url"]
	}
	if descs, ok := media["description"]; ok && len(descs) > 0 {
		desc = descs[0].Value
	}

	// Nested inside media:group
	if groups, ok := media["group"]; ok && len(groups) > 0 {
		children := groups[0].Children
		if thumb == "" {
			if thumbs, ok := children["thumbnail"]; ok && len(thumbs) > 0 {
				thumb = thumbs[0].Attrs["url"]
			}
		}
		if desc == "" {
			if descs, ok := children["description"]; ok && len(descs) > 0 {
				desc = descs[0].Value
			}
		}
	}

	if thumb == "" && desc == "" {
		return ""
	}

	var html string
	if thumb != "" {
		html += `<p><a href="` + item.Link + `"><img src="` + thumb + `" alt="video thumbnail"></a></p>`
	}
	if desc != "" {
		html += "<p>" + desc + "</p>"
	}
	return html
}

// FetchStats summarizes a feed polling cycle at the fetcher level.
type FetchStats struct {
	FeedsTotal       int // total feeds attempted
	FeedsDownloaded  int // feeds that returned new content (HTTP 200)
	FeedsNotModified int // feeds that returned 304
	FeedsErrored     int // feeds that failed
	NewArticles      int // articles newly written to DB
}

// FetchAllFeeds fetches all enabled feeds and stores their articles
func (f *Fetcher) FetchAllFeeds(ctx context.Context) (*FetchStats, error) {
	feeds, err := f.store.GetAllFeeds()
	if err != nil {
		return nil, fmt.Errorf("failed to get feeds: %w", err)
	}

	stats := &FetchStats{FeedsTotal: len(feeds)}
	for _, feed := range feeds {
		// Add timeout per feed
		feedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		result, err := f.FetchFeed(feedCtx, feed)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to fetch feed %s: %v\n", feed.URL, err)
			f.store.UpdateFeedError(feed.ID, err.Error())
			stats.FeedsErrored++
			continue
		}

		if result.NotModified {
			stats.FeedsNotModified++
			// Clear any previous error and update last_fetched
			if err := f.store.ClearFeedError(feed.ID); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to update last_fetched for %s: %v\n", feed.URL, err)
			}
			continue
		}

		stats.FeedsDownloaded++

		// Store articles
		stored, err := f.StoreArticles(feed.ID, result.Feed)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: error storing articles from %s: %v\n", feed.URL, err)
		}
		stats.NewArticles += stored

		// Persist cache headers for next conditional request
		if result.ETag != "" || result.LastModified != "" {
			f.store.UpdateFeedCacheHeaders(feed.ID, result.ETag, result.LastModified)
		}

		// Store blog homepage URL from feed metadata
		if result.Feed.Link != "" && result.Feed.Link != feed.SiteURL {
			f.store.UpdateFeedSiteURL(feed.ID, result.Feed.Link)
		}

		// Clear any previous error and update last_fetched
		if err := f.store.ClearFeedError(feed.ID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update last_fetched for %s: %v\n", feed.URL, err)
		}
	}

	return stats, nil
}
