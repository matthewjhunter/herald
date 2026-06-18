package main

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"

	embedding "github.com/matthewjhunter/go-embedding"
	herald "github.com/matthewjhunter/herald"
	"github.com/spf13/cobra"
)

// embeddingDriftCmd compares stored article-embedding vectors against
// vectors produced by the *current* embedding pipeline. It samples N
// already-embedded rows, re-runs each through BuildArticleEmbedInput
// and the configured embedder, and reports cosine similarity between
// the stored and freshly-computed vectors.
//
// Use case: any time the embedding input format changes (added
// metadata fields, body stripping, task-prefix change, model swap), the
// stored corpus and the new pipeline can drift apart. A global
// re-embed is expensive; this tool quantifies whether the drift is
// large enough to matter before paying that cost.
//
// Interpreting the cosine numbers:
//   - >= 0.99   negligible drift; mixed corpus is fine
//   - 0.95-0.99 small drift; ranking will shift at the margins
//   - 0.85-0.95 meaningful drift; clustering quality affected
//   - <  0.85   substantial drift; a global re-embed is justified
func embeddingDriftCmd() *cobra.Command {
	var (
		userID  int64
		nSample int
		seed    int64
	)
	cmd := &cobra.Command{
		Use:   "embedding-drift",
		Short: "Compare stored embeddings against the current pipeline's output (drift check)",
		Long: `Samples articles that already have embeddings and re-runs them through the
current BuildArticleEmbedInput → EmbedRecord pipeline. Reports cosine similarity
between stored and freshly-computed vectors, plus a histogram, summary stats,
and the worst-drift examples.

Use this before deciding whether an embedding-input change (metadata enrichment,
body stripping, model swap) requires a global re-embed. A high mean cosine
(>= 0.99) means existing vectors are still compatible with the new pipeline; a
lower mean means the corpus has effectively bifurcated and search/clustering
quality will suffer until you re-embed.

The check uses real API calls against the configured embedding backend, so a
larger sample uses real throughput from the same rate-limit budget the daemon
shares. Default sample size is intentionally small (30) to keep the cost down.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			engine, err := herald.NewEngine(herald.EngineConfig{
				DBPath:        cfg.Database.Path,
				OllamaBaseURL: cfg.Ollama.BaseURL,
				UserID:        cfg.DefaultUserID,
			})
			if err != nil {
				return fmt.Errorf("create engine: %w", err)
			}
			defer engine.Close()

			model := engine.EmbeddingModel()
			if model == "" {
				return fmt.Errorf("embedding not configured (no embed model)")
			}

			rows, err := engine.Store().GetArticleEmbeddings(userID, model)
			if err != nil {
				return fmt.Errorf("load stored embeddings: %w", err)
			}

			// GetArticleEmbeddings returns only status-ok rows, so every row
			// carries a real vector (no sentinel placeholders to filter).
			var realRows []driftRow
			for _, r := range rows {
				realRows = append(realRows, driftRow{ID: r.ArticleID, Stored: r.Embedding})
			}
			if len(realRows) == 0 {
				return fmt.Errorf("no real embeddings found for user %d, model %q", userID, model)
			}

			rng := rand.New(rand.NewSource(seed))
			rng.Shuffle(len(realRows), func(i, j int) { realRows[i], realRows[j] = realRows[j], realRows[i] })
			if nSample > len(realRows) {
				nSample = len(realRows)
			}
			realRows = realRows[:nSample]

			fmt.Printf("Comparing %d stored embeddings against current pipeline (model=%s)\n\n",
				len(realRows), model)

			results := make([]driftResult, 0, len(realRows))
			for i := range realRows {
				row := &realRows[i]
				article, err := engine.Store().GetArticle(row.ID)
				if err != nil || article == nil {
					fmt.Printf("[%d/%d] article %d: skip (not found)\n", i+1, len(realRows), row.ID)
					continue
				}
				fields, body := herald.BuildArticleEmbedInput(engine.Store(), *article)
				newVec, err := engine.EmbedRecord(ctx, fields, body)
				if err != nil {
					fmt.Printf("[%d/%d] article %d: embed error: %v\n", i+1, len(realRows), row.ID, err)
					continue
				}
				if newVec == nil {
					// Body fell below minEmbedContentLen — would now be a
					// deterministic skip. Stored vector exists from when
					// the row was longer (or pre-strip).
					fmt.Printf("[%d/%d] article %d: body now too short to embed (was previously embedded)\n",
						i+1, len(realRows), row.ID)
					continue
				}
				sim := embedding.CosineSimilarity(row.Stored, newVec)
				results = append(results, driftResult{
					ArticleID: row.ID,
					Title:     article.Title,
					Cosine:    sim,
				})
				fmt.Printf("[%d/%d] article %d cos=%.4f  %s\n",
					i+1, len(realRows), row.ID, sim, truncate(article.Title, 70))
			}

			if len(results) == 0 {
				return fmt.Errorf("no comparable rows after filtering")
			}
			printDriftSummary(results)
			return nil
		},
	}
	cmd.Flags().Int64Var(&userID, "user", 1, "user ID whose subscribed-feed embeddings to sample from")
	cmd.Flags().IntVar(&nSample, "n", 30, "number of articles to sample")
	cmd.Flags().Int64Var(&seed, "seed", 1, "rng seed for reproducible sampling")
	return cmd
}

type driftRow struct {
	ID     int64
	Stored []float32
}

type driftResult struct {
	ArticleID int64
	Title     string
	Cosine    float64
}

func printDriftSummary(rs []driftResult) {
	cosines := make([]float64, len(rs))
	for i, r := range rs {
		cosines[i] = r.Cosine
	}
	sort.Float64s(cosines)

	mean := 0.0
	for _, v := range cosines {
		mean += v
	}
	mean /= float64(len(cosines))

	median := cosines[len(cosines)/2]
	p10 := cosines[int(0.1*float64(len(cosines)))]
	p25 := cosines[int(0.25*float64(len(cosines)))]
	p75 := cosines[int(0.75*float64(len(cosines)))]
	min := cosines[0]
	max := cosines[len(cosines)-1]

	fmt.Println()
	fmt.Println("=== Cosine similarity (stored vs. current pipeline) ===")
	fmt.Printf("  n      = %d\n", len(cosines))
	fmt.Printf("  min    = %.4f\n", min)
	fmt.Printf("  p10    = %.4f\n", p10)
	fmt.Printf("  p25    = %.4f\n", p25)
	fmt.Printf("  median = %.4f\n", median)
	fmt.Printf("  p75    = %.4f\n", p75)
	fmt.Printf("  max    = %.4f\n", max)
	fmt.Printf("  mean   = %.4f\n", mean)

	fmt.Println()
	fmt.Println("=== Histogram ===")
	buckets := []struct {
		lo, hi float64
		label  string
	}{
		{0.99, 1.001, ">=0.99 negligible"},
		{0.95, 0.99, "0.95-0.99 small"},
		{0.90, 0.95, "0.90-0.95 moderate"},
		{0.85, 0.90, "0.85-0.90 meaningful"},
		{0.70, 0.85, "0.70-0.85 large"},
		{-1.001, 0.70, "<0.70 substantial"},
	}
	for _, b := range buckets {
		count := 0
		for _, v := range cosines {
			if v >= b.lo && v < b.hi {
				count++
			}
		}
		var bar strings.Builder
		for i := 0; i < count; i++ {
			bar.WriteString("#")
		}
		fmt.Printf("  %-22s %3d  %s\n", b.label, count, bar.String())
	}

	sort.Slice(rs, func(i, j int) bool { return rs[i].Cosine < rs[j].Cosine })
	fmt.Println()
	fmt.Println("=== Worst drift (lowest cosine) ===")
	worst := 5
	if worst > len(rs) {
		worst = len(rs)
	}
	for i := 0; i < worst; i++ {
		fmt.Printf("  cos=%.4f  article %d  %s\n",
			rs[i].Cosine, rs[i].ArticleID, truncate(rs[i].Title, 70))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
