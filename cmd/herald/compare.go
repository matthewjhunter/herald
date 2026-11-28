package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/matthewjhunter/herald/internal/storage"
)

// compareCmd reports how two models' output differs over the articles both
// have handled.
func compareCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compare",
		Short: "Compare two models' security scans or summaries over the articles both have handled (read-only)",
	}

	var disagreements int
	screens := &cobra.Command{
		Use:   "screens [model-a model-b]",
		Short: "Agreement between two models' security verdicts; with no arguments, list the models on record",
		Args:  argsNoneOrTwo,
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer store.Close()
			if len(args) == 0 {
				models, err := store.ListScreenModels()
				if err != nil {
					return err
				}
				writeModelRows(os.Stdout, "scans", models)
				return nil
			}
			pass, border := screenThresholds()
			cmp, err := store.CompareScreens(args[0], args[1], pass, border, disagreements)
			if err != nil {
				return err
			}
			writeScreenComparison(os.Stdout, cmp)
			return nil
		},
	}
	screens.Flags().IntVar(&disagreements, "disagreements", 25, "how many disagreeing articles to list, widest gap first")

	var samples int
	summaries := &cobra.Command{
		Use:   "summaries [model-a model-b]",
		Short: "Coverage, length and timing of two models' summaries, with side-by-side samples; with no arguments, list the models on record",
		Args:  argsNoneOrTwo,
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := storage.NewStore(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer store.Close()
			if len(args) == 0 {
				models, err := store.ListSummaryModels()
				if err != nil {
					return err
				}
				writeModelRows(os.Stdout, "summary rows", models)
				return nil
			}
			cmp, err := store.CompareSummaries(args[0], args[1], samples)
			if err != nil {
				return err
			}
			writeSummaryComparison(os.Stdout, cmp)
			return nil
		},
	}
	summaries.Flags().IntVar(&samples, "samples", 10, "how many side-by-side pairs to print; the same articles every run")

	cmd.AddCommand(screens, summaries)
	return cmd
}

func argsNoneOrTwo(_ *cobra.Command, args []string) error {
	if len(args) != 0 && len(args) != 2 {
		return fmt.Errorf("give two model names to compare, or none to list the models on record (got %d)", len(args))
	}
	return nil
}

// screenThresholds is the pass and borderline ceilings from config, with the
// defaults the pipeline applies when they are unset.
func screenThresholds() (pass, border float64) {
	pass, border = cfg.Thresholds.MaxSecurityThreat, cfg.Thresholds.SecurityBorderlineThreat
	if pass == 0 {
		pass = 3.0
	}
	if border == 0 {
		border = 6.0
	}
	return pass, border
}

// printable makes untrusted text safe to write to a terminal on one line:
// whitespace collapses to single spaces, and control and format characters
// (escape sequences, C1 controls, bidirectional overrides) become "?".
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			b.WriteByte(' ')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func writeModelRows(w io.Writer, what string, models []storage.ModelRows) {
	if len(models) == 0 {
		fmt.Fprintf(w, "No %s on record.\n", what)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "model\t%s\n", what)
	for _, m := range models {
		name := m.Model
		if name == "" {
			name = "(not recorded)"
		}
		fmt.Fprintf(tw, "%s\t%d\n", printable(name), m.Rows)
	}
	tw.Flush() //nolint:errcheck // stdout
}

func pct(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return 100 * float64(n) / float64(of)
}

func writeScreenComparison(w io.Writer, c *storage.ScreenComparison) {
	a, b := printable(c.ModelA), printable(c.ModelB)
	if c.Compared == 0 {
		fmt.Fprintf(w, "No article has a scan from both %s and %s.\n", a, b)
		return
	}
	fmt.Fprintf(w, "%d articles scanned by both %s and %s\n\n", c.Compared, a, b)
	fmt.Fprintf(w, "same bucket: %d (%.1f%%)\n", c.Agree(), pct(c.Agree(), c.Compared))
	// The two cells that matter most: one model would show readers an
	// article the other would hide.
	fmt.Fprintf(w, "%s passes, %s blocks: %d\n", a, b, c.Matrix[storage.ScreenBucketPass][storage.ScreenBucketBlock])
	fmt.Fprintf(w, "%s passes, %s blocks: %d\n", b, a, c.Matrix[storage.ScreenBucketBlock][storage.ScreenBucketPass])
	fmt.Fprintf(w, "mean |threat difference|: %.2f\n\n", c.MeanAbsDelta)

	names := [3]string{"pass", "borderline", "block"}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "%s \\ %s\tpass\tborderline\tblock\n", a, b)
	for i, row := range c.Matrix {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\n", names[i], row[0], row[1], row[2])
	}
	tw.Flush() //nolint:errcheck // caller's writer

	if len(c.Disagreements) == 0 {
		return
	}
	fmt.Fprintf(w, "\nDisagreements, widest gap first:\n")
	tw = tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "article\t%s\t%s\ttitle\n", a, b)
	for _, d := range c.Disagreements {
		fmt.Fprintf(tw, "%d\t%.0f %s\t%.0f %s\t%s\n", d.ArticleID,
			d.ThreatA, printable(d.CategoryA), d.ThreatB, printable(d.CategoryB), printable(truncateRunes(d.Title, 70)))
	}
	tw.Flush() //nolint:errcheck // caller's writer
}

func writeSummaryComparison(w io.Writer, c *storage.SummaryComparison) {
	a, b := printable(c.ModelA), printable(c.ModelB)
	if c.Compared == 0 {
		fmt.Fprintf(w, "No article has a summary row from both %s and %s.\n", a, b)
		return
	}
	fmt.Fprintf(w, "%d articles with a row from both %s and %s\n\n", c.Compared, a, b)
	fmt.Fprintf(w, "both produced a summary: %d (%.1f%%)\n", c.BothText, pct(c.BothText, c.Compared))
	fmt.Fprintf(w, "only %s: %d\n", a, c.OnlyAText)
	fmt.Fprintf(w, "only %s: %d\n", b, c.OnlyBText)
	fmt.Fprintf(w, "neither: %d\n\n", c.NeitherText)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "\t%s\t%s\n", a, b)
	fmt.Fprintf(tw, "average length (chars, where both summarized)\t%.0f\t%.0f\n", c.AvgCharsA, c.AvgCharsB)
	fmt.Fprintf(tw, "average model call\t%.1fs\t%.1fs\n", c.AvgDurationA.Seconds(), c.AvgDurationB.Seconds())
	tw.Flush() //nolint:errcheck // caller's writer

	for _, p := range c.Samples {
		fmt.Fprintf(w, "\n--- article %d: %s\n", p.ArticleID, printable(truncateRunes(p.Title, 90)))
		fmt.Fprintf(w, "[%s] %s\n", a, printable(p.SummaryA))
		fmt.Fprintf(w, "[%s] %s\n", b, printable(p.SummaryB))
	}
}

// truncateRunes shortens s to at most n runes, marking the cut. Rune-wise,
// so it never splits a character the way a byte slice would.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}
