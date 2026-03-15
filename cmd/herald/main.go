package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/feeds"
	"github.com/matthewjhunter/herald/internal/output"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"
)

var (
	configPath   string
	cfg          *storage.Config
	outputFormat string
)

// processArticlesForUser runs the AI pipeline (summarize, security check,
// interest scoring, grouping) for a single user's unscored articles. Returns
// the number of articles processed. This is the shared core used by both
// the `process` and `fetch` commands.
//
// Only articles with no read_state entry are processed — once scored, an
// article is never re-scored. This avoids redundant AI calls on articles
// that were fetched in a previous cycle but haven't been read yet.
//
// Up to appCfg.Ollama.MaxParallel articles are processed concurrently.
// Within each article, summarization and security check run in parallel
// since they are independent; curation and group matching run after.
// Articles are processed in batches of 100 until the queue is empty.
// Group summary updates are deferred until all batches complete.
func processArticlesForUser(ctx context.Context, store storage.Store, processor *ai.AIProcessor, formatter *output.Formatter, appCfg *storage.Config, userID int64) (int, error) {
	embedder := embedding.NewOpenAIEmbedder(appCfg.Ollama.BaseURL, appCfg.Ollama.APIKey, appCfg.Ollama.EmbeddingModel)
	groupMatcher := ai.NewGroupMatcher(embedder, store, appCfg.Grouping.SimilarityThreshold)

	maxParallel := appCfg.Ollama.MaxParallel
	if maxParallel < 1 {
		maxParallel = 1
	}

	var (
		mu            sync.Mutex
		processed     int
		updatedGroups = make(map[int64]bool)
	)

	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	for ctx.Err() == nil { //nolint:staticcheck // QF1006: batch-fetch-then-check pattern is intentional
		unscoredArticles, err := store.GetUnscoredArticlesForUser(userID, 100)
		if err != nil {
			return processed, fmt.Errorf("failed to get unscored articles for user %d: %w", userID, err)
		}
		if len(unscoredArticles) == 0 {
			break
		}

		for _, article := range unscoredArticles {
			if ctx.Err() != nil {
				break
			}

			sem <- struct{}{}
			wg.Add(1)

			go func(article storage.Article) {
				defer func() { <-sem; wg.Done() }()

				content := article.Content
				if content == "" {
					content = article.Summary
				}
				if content == "" {
					formatter.Warning("skipping article %d %q: no content", article.ID, article.Title)
					// Mark as scored so it doesn't block the queue forever.
					zeroInterest := 0.0
					zeroSec := 0.0
					reason := "no content"
					store.UpdateReadState(userID, article.ID, false, &zeroInterest, &zeroSec, &reason) //nolint:errcheck
					return
				}
				if article.LinkedContent != "" {
					content = content + "\n\n" + article.LinkedContent
				}

				// 1+2. Summarize and security check concurrently — they are independent.
				var (
					aiSummary string
					secResult *ai.SecurityResult
					secErr    error
				)
				g, gctx := errgroup.WithContext(ctx)

				g.Go(func() error {
					existing, err := store.GetArticleSummary(userID, article.ID)
					if err != nil {
						formatter.Warning("failed to check article summary for %d: %v", article.ID, err)
						return nil // non-fatal
					}
					if existing != nil {
						aiSummary = existing.AISummary
						return nil
					}
					aiSummary, err = processor.SummarizeArticle(gctx, userID, article.Title, content, cfg.Summarization.MaxSummaryLength)
					if err != nil {
						formatter.Warning("summarization failed for article %d: %v", article.ID, err)
						return nil // non-fatal: scoring can proceed without summary
					}
					if err := store.UpdateArticleAISummary(userID, article.ID, aiSummary); err != nil {
						formatter.Warning("failed to cache AI summary for %d: %v", article.ID, err)
					}
					return nil
				})

				g.Go(func() error {
					secResult, secErr = processor.SecurityCheck(gctx, userID, article.Title, content)
					return nil
				})

				g.Wait() //nolint:errcheck

				if secErr != nil {
					formatter.Warning("security check failed for article %d: %v", article.ID, secErr)
					return
				}

				if !secResult.Safe || secResult.Score < appCfg.Thresholds.SecurityScore {
					secScore := secResult.Score
					interestScore := 0.0
					store.UpdateReadState(userID, article.ID, false, &interestScore, &secScore, &secResult.Reasoning) //nolint:errcheck
					formatter.OutputProcessingStatus(article.ID, article.Title, interestScore, secScore, false)
					return
				}

				// 3. Interest scoring
				curResult, err := processor.CurateArticle(ctx, userID, article.Title, content, appCfg.Preferences.Keywords)
				if err != nil {
					formatter.Warning("curation failed for article %d: %v", article.ID, err)
					return
				}

				secScore := secResult.Score
				interestScore := curResult.InterestScore
				store.UpdateReadState(userID, article.ID, false, &interestScore, &secScore, &secResult.Reasoning) //nolint:errcheck
				formatter.OutputProcessingStatus(article.ID, article.Title, interestScore, secScore, true)

				// 4. Vector-based group matching
				matchedGroupID, articleEmb, err := groupMatcher.MatchArticleToGroup(ctx, userID, article.Title, aiSummary)
				if err != nil {
					formatter.Warning("vector group match failed: %v", err)
				}

				mu.Lock()
				defer mu.Unlock()

				processed++
				if matchedGroupID != nil {
					if err := store.AddArticleToGroup(*matchedGroupID, article.ID); err != nil {
						formatter.Warning("failed to add article to group: %v", err)
					} else {
						updatedGroups[*matchedGroupID] = true
						if articleEmb != nil {
							if err := groupMatcher.UpdateGroupCentroid(ctx, *matchedGroupID, articleEmb); err != nil {
								formatter.Warning("failed to update group centroid: %v", err)
							}
						}
					}
				} else {
					topic := article.Title
					if len(topic) > 100 {
						topic = topic[:100]
					}
					newGroupID, err := store.CreateArticleGroup(userID, topic)
					if err != nil {
						formatter.Warning("failed to create group: %v", err)
						return
					}
					if err := store.AddArticleToGroup(newGroupID, article.ID); err != nil {
						formatter.Warning("failed to add article to new group: %v", err)
					} else {
						updatedGroups[newGroupID] = true
					}
					if articleEmb != nil {
						if err := store.UpdateGroupEmbedding(newGroupID, embedding.EncodeFloat32s(articleEmb)); err != nil {
							formatter.Warning("failed to set initial group centroid: %v", err)
						}
					}
				}
			}(article)
		}

		wg.Wait()
	}

	// 5. Update group summaries for changed groups — sequential, after all batches.
	for groupID := range updatedGroups {
		if err := updateGroupSummary(ctx, store, processor, groupID, userID); err != nil {
			formatter.Warning("failed to update group summary for group %d: %v", groupID, err)
		}
	}

	return processed, nil
}

