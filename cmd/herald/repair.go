package main

import (
	"fmt"

	"github.com/matthewjhunter/herald/internal/feeds"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/spf13/cobra"
)

// repairCmd is the parent command for one-off passes that rewrite stored
// article text in place. Unlike `reset`, these do not hand work back to the
// daemon: the affected articles are ones the daemon can no longer reach, so
// the repair itself does the work.
func repairCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Rewrite stored article text that the daemon can no longer reach",
		Long: `One-off passes over stored article bodies.

These exist for content the ordinary pipeline cannot revisit. Full text is
fetched once per article and the flag is set whatever the outcome, so a
change to the extraction rules does not reach articles already processed
under the old ones. A repair applies the new rules to what is in the
database.`,
	}
	cmd.AddCommand(repairFullTextCmd())
	return cmd
}

// repairFullTextCmd re-runs the boilerplate trim over already-extracted bodies.
// It is the backfill for the trim added alongside it: without it, every article
// fetched before the fix keeps the sidebar and navigation text readability
// swept up, because clearing full_text_fetched would only have the next pass
// find the stored body complete and mark it done again.
func repairFullTextCmd() *cobra.Command {
	var dryRun bool
	var samples int
	cmd := &cobra.Command{
		Use:   "fulltext-boilerplate",
		Short: "Re-trim sidebar and navigation text out of stored full-text bodies",
		Long: `Re-runs the readability boilerplate trim over every article whose body
came from a full-text extraction, rewriting the ones that change.

The trim is a no-op on content it does not recognize, so this is safe to
run over the whole corpus and safe to run more than once. Use --dry-run
to count the affected articles without writing.

The run reports a per-feed breakdown, because the total alone cannot
answer the question worth asking before rewriting bodies in place: is
this concentrated in the few sites whose layout confuses readability,
or is it touching everything? Add --samples N to print up to N of the
blocks each feed would lose, which prints article text and so is off by
default.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer store.Close()

			report, err := feeds.RetrimStoredExtractions(cmd.Context(), store, dryRun, samples)
			if err != nil {
				return fmt.Errorf("after %d scanned, %d rewritten: %w", report.Scanned, report.Changed, err)
			}

			verb := "Rewrote"
			if dryRun {
				verb = "Would rewrite"
			}
			fmt.Printf("Scanned %d extracted articles. %s %d, dropping %d characters.\n",
				report.Scanned, verb, report.Changed, report.CharsRemoved)

			if len(report.Feeds) > 0 {
				fmt.Printf("\n%-40s %8s %8s %10s\n", "FEED", "CHANGED", "SCANNED", "CHARS")
				for _, f := range report.Feeds {
					fmt.Printf("%-40.40s %8d %8d %10d\n", f.FeedTitle, f.Changed, f.Scanned, f.CharsRemoved)
					for _, s := range f.Samples {
						fmt.Printf("    - %.160s\n", s)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without writing")
	cmd.Flags().IntVar(&samples, "samples", 0, "print up to N removed blocks per feed (prints article text)")
	return cmd
}
