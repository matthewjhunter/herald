package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/output"
	"github.com/matthewjhunter/herald/internal/pipeline"
	"github.com/matthewjhunter/herald/internal/storage"
)

// regenerateCmd fills in the configured model's summaries and security scans
// for articles another model already handled (docs/model-history.md).
func regenerateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "regenerate",
		Short: "Produce the configured model's summaries or security scans for articles it has not handled",
		Long: `Runs the currently configured model over articles that have no summary,
or no security scan, from that model: ones another model handled, and ones
handled before models were recorded.

Both subcommands are resumable. Stop one with Ctrl-C and run it again; it
continues with whatever is still missing. Both compete with the daemon for the
same backend, so expect live scoring to slow down while one runs.`,
	}

	var limit int
	summaries := &cobra.Command{
		Use:   "summaries",
		Short: "Summarize every security-passed article the configured model has not summarized; the new summary becomes the one readers see",
		Long: `Summarizes every security-passed article that has no summary from the
configured summary model. The new summary becomes the article's current one;
the summary it replaces is kept in the history, for 'herald compare summaries'.

The current summary is prefixed to every embedding chunk, so articles whose
summary changes have stale embeddings until they are re-embedded.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRegenerate(cmd.Context(), limit, "summaries",
				func(p *ai.AIProcessor) (string, error) { return p.SummaryModel() },
				func(st *pipeline.Stage) func(context.Context, pipeline.Regen) (pipeline.RegenResult, error) {
					return st.RegenerateSummaries
				})
		},
	}
	screens := &cobra.Command{
		Use:   "screens",
		Short: "Scan every already-screened article with the configured model, as a shadow: recorded for comparison, gating nothing",
		Long: `Runs the configured security model over every article that already has a
verdict and no scan from that model. The scans are shadows: each is recorded
in the screen history for 'herald compare screens', and none changes the
verdict that decides whether readers see the article.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRegenerate(cmd.Context(), limit, "screens",
				func(p *ai.AIProcessor) (string, error) { return p.ScreenModel(), nil },
				func(st *pipeline.Stage) func(context.Context, pipeline.Regen) (pipeline.RegenResult, error) {
					return st.RegenerateScreens
				})
		},
	}
	for _, c := range []*cobra.Command{summaries, screens} {
		c.Flags().IntVar(&limit, "limit", 0, "stop after this many articles (0 = all)")
		cmd.AddCommand(c)
	}
	return cmd
}

func runRegenerate(parent context.Context, limit int, what string,
	modelOf func(*ai.AIProcessor) (string, error),
	run func(*pipeline.Stage) func(context.Context, pipeline.Regen) (pipeline.RegenResult, error),
) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := storage.NewStore(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()
	processor, err := ai.NewAIProcessor(cfg.Ollama.BaseURL, cfg.Ollama.SecurityModel, cfg.Ollama.CurationModel, store, cfg)
	if err != nil {
		return fmt.Errorf("create AI processor: %w", err)
	}
	model, err := modelOf(processor)
	if err != nil {
		return err
	}
	if model == "" {
		return fmt.Errorf("no model is configured for %s", what)
	}

	formatter := output.NewFormatter(output.Format(outputFormat))
	stage := newPipelineStage(store, processor, nil, formatter, cfg.DefaultUserID)
	fmt.Fprintf(os.Stderr, "regenerating %s with %s\n", what, model)
	start := time.Now()
	res, err := run(stage)(ctx, pipeline.Regen{
		Model: model,
		Limit: limit,
		Progress: func(r pipeline.RegenResult) {
			fmt.Fprintf(os.Stderr, "  %d attempted, %d recorded, %s elapsed\n", r.Attempted, r.Recorded, time.Since(start).Round(time.Second))
		},
	})
	fmt.Fprintf(os.Stdout, "%s: %d attempted, %d recorded, %d left for a later run, in %s\n",
		what, res.Attempted, res.Recorded, res.Attempted-res.Recorded, time.Since(start).Round(time.Second))
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "interrupted; run again to continue")
		return nil
	}
	return err
}