// updateGroupSummary regenerates the summary for a group
func updateGroupSummary(ctx context.Context, store storage.Store, processor *ai.AIProcessor, groupID, userID int64) error {
	// Get all articles in the group
	articles, err := store.GetGroupArticles(groupID)
	if err != nil {
		return fmt.Errorf("failed to get group articles: %w", err)
	}

	if len(articles) == 0 {
		return nil
	}

	// Get first article group to get topic
	userGroups, err := store.GetUserGroups(userID)
	if err != nil {
		return err
	}

	var topic string
	for _, g := range userGroups {
		if g.ID == groupID {
			topic = g.Topic
			break
		}
	}

	// Build input for group summary
	var summaryInputs []ai.GroupSummaryInput
	var maxScore float64

	for _, article := range articles {
		// Get AI summary for this article
		summary, err := store.GetArticleSummary(userID, article.ID)
		if err != nil || summary == nil {
			continue
		}

		// Get interest score
		// For now, query from read_state
		// TODO: could add a method to get this more efficiently
		score := 5.0 // default

		summaryInputs = append(summaryInputs, ai.GroupSummaryInput{
			Title:     article.Title,
			AISummary: summary.AISummary,
			Score:     score,
		})

		if score > maxScore {
			maxScore = score
		}
	}

	if len(summaryInputs) == 0 {
		return nil
	}

	// Generate group summary
	groupSummary, err := processor.GenerateGroupSummary(ctx, userID, topic, summaryInputs)
	if err != nil {
		return fmt.Errorf("failed to generate group summary: %w", err)
	}

	// Store group summary
	maxScorePtr := &maxScore
	if err := store.UpdateGroupSummary(groupID, groupSummary, len(articles), maxScorePtr); err != nil {
		return err
	}

	// Phase 6: Refine topic label when group has 3+ articles.
	// Use the LLM to generate a concise topic from the group summary.
	if len(articles) >= 3 {
		refinedTopic, err := processor.RefineGroupTopic(ctx, userID, groupSummary)
		if err == nil && refinedTopic != "" {
			store.UpdateGroupTopic(groupID, refinedTopic)
		}
	}

	return nil
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "herald",
		Short: "Your AI-powered news herald - intelligent RSS/Atom feed reader with AI curation",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return loadConfig()
		},
	}

	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "config file path (default: ./config/config.yaml)")
	rootCmd.PersistentFlags().StringVarP(&outputFormat, "format", "f", "json", "output format: json, text, human (default: json)")

	rootCmd.AddCommand(createUserCmd())
	rootCmd.AddCommand(importCmd())
	rootCmd.AddCommand(fetchFeedsCmd())
	rootCmd.AddCommand(processCmd())
	rootCmd.AddCommand(fetchCmd())
	rootCmd.AddCommand(daemonCmd())
	rootCmd.AddCommand(listCmd())
	rootCmd.AddCommand(readCmd())
	rootCmd.AddCommand(initConfigCmd())
	rootCmd.AddCommand(migrateDBCmd())
	rootCmd.AddCommand(resetScoresCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func loadConfig() error {
	if configPath == "" {
		configPath = "./config/config.yaml"
	}

	// Check if config exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Use default config
		cfg = storage.DefaultConfig()
		return nil
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	cfg = storage.DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	return nil
}

func createUserCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create-user <name>",
		Short: "Create a new user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer store.Close()

			id, err := store.CreateUser(args[0])
			if err != nil {
				return fmt.Errorf("failed to create user: %w", err)
			}

			fmt.Printf("Created user %q with ID %d\n", args[0], id)
			return nil
		},
	}
}

