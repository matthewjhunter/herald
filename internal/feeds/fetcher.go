package feeds

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/mmcdole/gofeed"
)

type Fetcher struct {
	parser *gofeed.Parser
	client *http.Client
	store  *storage.Store
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
func NewFetcher(store *storage.Store) *Fetcher {
	parser := gofeed.NewParser()
	parser.UserAgent = "FeedReader/1.0"
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
	req.Header.Set("User-Agent", "FeedReader/1.0")
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

// ImportOPML imports feeds from an OPML file and subscribes user to them
func (f *Fetcher) ImportOPML(opmlPath string, userID int64) error {
	data, err := os.ReadFile(opmlPath)
	if err != nil {
		return fmt.Errorf("failed to read OPML file: %w", err)
	}

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
			Title:   item.Title,
			URL:     item.Link,
			Summary: item.Description,
			Author:  author,
		}

		// Use content if available, otherwise use description
		if item.Content != "" {
			article.Content = item.Content
		} else {
			article.Content = item.Description
		}

		// Parse published date
		if item.PublishedParsed != nil {
			article.PublishedDate = item.PublishedParsed
		} else if item.UpdatedParsed != nil {
			article.PublishedDate = item.UpdatedParsed
		}

		// Store article (ignore duplicates)
		articleID, err := f.store.AddArticle(article)
		if err == nil && articleID > 0 {
			stored++
		}
	}

	return stored, nil
}

// FetchAllFeeds fetches all enabled feeds and stores their articles
func (f *Fetcher) FetchAllFeeds(ctx context.Context) (int, error) {
	feeds, err := f.store.GetAllFeeds()
	if err != nil {
		return 0, fmt.Errorf("failed to get feeds: %w", err)
	}

	totalArticles := 0
	for _, feed := range feeds {
		// Add timeout per feed
		feedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		result, err := f.FetchFeed(feedCtx, feed)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to fetch feed %s: %v\n", feed.URL, err)
			f.store.UpdateFeedError(feed.ID, err.Error())
			continue
		}

		if result.NotModified {
			// Clear any previous error and update last_fetched
			if err := f.store.ClearFeedError(feed.ID); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to update last_fetched for %s: %v\n", feed.URL, err)
			}
			continue
		}

		// Store articles
		stored, err := f.StoreArticles(feed.ID, result.Feed)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: error storing articles from %s: %v\n", feed.URL, err)
		}
		totalArticles += stored

		// Persist cache headers for next conditional request
		if result.ETag != "" || result.LastModified != "" {
			f.store.UpdateFeedCacheHeaders(feed.ID, result.ETag, result.LastModified)
		}

		// Clear any previous error and update last_fetched
		if err := f.store.ClearFeedError(feed.ID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update last_fetched for %s: %v\n", feed.URL, err)
		}
	}

	return totalArticles, nil
}
