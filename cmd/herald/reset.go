package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/spf13/cobra"
)

// resetCmd is the parent command for reset operations. Subcommands clear
// specific kinds of state in the database so the daemon will redo the
// corresponding work on the next cycle. The CLI mutates state; the
// daemon does the work — `reset` does NOT block on regeneration.
func resetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset specific kinds of AI-derived state so the daemon redoes it",
		Long: `Pure DB state mutators. Each subcommand clears a category of
AI-derived state (embeddings, summaries, scores, …) so the daemon
will regenerate it on subsequent cycles.

The reset itself is fast — just SQL. Regeneration runs at the daemon's
own pace via its existing per-cycle backfill passes. To run reset on
a non-running deployment, the daemon needs to be started afterward to
do the work.`,
	}
	cmd.AddCommand(resetEmbeddingsCmd())
	return cmd
}

// resetEmbeddingsCmd clears all per-article embeddings AND all group
// centroid embeddings. Used after an embed-format change (e.g. switching
// from title+content to a metadata-enriched record) when every existing
// vector becomes incompatible with new ones.
//
// Group memberships are preserved. Centroids will repopulate gradually
// as new articles re-embed and rejoin groups via the scoring pipeline.
func resetEmbeddingsCmd() *cobra.Command {
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "embeddings",
		Short: "Delete all article embeddings and clear group centroids",
		Long: `Deletes every row in article_embeddings and article_embedding_chunks,
and sets article_groups.embedding to NULL on every group.

The daemon's per-cycle embedding backfill repopulates article embeddings
on subsequent cycles. Throughput is set by embed_batch_size rather than
max_parallel: the backends serialize per model, so batch size is the lever.

Group centroids stay NULL until articles rejoin groups via the scoring
pipeline. Existing memberships are preserved — only the centroid vector
is cleared. New articles will not match groups via embedding pre-filter
until centroids rebuild, but the LLM grouping pass still works on
topic/display-name.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer store.Close()

			if !assumeYes {
				fmt.Fprintln(os.Stderr, "This will:")
				fmt.Fprintln(os.Stderr, "  • DELETE every row from article_embeddings and article_embedding_chunks")
				fmt.Fprintln(os.Stderr, "  • SET article_groups.embedding = NULL on every group")
				fmt.Fprintln(os.Stderr, "")
				fmt.Fprintln(os.Stderr, "The daemon will refill article embeddings on subsequent cycles.")
				fmt.Fprintln(os.Stderr, "Group centroids will rebuild gradually as articles rejoin groups.")
				fmt.Fprint(os.Stderr, "Proceed? [y/N]: ")

				reader := bufio.NewReader(os.Stdin)
				resp, _ := reader.ReadString('\n')
				resp = strings.TrimSpace(strings.ToLower(resp))
				if resp != "y" && resp != "yes" {
					fmt.Fprintln(os.Stderr, "Aborted.")
					return nil
				}
			}

			embRows, err := store.ResetAllArticleEmbeddings()
			if err != nil {
				return fmt.Errorf("reset article embeddings: %w", err)
			}
			groupRows, err := store.ResetAllGroupEmbeddings()
			if err != nil {
				return fmt.Errorf("reset group embeddings: %w", err)
			}

			fmt.Printf("Cleared %d article embeddings and %d group centroids.\n", embRows, groupRows)
			fmt.Println("Daemon will refill article embeddings on next cycle (per-cycle backfill).")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip confirmation prompt")
	return cmd
}
