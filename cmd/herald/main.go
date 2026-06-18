package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	embedding "github.com/matthewjhunter/go-embedding"
	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/feeds"
	"github.com/matthewjhunter/herald/internal/output"
	"github.com/matthewjhunter/herald/internal/pipeline"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"
)

var (
	configPath   string
	cfg          *storage.Config
	outputFormat string
)

// newGroupMatcher constructs a GroupMatcher from HERALD_EMBED env vars and
// the Grouping config. Used by both the daemon cycle and the `process` CLI.
func newGroupMatcher() (*ai.GroupMatcher, error) {
	embCfg, err := embedding.ConfigFromEnvPrefix("HERALD_EMBED")
	if err != nil {
		return nil, fmt.Errorf("embedder config: %w", err)
	}
	embedder, err := embedding.New(embCfg)
	if err != nil {
		return nil, fmt.Errorf("create embedder: %w", err)
	}
	return ai.NewGroupMatcher(embedder, embCfg.Model), nil
}

// newPipelineStage builds the staged AI pipeline for one user, wiring the AI
// processor, the (possibly nil) embedder, and BuildArticleEmbedInput. The
// groupMatcher must only be assigned to the Embedder interface when non-nil —
// a typed nil pointer stored in an interface is not == nil, which would defeat
// the embed/cluster stages' "embedding unconfigured" guards.
func newPipelineStage(store storage.Store, processor *ai.AIProcessor, groupMatcher *ai.GroupMatcher, formatter *output.Formatter, userID int64) *pipeline.Stage {
	st := &pipeline.Stage{
		Store:     store,
		AI:        processor,
		Cfg:       cfg,
		Formatter: formatter,
		UserID:    userID,
		BuildEmbedInput: func(a storage.Article) ([]embedding.Field, string) {
			return herald.BuildArticleEmbedInput(store, a)
		},
	}
	if groupMatcher != nil {
		st.Embedder = groupMatcher
	}
	return st
}

// annotationSkipConfigLoad marks a subcommand that bootstraps its own
// config (e.g. init-config writes a fresh one). The persistent pre-run
// skips loadConfig for these so they can run before any config exists.
const annotationSkipConfigLoad = "herald.skip-config-load"

