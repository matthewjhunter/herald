package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func daemonCmd() *cobra.Command {
	var interval time.Duration
	var stagesCSV string

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run fetch+process in a loop with configurable interval",
		Long: `Continuously fetch feeds and process articles with AI on a timer.
Designed for running inside a Docker container or as a background service.
Handles SIGINT/SIGTERM for graceful shutdown (finishes the current cycle).

--stages selects which of the three independent loops this process runs
(fetch, screen, curate), comma-separated; the default runs all three, the
original single-service behavior. Run them as separate services -- each with
its own image command and interval -- to decouple the slow, LLM-bound screen
pass from feed fetching and per-user curation (#233). The security screen is
concurrency-safe across multiple screen loops via a per-article claim/lease.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			stages, err := parseStages(stagesCSV)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

			// Cancel the context immediately on signal so in-flight
			// HTTP and Ollama requests abort rather than waiting for
			// the current cycle to complete.
			go func() {
				<-sig
				slog.Info("herald daemon: shutdown signal received, cancelling the current cycle")
				cancel()
			}()

			slog.Info("herald daemon: starting", "interval", interval, "stages", stagesCSV)

			cycle := 1
			for {
				start := time.Now()
				slog.Info("herald daemon: cycle starting", "cycle", cycle)

				if err := runCycle(ctx, stages); err != nil {
					if ctx.Err() != nil {
						slog.Info("herald daemon: cycle cancelled, exiting")
						return nil
					}
					slog.Error("herald daemon: cycle failed", "cycle", cycle, "err", err)
				} else {
					slog.Info("herald daemon: cycle complete", "cycle", cycle, "duration", time.Since(start).Round(time.Millisecond))
				}

				// Due newsletters belong to the curate stage (per-user output); a
				// fetch- or screen-only loop does not send them.
				if stages.has(stageCurate) {
					if err := processNewsletters(ctx); err != nil {
						if ctx.Err() != nil {
							return nil
						}
						slog.Error("herald daemon: newsletter processing failed", "err", err)
					}
				}

				cycle++

				// Wait for the next tick or a shutdown signal.
				timer := time.NewTimer(interval)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
			}
		},
	}

	cmd.Flags().DurationVarP(&interval, "interval", "i", 5*time.Minute, "duration between cycles (e.g. 5m, 30s, 1h)")
	cmd.Flags().StringVar(&stagesCSV, "stages", "", "comma-separated stages to run: fetch,screen,curate (default all)")
	return cmd
}
