package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func daemonCmd() *cobra.Command {
	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run fetch+process in a loop with configurable interval",
		Long: `Continuously fetch feeds and process articles with AI on a timer.
Designed for running inside a Docker container or as a background service.
Handles SIGINT/SIGTERM for graceful shutdown (finishes the current cycle).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

			log.Printf("herald daemon: starting with interval %s", interval)

			cycle := 1
			for {
				start := time.Now()
				log.Printf("herald daemon: cycle %d starting", cycle)

				if err := doFetch(ctx); err != nil {
					log.Printf("herald daemon: cycle %d error: %v", cycle, err)
				} else {
					log.Printf("herald daemon: cycle %d completed in %s", cycle, time.Since(start).Round(time.Millisecond))
				}

				cycle++

				// Wait for the next tick or a shutdown signal.
				timer := time.NewTimer(interval)
				select {
				case <-sig:
					timer.Stop()
					log.Println("herald daemon: received shutdown signal, exiting")
					return nil
				case <-timer.C:
				}
			}
		},
	}

	cmd.Flags().DurationVarP(&interval, "interval", "i", 5*time.Minute, "duration between fetch cycles (e.g. 5m, 30s, 1h)")
	return cmd
}