func main() {
	rootCmd := &cobra.Command{
		Use:   "herald",
		Short: "Your AI-powered news herald - intelligent RSS/Atom feed reader with AI curation",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Annotations[annotationSkipConfigLoad] == "true" {
				return nil
			}
			return loadConfig()
		},
	}

	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "config file path (default: ./config/config.toml)")
	rootCmd.PersistentFlags().StringVarP(&outputFormat, "format", "f", "json", "output format: json, text, human (default: json)")

	rootCmd.AddCommand(createUserCmd())
	rootCmd.AddCommand(importCmd())
	rootCmd.AddCommand(fetchFeedsCmd())
	rootCmd.AddCommand(processCmd())
	rootCmd.AddCommand(fetchCmd())
	rootCmd.AddCommand(daemonCmd())
	rootCmd.AddCommand(serveCmd())
	rootCmd.AddCommand(listCmd())
	rootCmd.AddCommand(readCmd())
	rootCmd.AddCommand(initConfigCmd())
	rootCmd.AddCommand(resetScoresCmd())
	rootCmd.AddCommand(resetCmd())
	rootCmd.AddCommand(backfillEmbeddingsCmd())
	rootCmd.AddCommand(embeddingDriftCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// defaultConfigPath is the search location for the config file when the
// caller does not pass --config explicitly. Kept as a constant so tests
// can refer to the same value without duplication.
const defaultConfigPath = "./config/config.toml"

// loadConfig reads the TOML config from configPath into the package-level
// cfg, layered over storage.DefaultConfig (so unset fields keep their
// defaults).
//
// Missing-file is a hard error. Earlier versions silently fell back to a
// pure-defaults config when the file didn't exist, which produced a
// surprising "empty SQLite at the binary's CWD" when the running user
// expected to be talking to their real database. Bootstrap commands that
// need to run before any config exists (init-config) opt out via the
// annotationSkipConfigLoad cobra annotation.
func loadConfig() error {
	if configPath == "" {
		configPath = defaultConfigPath
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return fmt.Errorf("config file not found: %s\n"+
			"  Create one with:  herald init-config [--config <path>]\n"+
			"  Or point at an existing config:  --config <path>",
			configPath)
	} else if err != nil {
		return fmt.Errorf("failed to stat config %s: %w", configPath, err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	cfg = storage.DefaultConfig()
	// DisallowUnknownFields turns a key that exists in the file but not in the
	// Config struct into a hard parse error instead of a silent no-op. This
	// catches typos (security_modle) and stale pre-unification layouts (a
	// top-level [webauth] that now lives under [web.webauth] -- see #197) at
	// boot, matching the fail-loud stance of the missing-file check above.
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		// StrictMissingError.String() pinpoints each offending key with a
		// source excerpt; plain Error() only says "fields ... are missing".
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return fmt.Errorf("invalid config %s (unknown or misplaced key):\n%s", configPath, strict.String())
		}
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
			// Local CLI import is admin-only and intentionally exempt from the per-user feed cap (pass 0 = unbounded).
			if err := fetcher.ImportOPML(opmlPath, userID, 0); err != nil {
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
				fetchResult.NewArticles += len(stored)

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

			groupMatcher, err := newGroupMatcher()
			if err != nil {
				return err
			}

			// The `process` command runs the global security pass (screens any
			// unscreened articles once) followed by the per-user pipeline.
			stage := newPipelineStage(store, processor, groupMatcher, formatter, userID)
			if _, err := stage.RunSecurity(ctx); err != nil {
				return err
			}
			if err := stage.Run(ctx); err != nil {
				return err
			}

			result := &output.FetchResult{}

			// Get and output high-interest articles
			highInterestArticles, scores, err := store.GetArticlesByInterestScore(userID, cfg.Thresholds.InterestScore, 10, 0, nil)
			if err != nil {
				return fmt.Errorf("failed to get high-interest articles: %w", err)
			}

			result.HighInterest = len(highInterestArticles)

			// Use structured cron output format for JSON, traditional format for others
			if outputFormat == "json" {
				return formatter.OutputCronResult(result, userID, highInterestArticles)
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
	cycleStart := time.Now()
	formatter := output.NewFormatter(output.Format(outputFormat))

	store, err := storage.NewStore(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer store.Close()

	// Persist a per-cycle summary on the way out so the web UI can show
	// throughput and backend health without access to the daemon's memory. The
	// closure reads fetchResult/aiBackendUp, populated as the cycle runs, and
	// records whatever progress was made even on an early return. Registered
	// after store.Close()'s defer, so it runs first (LIFO) while the store is
	// still open.
	var fetchResult *output.FetchResult
	aiBackendUp := false
	defer func() {
		if fetchResult == nil {
			return
		}
		if err := store.RecordCycleStats(storage.CycleStats{
			CompletedAt:        time.Now(),
			DurationMs:         time.Since(cycleStart).Milliseconds(),
			FeedsTotal:         fetchResult.FeedsTotal,
			FeedsDownloaded:    fetchResult.FeedsDownloaded,
			FeedsNotModified:   fetchResult.FeedsNotModified,
			FeedsErrored:       fetchResult.FeedsErrored,
			NewArticles:        fetchResult.NewArticles,
			Processed:          fetchResult.ProcessedCount,
			HighInterest:       fetchResult.HighInterest,
			AIBackendAvailable: aiBackendUp,
		}); err != nil {
			formatter.Warning("failed to record cycle stats: %v", err)
		}
	}()

	// Get all feeds due to fetch this cycle. Adaptive scheduling stages
	// next_fetch_at across feeds, so it's normal for an individual cycle to
	// have zero due — the AI passes downstream still need to run to drain
	// pending work from prior cycles, so we don't early-return on this.
	subscribedFeeds, err := store.GetAllSubscribedFeeds()
	if err != nil {
		return fmt.Errorf("failed to get subscribed feeds: %w", err)
	}

	// Fetch each feed once (efficient)
	fetcher := feeds.NewFetcher(store)
	fetchResult = &output.FetchResult{FeedsTotal: len(subscribedFeeds)}
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
		fetchResult.NewArticles += len(stored)

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

	// Run AI passes every cycle, regardless of whether new articles were fetched
	// this cycle — pending work from prior cycles (unscored articles, missing
	// summaries from transient failures) needs to drain.
	processor, err := ai.NewAIProcessor(cfg.Ollama.BaseURL, cfg.Ollama.SecurityModel, cfg.Ollama.CurationModel, store, cfg)
	if err != nil {
		formatter.Warning("failed to create AI processor: %v", err)
		formatter.Warning("skipping AI processing (Ollama may not be running)")
		return formatter.OutputFetchResult(fetchResult)
	}
	aiBackendUp = processor.BackendAvailable()

	// Get all users who have subscriptions
	allUserIDs, err := store.GetAllSubscribingUsers()
	if err != nil {
		return fmt.Errorf("failed to get subscribing users: %w", err)
	}

	if len(allUserIDs) == 0 {
		formatter.Warning("no users with subscriptions")
		return formatter.OutputFetchResult(fetchResult)
	}

	groupMatcher, err := newGroupMatcher()
	if err != nil {
		formatter.Warning("failed to create group matcher: %v", err)
		formatter.Warning("skipping embedding backfill and group matching")
	}

	// Security screening and summarization are global: each article is screened
	// and summarized once, with the verdict and summary shared by every
	// subscriber (#141, #162), so run both once per cycle before the per-user
	// pipelines rather than redoing them per user. One breaker check, not one
	// per user (#111).
	securityStage := newPipelineStage(store, processor, groupMatcher, formatter, cfg.DefaultUserID)
	totalProcessed, err := securityStage.RunSecurity(ctx)
	if err != nil {
		formatter.Warning("security pass failed: %v", err)
	}
	if _, err := securityStage.RunSummaries(ctx); err != nil {
		formatter.Warning("summarize pass failed: %v", err)
	}

	// Run the per-user pipeline for each subscribing user. It drives every stage
	// from its own state-driven, newest-first query, reading the article-level
	// security verdict to decide what to curate, so this cycle's freshly
	// screened articles are processed ahead of older backlog, and anything left
	// pending from prior cycles drains the same way. Self-skips when the AI
	// backend is unavailable.
	for _, userID := range allUserIDs {
		stage := newPipelineStage(store, processor, groupMatcher, formatter, userID)
		if err := stage.Run(ctx); err != nil {
			formatter.Warning("pipeline failed for user %d: %v", userID, err)
		}
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

// processNewsletters creates a temporary Engine and processes due newsletters.
func processNewsletters(ctx context.Context) error {
	if cfg.Ollama.BaseURL == "" {
		return nil // AI not configured, skip newsletters
	}
	engine, err := herald.NewEngine(herald.EngineConfig{
		DBPath:        cfg.Database.Path,
		OllamaBaseURL: cfg.Ollama.BaseURL,
		SecurityModel: cfg.Ollama.SecurityModel,
		CurationModel: cfg.Ollama.CurationModel,
		UserID:        cfg.DefaultUserID,
		// Scheduled AI digests need the summarizer in the daemon (built only when
		// a summary backend is configured). Same fields herald-web passes.
		SummaryBaseURL:         cfg.Summary.BaseURL,
		SummaryModel:           cfg.Summary.Model,
		SummaryAPIKey:          os.Getenv("HERALD_SUMMARY_API_KEY"),
		SummaryDisableThinking: cfg.Summary.DisableThinking,
		SummaryMaxInputTokens:  cfg.Summary.MaxInputTokens,
	})
	if err != nil {
		return fmt.Errorf("create engine for newsletters: %w", err)
	}
	defer engine.Close()

	return engine.ProcessDueNewsletters(ctx)
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

			if err := store.UpdateReadState(userID, articleID, true, nil, nil, nil, nil); err != nil {
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
		// init-config bootstraps the config file itself, so it must run
		// before any config exists. Skip the persistent loadConfig.
		Annotations: map[string]string{annotationSkipConfigLoad: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				configPath = defaultConfigPath
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
			data, err := toml.Marshal(cfg)
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

func backfillEmbeddingsCmd() *cobra.Command {
	var (
		batchSize    int
		resetErrors  bool
		resetPattern string
	)
	cmd := &cobra.Command{
		Use:   "backfill-embeddings",
		Short: "Generate embeddings for articles that don't have them (for semantic search)",
		Long: `Generates embedding vectors for all articles missing them, using the
configured Ollama embedding model. Processes articles in batches until all
articles have embeddings. Required for semantic search to return results.

Existing embeddings are not regenerated unless the embedding model has changed.

Use --reset-errors to clear the retry budget on rows that hit
EmbedMaxAttempts (e.g. after a sustained backend outage). Combine with
--reset-pattern to narrow the reset to a specific error class — for
example, "%HTTP 403%" to retry only rate-limited rows after fixing the
limiter, leaving rows stuck on other errors untouched.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			engine, err := herald.NewEngine(herald.EngineConfig{
				DBPath:        cfg.Database.Path,
				OllamaBaseURL: cfg.Ollama.BaseURL,
				UserID:        cfg.DefaultUserID,
			})
			if err != nil {
				return fmt.Errorf("failed to create engine: %w", err)
			}
			defer engine.Close()

			if resetErrors {
				n, err := engine.ResetStuckEmbeddings(resetPattern)
				if err != nil {
					return fmt.Errorf("reset stuck embeddings: %w", err)
				}
				if resetPattern == "" {
					fmt.Printf("Reset %d stuck embedding rows\n", n)
				} else {
					fmt.Printf("Reset %d stuck embedding rows matching %q\n", n, resetPattern)
				}
			}

			total := 0
			for {
				n, err := engine.BackfillEmbeddings(ctx, batchSize)
				if err != nil {
					return fmt.Errorf("backfill failed: %w", err)
				}
				if n == 0 {
					break
				}
				total += n
				fmt.Printf("Embedded %d articles (%d total so far)\n", n, total)
			}

			fmt.Printf("Backfill complete: %d articles embedded\n", total)
			return nil
		},
	}
	cmd.Flags().IntVar(&batchSize, "batch", 50, "number of articles to process per batch")
	cmd.Flags().BoolVar(&resetErrors, "reset-errors", false, "before backfill, clear retry budget on rows stuck at EmbedMaxAttempts so they get re-tried")
	cmd.Flags().StringVar(&resetPattern, "reset-pattern", "", "optional SQL LIKE pattern narrowing --reset-errors to matching error_message values (e.g. '%HTTP 403%')")
	return cmd
}