func importCmd() *cobra.Command {
	var userID int64
	cmd := &cobra.Command{
		Use:   "import <opml-file>",
		Short: "Import feeds from an OPML file and subscribe user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("user") {
				userID = cfg.DefaultUserID
			}
			opmlPath := args[0]

			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer store.Close()

			fetcher := feeds.NewFetcher(store)
			if err := fetcher.ImportOPML(opmlPath, userID); err != nil {
				return fmt.Errorf("failed to import OPML: %w", err)
			}

			fmt.Printf("Successfully imported and subscribed user %d to feeds from %s\n", userID, opmlPath)
			return nil
		},
	}
	cmd.Flags().Int64VarP(&userID, "user", "u", 0, "user ID to subscribe to feeds")
	return cmd
}

func fetchFeedsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fetch-feeds",
		Short: "Fetch all subscribed feeds and store articles (no AI processing)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			formatter := output.NewFormatter(output.Format(outputFormat))

			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer store.Close()

			// Get all feeds that ANY user is subscribed to
			subscribedFeeds, err := store.GetAllSubscribedFeeds()
			if err != nil {
				return fmt.Errorf("failed to get subscribed feeds: %w", err)
			}

			if len(subscribedFeeds) == 0 {
				formatter.Warning("no feeds subscribed by any user")
				return formatter.OutputFetchResult(&output.FetchResult{})
			}

			// Fetch each feed once (efficient, no AI processing)
			fetcher := feeds.NewFetcher(store)
			fetchResult := &output.FetchResult{FeedsTotal: len(subscribedFeeds)}
			for _, feed := range subscribedFeeds {
				feedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				result, err := fetcher.FetchFeed(feedCtx, feed)
				cancel()

				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to fetch feed %s: %v\n", feed.URL, err)
					fetchResult.FeedsErrored++
					continue
				}

				if result.NotModified {
					fetchResult.FeedsNotModified++
					store.UpdateFeedLastFetched(feed.ID)
					continue
				}

				fetchResult.FeedsDownloaded++

				// Store articles (global, fetched once)
				stored, err := fetcher.StoreArticles(feed.ID, result.Feed)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: error storing articles from %s: %v\n", feed.URL, err)
				}
				fetchResult.NewArticles += stored

				// Persist cache headers for next conditional request
				if result.ETag != "" || result.LastModified != "" {
					store.UpdateFeedCacheHeaders(feed.ID, result.ETag, result.LastModified)
				}

				// Update last fetched timestamp
				if err := store.UpdateFeedLastFetched(feed.ID); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to update last_fetched for %s: %v\n", feed.URL, err)
				}
			}

			return formatter.OutputFetchResult(fetchResult)
		},
	}
}

