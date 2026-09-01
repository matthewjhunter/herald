package feeds

import (
	"context"
	"fmt"

	"github.com/matthewjhunter/herald/internal/storage"
)

// retrimPageSize is how many articles a repair pass reads per round trip.
const retrimPageSize = 500

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
// change without writing.
//
// Returns the number of articles scanned and the number rewritten.
func RetrimStoredExtractions(ctx context.Context, store storage.Store, dryRun bool) (scanned, changed int, err error) {
	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return scanned, changed, err
		}
		batch, err := store.GetFetchedFullTextArticles(afterID, retrimPageSize)
		if err != nil {
			return scanned, changed, fmt.Errorf("read full-text articles after %d: %w", afterID, err)
		}
		if len(batch) == 0 {
			return scanned, changed, nil
		}
		for _, a := range batch {
			afterID = a.ID
			scanned++

			content := trimSurroundingBoilerplate(a.Content)
			linked := trimSurroundingBoilerplate(a.LinkedContent)
			if content == a.Content && linked == a.LinkedContent {
				continue
			}
			changed++
			if dryRun {
				continue
			}
			if err := store.UpdateArticleExtractedContent(a.ID, content, linked); err != nil {
				return scanned, changed, fmt.Errorf("rewrite article %d: %w", a.ID, err)
			}
		}
	}
}
