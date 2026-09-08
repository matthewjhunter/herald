package feeds

import (
	"context"
	"fmt"
	"sort"

	"github.com/matthewjhunter/herald/internal/storage"
)

// retrimPageSize is how many articles a repair pass reads per round trip.
const retrimPageSize = 500

// RetrimFeedStat is what a repair pass would do, or did, to one feed.
type RetrimFeedStat struct {
	FeedID    int64
	FeedTitle string
	Scanned   int
	Changed   int
	// CharsRemoved is text characters dropped, not bytes: markup the trim
	// rewrites on its way through would otherwise show up as a change to
	// content that no reader ever sees.
	CharsRemoved int
	// Samples holds the text of blocks the trim dropped, longest first, up to
	// the caller's limit. Empty unless samples were asked for.
	Samples []string
}

// RetrimReport is the whole pass, in total and broken down by feed.
//
// The breakdown exists because the totals alone cannot answer the question an
// operator actually has before rewriting bodies in place: is this concentrated
// in the few sites whose layout is known to confuse readability, or is it
// touching everything? Those have very different explanations, and only one of
// them is the fix working as intended.
type RetrimReport struct {
	Scanned      int
	Changed      int
	CharsRemoved int
	// Feeds holds only feeds with at least one change, most changes first.
	Feeds []RetrimFeedStat
}

// RetrimStoredExtractions re-runs the boilerplate trim over article bodies that
// were already replaced by a full-text extraction, rewriting the ones that
// change.
//
// It exists because the ordinary path cannot reach these articles. Full text is
// fetched once -- full_text_fetched is set whatever the outcome -- and the
// stored body is no longer an excerpt, so clearing the flag would only have the
// next pass conclude the content is not truncated and mark it done again. The
// sidebar text is in the database and only a rewrite gets it out.
//
// The trim is a no-op on content it does not recognize, so the pass is safe to
// run over the whole corpus and safe to run twice. dryRun reports what would
// change without writing. samplesPerFeed caps the removed blocks kept per feed
// for inspection; 0 keeps none, which is the right default for a pass whose
// output may be pasted somewhere.
func RetrimStoredExtractions(ctx context.Context, store storage.Store, dryRun bool, samplesPerFeed int) (*RetrimReport, error) {
	report := &RetrimReport{}
	byFeed := map[int64]*RetrimFeedStat{}

	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		batch, err := store.GetFetchedFullTextArticles(afterID, retrimPageSize)
		if err != nil {
			return report, fmt.Errorf("read full-text articles after %d: %w", afterID, err)
		}
		if len(batch) == 0 {
			finalizeRetrimReport(report, byFeed)
			return report, nil
		}
		for _, a := range batch {
			afterID = a.ID
			report.Scanned++

			stat, ok := byFeed[a.FeedID]
			if !ok {
				stat = &RetrimFeedStat{FeedID: a.FeedID, FeedTitle: a.FeedTitle}
				byFeed[a.FeedID] = stat
			}
			stat.Scanned++

			content, removedContent := trimSurroundingBoilerplateDetail(a.Content)
			linked, removedLinked := trimSurroundingBoilerplateDetail(a.LinkedContent)
			if content == a.Content && linked == a.LinkedContent {
				continue
			}

			report.Changed++
			stat.Changed++
			dropped := (textLength(a.Content) - textLength(content)) +
				(textLength(a.LinkedContent) - textLength(linked))
			report.CharsRemoved += dropped
			stat.CharsRemoved += dropped
			if samplesPerFeed > 0 {
				stat.Samples = keepLongestSamples(stat.Samples, append(removedContent, removedLinked...), samplesPerFeed)
			}

			if dryRun {
				continue
			}
			if err := store.UpdateArticleExtractedContent(a.ID, content, linked); err != nil {
				finalizeRetrimReport(report, byFeed)
				return report, fmt.Errorf("rewrite article %d: %w", a.ID, err)
			}
		}
	}
}

// keepLongestSamples merges new removed blocks into the kept set, longest
// first, capped at limit. Longest rather than first: a run of empty layout
// divs would otherwise fill the sample slots and hide the block that actually
// carried text.
func keepLongestSamples(kept, incoming []string, limit int) []string {
	for _, s := range incoming {
		if s != "" {
			kept = append(kept, s)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return len(kept[i]) > len(kept[j]) })
	if len(kept) > limit {
		kept = kept[:limit]
	}
	return kept
}

// finalizeRetrimReport flattens the per-feed map into the report, dropping
// feeds nothing changed on and ordering the rest by how much they changed.
func finalizeRetrimReport(report *RetrimReport, byFeed map[int64]*RetrimFeedStat) {
	for _, stat := range byFeed {
		if stat.Changed > 0 {
			report.Feeds = append(report.Feeds, *stat)
		}
	}
	sort.Slice(report.Feeds, func(i, j int) bool {
		if report.Feeds[i].Changed != report.Feeds[j].Changed {
			return report.Feeds[i].Changed > report.Feeds[j].Changed
		}
		return report.Feeds[i].FeedID < report.Feeds[j].FeedID
	})
}