func processCmd() *cobra.Command {
	var userID int64
	cmd := &cobra.Command{
		Use:   "process",
		Short: "Process articles with AI for a specific user",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("user") {
				userID = cfg.DefaultUserID
			}
			ctx := context.Background()
			formatter := output.NewFormatter(output.Format(outputFormat))

			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer store.Close()

			// Process articles with AI
			processor, err := ai.NewAIProcessor(cfg.Ollama.BaseURL, cfg.Ollama.SecurityModel, cfg.Ollama.CurationModel, store, cfg)
			if err != nil {
				formatter.Warning("failed to create AI processor: %v", err)
				formatter.Warning("skipping AI processing (Ollama may not be running)")
				return nil
			}

			processed, err := processArticlesForUser(ctx, store, processor, formatter, cfg, userID)
			if err != nil {
				return err
			}

			result := &output.FetchResult{
				ProcessedCount: processed,
			}

			// Get and output high-interest articles
			highInterestArticles, scores, err := store.GetArticlesByInterestScore(userID, cfg.Thresholds.InterestScore, 10, 0, nil)
			if err != nil {
				return fmt.Errorf("failed to get high-interest articles: %w", err)
			}

			result.HighInterest = len(highInterestArticles)
			result.NewArticles = processed

			// Use Majordomo format for JSON output, traditional format for others
			if outputFormat == "json" {
				// Output in Majordomo CommandOutput format
				return formatter.OutputMajordomoResult(result, userID, highInterestArticles)
			}

			// Output result summary (text/human formats)
			if err := formatter.OutputFetchResult(result); err != nil {
				return err
			}

			// Output high-interest notifications (text/human formats)
			if len(highInterestArticles) > 0 {
				if err := formatter.OutputHighInterestNotification(highInterestArticles, scores); err != nil {
					formatter.Warning("notification output failed: %v", err)
				}
			}

			return nil
		},
	}
	cmd.Flags().Int64VarP(&userID, "user", "u", 0, "user ID to process articles for")
	return cmd
}

