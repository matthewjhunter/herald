package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"text/tabwriter"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/spf13/cobra"
)

// screenCompareCmd is the plan-012 rescore sanity check. It runs the three
// detectors side by side over a sample of already-screened articles and reports
// the deltas, so the operator can SEE how much the number moves and why before
// nulling production and rescoring. It PERSISTS NOTHING -- no columns, no verdict
// rows, just a table on stdout that you read and discard.
//
// Everything is reported on the THREAT scale (0 = clean, higher = worse):
//
//	regex       airlock/detect Score()/10 -- the deterministic prescreen
//	old_prompt  the legacy Herald prompt, re-run live, converted 10 - score
//	new_prompt  airlock's screen verdict (screen.Verdict.Threat)
//	combined    max(new_prompt, regex) -- the value the rescore will store
//	old_stored  the article's current security_threat column, converted 10 - x
//	            (pre-rescore that column still holds the old 10 = safe value)
//
// Converting 10 - x for display is fine here: this is a read-only report, not the
// migration -- it never writes back. Two model calls per article (new + legacy),
// so keep the sample modest.
func screenCompareCmd() *cobra.Command {
	var sample int
	var unsafeFirst bool
	cmd := &cobra.Command{
		Use:   "screen-compare",
		Short: "Compare regex/old-prompt/new-prompt/combined security scores over a sample (plan 012; diagnostic, persists nothing)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer store.Close()

			processor, err := ai.NewAIProcessor(cfg.Ollama.BaseURL, cfg.Ollama.SecurityModel, cfg.Ollama.CurationModel, store, cfg)
			if err != nil {
				return fmt.Errorf("create AI processor (is Ollama running?): %w", err)
			}

			sampler := store.GetScreenedArticleSample
			if unsafeFirst {
				// Bias toward the articles prod flagged: worst stored verdict
				// first. This is where old and new prompts most likely disagree.
				sampler = store.GetLowSafetyArticleSample
			}
			arts, err := sampler(sample)
			if err != nil {
				return fmt.Errorf("sample screened articles: %w", err)
			}
			if len(arts) == 0 {
				fmt.Fprintln(os.Stdout, "no screened articles with content to sample")
				return nil
			}

			passT := cfg.Thresholds.MaxSecurityThreat
			if passT == 0 {
				passT = 3.0
			}
			borderT := cfg.Thresholds.SecurityBorderlineThreat
			if borderT == 0 {
				borderT = 6.0
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "id\tregex\told_prompt\tnew_prompt\tcombined\told_stored\tclass_shift")

			var (
				n           int
				shifts      int
				sumAbsDelta float64
				regexOnly   int // regex would flag, new_prompt would not
				modelOnly   int // new_prompt would flag, regex would not
				failedCalls int
			)
			for _, a := range arts {
				res, err := processor.SecurityCheck(ctx, a.Title, a.Content)
				if err != nil {
					failedCalls++
					continue
				}
				legacy, err := processor.LegacySecurityScore(ctx, a.Title, a.Content)
				if err != nil {
					failedCalls++
					continue
				}
				n++

				oldPrompt := 10.0 - legacy
				newPrompt := res.LLMThreat
				regexT := res.RegexThreat
				combined := res.Threat
				oldStored := math.NaN()
				if a.StoredThreat != nil {
					oldStored = 10.0 - *a.StoredThreat
				}

				shift := ""
				if !math.IsNaN(oldStored) {
					if band(combined, passT, borderT) != band(oldStored, passT, borderT) {
						shift = band(oldStored, passT, borderT) + "->" + band(combined, passT, borderT)
						shifts++
					}
					sumAbsDelta += math.Abs(combined - oldStored)
				}
				if regexT > passT && newPrompt <= passT {
					regexOnly++
				}
				if newPrompt > passT && regexT <= passT {
					modelOnly++
				}

				fmt.Fprintf(w, "%d\t%.1f\t%.1f\t%.1f\t%.1f\t%s\t%s\n",
					a.ID, regexT, oldPrompt, newPrompt, combined, fmtThreat(oldStored), shift)
			}
			w.Flush()

			fmt.Fprintf(os.Stdout, "\nsampled %d, scored %d, model-call failures %d\n", len(arts), n, failedCalls)
			if n > 0 {
				fmt.Fprintf(os.Stdout, "classification shifts (combined vs old_stored): %d/%d\n", shifts, n)
				fmt.Fprintf(os.Stdout, "mean |combined - old_stored|: %.2f\n", sumAbsDelta/float64(n))
				fmt.Fprintf(os.Stdout, "regex-only flags (rule fired, model calm): %d\n", regexOnly)
				fmt.Fprintf(os.Stdout, "model-only flags (model fired, no rule): %d\n", modelOnly)
			}
			fmt.Fprintln(os.Stdout, "\nThis report persists nothing. Read it, then decide whether to rescore.")
			return nil
		},
	}
	cmd.Flags().IntVar(&sample, "sample", 50, "number of screened articles to sample")
	cmd.Flags().BoolVar(&unsafeFirst, "unsafe-first", false, "bias the sample toward the lowest-scoring (prod-flagged) articles instead of random")
	return cmd
}

// band classifies a threat into the pipeline's pass/borderline/fail bands.
func band(threat, passT, borderT float64) string {
	switch {
	case threat <= passT:
		return "pass"
	case threat <= borderT:
		return "border"
	default:
		return "fail"
	}
}

func fmtThreat(t float64) string {
	if math.IsNaN(t) {
		return "-"
	}
	return fmt.Sprintf("%.1f", t)
}
