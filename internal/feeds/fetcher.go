package feeds

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/mmcdole/gofeed"
)

type Fetcher struct {
	parser *gofeed.Parser
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
		store:  store,
	}
}

// FetchFeed fetches and parses a single feed
func (f *Fetcher) FetchFeed(ctx context.Context, feedURL string) (*gofeed.Feed, error) {
	feed, err := f.parser.ParseURLWithContext(feedURL, ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch feed %s: %w", feedURL, err)
	}
	return feed, nil
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
		article := &storage.Article{
			FeedID:  feedID,
			GUID:    item.GUID,
			Title:   item.Title,
			URL:     item.Link,
			Summary: item.Description,
			Author:  item.Author.Name,
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
		defer cancel()

		parsedFeed, err := f.FetchFeed(feedCtx, feed.URL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to fetch feed %s: %v\n", feed.URL, err)
			continue
		}

		// Update feed metadata
		if parsedFeed.Title != "" && parsedFeed.Title != feed.Title {
			// Could update feed title here if needed
		}

		// Store articles
		stored, err := f.StoreArticles(feed.ID, parsedFeed)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: error storing articles from %s: %v\n", feed.URL, err)
		}
		totalArticles += stored

		// Update last fetched timestamp
		if err := f.store.UpdateFeedLastFetched(feed.ID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update last_fetched for %s: %v\n", feed.URL, err)
		}
	}

	return totalArticles, nil
}