// doFetch runs the complete fetch+process cycle once. Both the `fetch` command
// and the `daemon` command call this. It uses the package-level cfg and
// outputFormat variables.
func doFetch(ctx context.Context) error {
	formatter := output.NewFormatter(output.Format(outputFormat))

	store, err := storage.NewStore(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer store.Close()

	// Get all feeds that ANY user is subscribed to
	subscribedFeeds, err := store.GetAllSubscribedFeeds()
	if err != nil {
		return fmt.Errorf("failed to get subscribed feeds: %w", err)
	}

	if len(subscribedFeeds) == 0 {
		formatter.Warning("no feeds subscribed by any user")
		return formatter.OutputFetchResult(&output.FetchResult{})
	}

	// Fetch each feed once (efficient)
	fetcher := feeds.NewFetcher(store)
	fetchResult := &output.FetchResult{FeedsTotal: len(subscribedFeeds)}
	for _, feed := range subscribedFeeds {
		feedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		result, err := fetcher.FetchFeed(feedCtx, feed)
		cancel()

		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to fetch feed %s: %v\n", feed.URL, err)
			store.UpdateFeedError(feed.ID, err.Error()) //nolint:errcheck
			fetchResult.FeedsErrored++
			continue
		}

		if result.NotModified {
			fetchResult.FeedsNotModified++
			store.UpdateFeedLastFetched(feed.ID)
			continue
		}

		fetchResult.FeedsDownloaded++

		// Store articles (global, fetched once)
		stored, err := fetcher.StoreArticles(feed.ID, result.Feed)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: error storing articles from %s: %v\n", feed.URL, err)
		}
		fetchResult.NewArticles += stored

		// Persist cache headers for next conditional request
		if result.ETag != "" || result.LastModified != "" {
			store.UpdateFeedCacheHeaders(feed.ID, result.ETag, result.LastModified)
		}

		// Update last fetched timestamp
		if err := store.UpdateFeedLastFetched(feed.ID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update last_fetched for %s: %v\n", feed.URL, err)
		}
	}

	// Fetch full text for any articles whose feed content appears truncated.
	// This runs after all feeds are stored so the AI pipeline gets the best content.
	if fullTextUpdated, err := fetcher.FetchFullTextForArticles(ctx); err != nil {
		formatter.Warning("full-text fetch error: %v", err)
	} else if fullTextUpdated > 0 {
		fmt.Fprintf(os.Stdout, "Updated full text for %d articles\n", fullTextUpdated)
	}

	// Cache images referenced in article content.
	if imagesStored, err := fetcher.CacheArticleImages(ctx); err != nil {
		formatter.Warning("image cache error: %v", err)
	} else if imagesStored > 0 {
		fmt.Fprintf(os.Stdout, "Cached %d article images\n", imagesStored)
	}

	// Fetch and cache favicons for any newly-subscribed feeds.
	if faviconStored, err := fetcher.FetchFaviconsForFeeds(ctx); err != nil {
		formatter.Warning("favicon fetch error: %v", err)
	} else if faviconStored > 0 {
		fmt.Fprintf(os.Stdout, "Cached favicons for %d feeds\n", faviconStored)
	}

	if fetchResult.NewArticles == 0 {
		return formatter.OutputFetchResult(fetchResult)
	}

	// Process unread articles with AI
	processor, err := ai.NewAIProcessor(cfg.Ollama.BaseURL, cfg.Ollama.SecurityModel, cfg.Ollama.CurationModel, store, cfg)
	if err != nil {
		formatter.Warning("failed to create AI processor: %v", err)
		formatter.Warning("skipping AI processing (Ollama may not be running)")
		return formatter.OutputFetchResult(fetchResult)
	}

	// Get all users who have subscriptions
	allUserIDs, err := store.GetAllSubscribingUsers()
	if err != nil {
		return fmt.Errorf("failed to get subscribing users: %w", err)
	}

	if len(allUserIDs) == 0 {
		formatter.Warning("no users with subscriptions")
		return formatter.OutputFetchResult(fetchResult)
	}

	totalProcessed := 0

	// Process articles for each subscribing user
	for _, userID := range allUserIDs {
		processed, err := processArticlesForUser(ctx, store, processor, formatter, cfg, userID)
		if err != nil {
			formatter.Warning("failed to process articles for user %d: %v", userID, err)
			continue
		}
		totalProcessed += processed
	}

	fetchResult.ProcessedCount = totalProcessed

	// Get and output high-interest articles
	// Show high-interest articles for the first subscribing user.
	var displayUserID int64 = 1
	if len(allUserIDs) > 0 {
		displayUserID = allUserIDs[0]
	}
	highInterestArticles, scores, err := store.GetArticlesByInterestScore(displayUserID, cfg.Thresholds.InterestScore, 10, 0, nil)
	if err != nil {
		return fmt.Errorf("failed to get high-interest articles: %w", err)
	}

	fetchResult.HighInterest = len(highInterestArticles)

	// Output result summary
	if err := formatter.OutputFetchResult(fetchResult); err != nil {
		return err
	}

	// Output high-interest notifications
	if len(highInterestArticles) > 0 {
		if err := formatter.OutputHighInterestNotification(highInterestArticles, scores); err != nil {
			formatter.Warning("notification output failed: %v", err)
		}
	}

	return nil
}

func fetchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fetch",
		Short: "Fetch all feeds and process articles with AI",
		RunE: func(cmd *cobra.Command, args []string) error {
			return doFetch(context.Background())
		},
	}
}

func listCmd() *cobra.Command {
	var limit int
	var cluster bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List unread articles",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			formatter := output.NewFormatter(output.Format(outputFormat))

			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer store.Close()

			articles, err := store.GetUnreadArticles(limit)
			if err != nil {
				return fmt.Errorf("failed to get articles: %w", err)
			}

			if !cluster {
				// Simple list output
				return formatter.OutputArticleList(articles)
			}

			// Clustering mode - get scores and cluster articles
			var scores []float64
			for range articles {
				// Try to get interest score from read_state
				// For now, use 0 if no score exists
				scores = append(scores, 0.0)
			}

			// Create AI processor for clustering
			processor, err := ai.NewAIProcessor(cfg.Ollama.BaseURL, cfg.Ollama.SecurityModel, cfg.Ollama.CurationModel, store, cfg)
			if err != nil {
				formatter.Warning("clustering requires Ollama: %v", err)
				// Fall back to simple list
				return formatter.OutputArticleList(articles)
			}

			groups, err := processor.ClusterArticles(ctx, articles, scores)
			if err != nil {
				formatter.Warning("clustering failed: %v", err)
				// Fall back to simple list
				return formatter.OutputArticleList(articles)
			}

			return formatter.OutputArticleGroups(groups)
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "maximum number of articles to show")
	cmd.Flags().BoolVarP(&cluster, "cluster", "g", false, "group articles by topic/event")
	return cmd
}

func readCmd() *cobra.Command {
	var userID int64
	cmd := &cobra.Command{
		Use:   "read <article-id>",
		Short: "Mark an article as read",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("user") {
				userID = cfg.DefaultUserID
			}
			var articleID int64
			if _, err := fmt.Sscanf(args[0], "%d", &articleID); err != nil {
				return fmt.Errorf("invalid article ID: %w", err)
			}

			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer store.Close()

			if err := store.UpdateReadState(userID, articleID, true, nil, nil, nil); err != nil {
				return fmt.Errorf("failed to mark article as read: %w", err)
			}

			fmt.Printf("Marked article %d as read\n", articleID)
			return nil
		},
	}
	cmd.Flags().Int64VarP(&userID, "user", "u", 0, "user ID")
	return cmd
}

func initConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init-config",
		Short: "Create a default config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				configPath = "./config/config.yaml"
			}

			// Create config directory
			dir := filepath.Dir(configPath)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create config directory: %w", err)
			}

			// Check if config already exists
			if _, err := os.Stat(configPath); err == nil {
				return fmt.Errorf("config file already exists: %s", configPath)
			}

			// Write default config
			cfg := storage.DefaultConfig()
			data, err := yaml.Marshal(cfg)
			if err != nil {
				return fmt.Errorf("failed to marshal config: %w", err)
			}

			if err := os.WriteFile(configPath, data, 0644); err != nil {
				return fmt.Errorf("failed to write config: %w", err)
			}

			fmt.Printf("Created default config at %s\n", configPath)
			return nil
		},
	}
}

