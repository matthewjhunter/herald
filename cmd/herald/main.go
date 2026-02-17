package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/feeds"
	"github.com/matthewjhunter/herald/internal/output"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/spf13/cobra"
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
func processArticlesForUser(ctx context.Context, store *storage.Store, processor *ai.AIProcessor, formatter *output.Formatter, appCfg *storage.Config, userID int64) (int, error) {
	processed := 0
	updatedGroups := make(map[int64]bool)

	unscoredArticles, err := store.GetUnscoredArticlesForUser(userID, 100)
	if err != nil {
		return 0, fmt.Errorf("failed to get unscored articles for user %d: %w", userID, err)
	}

	for _, article := range unscoredArticles {
		content := article.Content
		if content == "" {
			content = article.Summary
		}

		// 1. Generate AI summary (cached per-user)
		existingSummary, err := store.GetArticleSummary(userID, article.ID)
		if err != nil {
			formatter.Warning("failed to check article summary: %v", err)
			continue
		}

		if existingSummary == nil {
			aiSummary, err := processor.SummarizeArticle(ctx, userID, article.Title, content)
			if err != nil {
				formatter.Warning("summarization failed for article %d: %v", article.ID, err)
				continue
			}
			if err := store.UpdateArticleAISummary(userID, article.ID, aiSummary); err != nil {
				formatter.Warning("failed to cache AI summary: %v", err)
			}
		}

		// 2. Security check
		secResult, err := processor.SecurityCheck(ctx, userID, article.Title, content)
		if err != nil {
			formatter.Warning("security check failed for article %d: %v", article.ID, err)
			continue
		}

		if !secResult.Safe || secResult.Score < appCfg.Thresholds.SecurityScore {
			secScore := secResult.Score
			interestScore := 0.0
			store.UpdateReadState(article.ID, false, &interestScore, &secScore)
			formatter.OutputProcessingStatus(article.ID, article.Title, interestScore, secScore, false)
			continue
		}

		// 3. Interest scoring
		curResult, err := processor.CurateArticle(ctx, userID, article.Title, content, appCfg.Preferences.Keywords)
		if err != nil {
			formatter.Warning("curation failed for article %d: %v", article.ID, err)
			continue
		}

		secScore := secResult.Score
		interestScore := curResult.InterestScore
		store.UpdateReadState(article.ID, false, &interestScore, &secScore)
		formatter.OutputProcessingStatus(article.ID, article.Title, interestScore, secScore, true)

		// 4. Find or create group
		userGroups, err := store.GetUserGroups(userID)
		if err != nil {
			formatter.Warning("failed to get user groups: %v", err)
			continue
		}

		relatedGroupIDs, err := processor.FindRelatedGroups(ctx, userID, article, userGroups, store)
		if err != nil {
			formatter.Warning("failed to find related groups: %v", err)
			relatedGroupIDs = nil
		}

		if len(relatedGroupIDs) > 0 {
			if err := store.AddArticleToGroup(relatedGroupIDs[0], article.ID); err != nil {
				formatter.Warning("failed to add article to group: %v", err)
			} else {
				updatedGroups[relatedGroupIDs[0]] = true
			}
		} else {
			topic := article.Title
			if len(topic) > 100 {
				topic = topic[:100]
			}
			newGroupID, err := store.CreateArticleGroup(userID, topic)
			if err != nil {
				formatter.Warning("failed to create group: %v", err)
				continue
			}
			if err := store.AddArticleToGroup(newGroupID, article.ID); err != nil {
				formatter.Warning("failed to add article to new group: %v", err)
			} else {
				updatedGroups[newGroupID] = true
			}
		}

		processed++
	}

	// 5. Update group summaries for changed groups
	for groupID := range updatedGroups {
		if err := updateGroupSummary(ctx, store, processor, groupID, userID); err != nil {
			formatter.Warning("failed to update group summary for group %d: %v", groupID, err)
		}
	}

	return processed, nil
}

// updateGroupSummary regenerates the summary for a group
func updateGroupSummary(ctx context.Context, store *storage.Store, processor *ai.AIProcessor, groupID, userID int64) error {
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
		var score float64 = 5.0 // default

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
	return store.UpdateGroupSummary(groupID, groupSummary, len(articles), maxScorePtr)
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

	rootCmd.AddCommand(importCmd())
	rootCmd.AddCommand(fetchFeedsCmd())
	rootCmd.AddCommand(processCmd())
	rootCmd.AddCommand(fetchCmd())
	rootCmd.AddCommand(listCmd())
	rootCmd.AddCommand(readCmd())
	rootCmd.AddCommand(initConfigCmd())

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

func importCmd() *cobra.Command {
	var userID int64
	cmd := &cobra.Command{
		Use:   "import <opml-file>",
		Short: "Import feeds from an OPML file and subscribe user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
	cmd.Flags().Int64VarP(&userID, "user", "u", 1, "user ID to subscribe to feeds (default: 1)")
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
			highInterestArticles, scores, err := store.GetArticlesByInterestScore(cfg.Thresholds.InterestScore, 10, 0)
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
	cmd.Flags().Int64VarP(&userID, "user", "u", 1, "user ID to process articles for")
	return cmd
}

func fetchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fetch",
		Short: "Fetch all feeds and process articles with AI",
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

			// Fetch each feed once (efficient)
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
			highInterestArticles, scores, err := store.GetArticlesByInterestScore(cfg.Thresholds.InterestScore, 10, 0)
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
	return &cobra.Command{
		Use:   "read <article-id>",
		Short: "Mark an article as read",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var articleID int64
			if _, err := fmt.Sscanf(args[0], "%d", &articleID); err != nil {
				return fmt.Errorf("invalid article ID: %w", err)
			}

			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer store.Close()

			if err := store.UpdateReadState(articleID, true, nil, nil); err != nil {
				return fmt.Errorf("failed to mark article as read: %w", err)
			}

			fmt.Printf("Marked article %d as read\n", articleID)
			return nil
		},
	}
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