func migrateDBCmd() *cobra.Command {
	var srcDSN, dstDSN string
	cmd := &cobra.Command{
		Use:   "migrate-db",
		Short: "Migrate data between storage backends (SQLite ↔ PostgreSQL)",
		Long: `Copies all data from a source store to a destination store.

Both --src and --dst accept either a SQLite file path or a postgres:// DSN.
The destination store must already exist (schema is applied automatically).

Example — SQLite to PostgreSQL:
  herald migrate-db --src ./herald.db --dst postgres://user:pass@localhost/herald

Example — PostgreSQL to SQLite:
  herald migrate-db --src postgres://user:pass@localhost/herald --dst ./herald.db`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if srcDSN == "" || dstDSN == "" {
				return fmt.Errorf("--src and --dst are required")
			}

			ctx := context.Background()

			src, err := storage.NewStore(srcDSN)
			if err != nil {
				return fmt.Errorf("failed to open source store: %w", err)
			}
			defer src.Close()

			dst, err := storage.NewStore(dstDSN)
			if err != nil {
				return fmt.Errorf("failed to open destination store: %w", err)
			}
			defer dst.Close()

			fmt.Println("Starting migration...")
			stats, err := storage.MigrateStore(ctx, src, dst)
			if err != nil {
				return fmt.Errorf("migration failed: %w", err)
			}

			fmt.Printf("Migration complete:\n")
			fmt.Printf("  Feeds:         %d\n", stats.Feeds)
			fmt.Printf("  Users:         %d\n", stats.Users)
			fmt.Printf("  Articles:      %d\n", stats.Articles)
			fmt.Printf("  Read states:   %d\n", stats.ReadStates)
			fmt.Printf("  Subscriptions: %d\n", stats.Subscriptions)
			fmt.Printf("  Preferences:   %d\n", stats.Preferences)
			fmt.Printf("  Prompts:       %d\n", stats.Prompts)
			fmt.Printf("  Groups:        %d\n", stats.Groups)
			fmt.Printf("  Filter rules:  %d\n", stats.FilterRules)
			fmt.Printf("  Fever creds:   %d\n", stats.FeverCreds)
			fmt.Printf("  Favicons:      %d\n", stats.Favicons)
			fmt.Printf("  Images:        %d\n", stats.Images)
			return nil
		},
	}
	cmd.Flags().StringVar(&srcDSN, "src", "", "source DSN: SQLite file path or postgres:// URL")
	cmd.Flags().StringVar(&dstDSN, "dst", "", "destination DSN: SQLite file path or postgres:// URL")
	return cmd
}

func resetScoresCmd() *cobra.Command {
	var userID int64
	var securityOnly bool
	var belowScore float64
	cmd := &cobra.Command{
		Use:   "reset-scores",
		Short: "Clear AI scores so articles are reprocessed by the pipeline",
		Long: `Resets AI scoring state so articles will be picked up on the next
process or fetch run. Useful after tuning prompts or thresholds.

By default resets all scored articles. Use --security-only to target only
articles that failed the security check. Use --below to further narrow to
articles with a security score below a given value.

Examples:
  # Reset all security failures (score < 7.0, the default threshold):
  herald reset-scores --security-only --below 7.0

  # Reset everything and rescore from scratch:
  herald reset-scores

  # Reset only the worst security failures:
  herald reset-scores --security-only --below 4.0`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("failed to open store: %w", err)
			}
			defer store.Close()

			n, err := store.ResetScores(userID, securityOnly, belowScore)
			if err != nil {
				return fmt.Errorf("reset-scores failed: %w", err)
			}
			if securityOnly {
				fmt.Printf("Reset %d articles with security score < %.1f (will be rescored on next run)\n", n, belowScore)
			} else {
				fmt.Printf("Reset %d articles (will be rescored on next run)\n", n)
			}
			return nil
		},
	}
	cmd.Flags().Int64Var(&userID, "user", 1, "user ID")
	cmd.Flags().BoolVar(&securityOnly, "security-only", false, "reset only articles that failed the security check")
	cmd.Flags().Float64Var(&belowScore, "below", 7.0, "reset security failures below this score (implies --security-only if used alone)")
	return cmd
}
