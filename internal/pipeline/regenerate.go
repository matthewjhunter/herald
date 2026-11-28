package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
)

// Regeneration fills in one model's output across articles that were already
// processed by another model, or by a model nobody recorded. It is driven by
// hand (`herald regenerate`), not by the daemon: it is a one-off pass over the
// whole corpus, and it competes with live scoring for the same backend.
//
// Both passes are resumable and idempotent. The queue is "articles with no row
// from this model", so a run that is interrupted, or whose calls fail, leaves
// those articles for the next run and never repeats the ones that are done.

// Regen configures one regeneration run.
type Regen struct {
	// Model is the model whose rows the run fills in. It must be the model
	// the backend is actually configured to use: the run asks the configured
	// backend and refuses a completion attributed to anything else.
	Model string
	// Limit caps how many articles the run attempts; 0 means all of them.
	Limit int
	// Progress, when set, is called after every batch with the totals so far.
	Progress func(RegenResult)
}

// RegenResult counts a run: Attempted articles were taken from the queue,
// Recorded ones now have a row from the model (a result or a skip marker).
// The difference is calls that failed and will be picked up by a later run.
type RegenResult struct {
	Attempted int
	Recorded  int
}

// RegenerateSummaries summarizes every security-passed article that has no
// summary row from r.Model. Each new summary becomes the article's current
// one; whatever it replaces stays in the history.
func (s *Stage) RegenerateSummaries(ctx context.Context, r Regen) (RegenResult, error) {
	return s.regenerate(ctx, r,
		func(before int64, limit int) ([]storage.Article, error) {
			return s.Store.GetArticlesLackingSummaryBy(r.Model, s.Cfg.Thresholds.MaxSecurityThreat, before, limit)
		},
		func(ctx context.Context, a storage.Article) (*storage.Article, error) {
			return s.summarize(ctx, a, r.Model)
		})
}

// RegenerateScreens runs r.Model's security screen over every article that
// already has a verdict and no screen-history row from that model. The scans
// are shadows: each is recorded in the history for comparison and none of
// them changes the verdict that gates its article, in either direction.
// Acting on what the comparison shows is a separate decision.
func (s *Stage) RegenerateScreens(ctx context.Context, r Regen) (RegenResult, error) {
	return s.regenerate(ctx, r,
		func(before int64, limit int) ([]storage.Article, error) {
			return s.Store.GetArticlesLackingScreenBy(r.Model, before, limit)
		},
		func(ctx context.Context, a storage.Article) (*storage.Article, error) {
			return s.shadowScreen(ctx, a, r.Model)
		})
}

// regenerate walks a queue by descending article id. The cursor, not the
// queue's own emptiness, is what ends the run: an article whose call failed
// still lacks its row and would otherwise be fetched again forever.
func (s *Stage) regenerate(ctx context.Context, r Regen,
	fetch func(beforeID int64, limit int) ([]storage.Article, error),
	one func(context.Context, storage.Article) (*storage.Article, error),
) (RegenResult, error) {
	var res RegenResult
	if r.Model == "" {
		return res, errors.New("regenerate: no model given")
	}
	batch := 4 * s.maxParallel()
	before := int64(math.MaxInt64)
	for r.Limit <= 0 || res.Attempted < r.Limit {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !s.AI.BackendAvailable() {
			return res, fmt.Errorf("regenerate: %w", ai.ErrBackendUnavailable)
		}
		n := batch
		if r.Limit > 0 {
			n = min(n, r.Limit-res.Attempted)
		}
		arts, err := fetch(before, n)
		if err != nil {
			return res, err
		}
		if len(arts) == 0 {
			break
		}
		before = arts[len(arts)-1].ID

		// The first hard error stops the run after this batch; the calls
		// already in flight finish and are recorded or not on their merits.
		var (
			mu      sync.Mutex
			hardErr error
		)
		done := s.mapArticles(ctx, arts, func(ctx context.Context, a storage.Article) *storage.Article {
			out, err := one(ctx, a)
			if err != nil {
				mu.Lock()
				if hardErr == nil {
					hardErr = err
				}
				mu.Unlock()
			}
			return out
		})
		res.Attempted += len(arts)
		res.Recorded += len(done)
		if r.Progress != nil {
			r.Progress(res)
		}
		if hardErr != nil {
			return res, fmt.Errorf("regenerate: %w", hardErr)
		}
	}
	return res, nil
}

// shadowScreen scans one article with the configured screen and records the
// result as a shadow row. It returns the article when a row was written.
func (s *Stage) shadowScreen(ctx context.Context, article storage.Article, wantModel string) (*storage.Article, error) {
	content := articleContent(article)
	if content == "" {
		// It carries a verdict, so it had content once; nothing to scan now.
		s.Formatter.Warning("shadow screen: article %d has no content, skipping", article.ID)
		return nil, nil
	}
	res, err := s.AI.SecurityCheck(ctx, article.Title, content)
	if err != nil {
		s.Formatter.Warning("shadow screen failed for article %d: %v", article.ID, err)
		return nil, nil
	}
	if res.Model != wantModel {
		return nil, fmt.Errorf("%w: article %d was screened by %q, not %q", errModelMismatch, article.ID, res.Model, wantModel)
	}
	contentHash := storage.ContentHash(content)
	for _, c := range res.Chunks {
		s.recordModelOutput(storage.ModelOutput{
			Kind: storage.ModelOutputScreen, ArticleID: article.ID, ChunkIndex: c.Index, ChunkCount: len(res.Chunks),
			Model: res.Model, PromptHash: res.PromptHash, ContentHash: contentHash,
			Response: c.Response, Parsed: screenParsed(c.Finding.Threat, c.Finding.Category, c.Finding.Verified),
			Via: storage.ModelOutputViaSync, Trainable: true,
		})
	}
	// Flagged means what it means on a live verdict: borderline, between the
	// pass ceiling and the hard block.
	pass, border := s.securityThresholds()
	if err := s.Store.RecordArticleScreen(storage.ArticleScreen{
		ArticleID: article.ID, Model: res.Model, PromptHash: res.PromptHash, Via: storage.ModelOutputViaSync,
		Threat: res.Threat, Category: res.Category, Verified: res.Verified,
		Flagged:   res.Threat > pass && res.Threat <= border,
		Effective: false,
	}); err != nil {
		s.Formatter.Warning("shadow screen: could not record scan for article %d: %v", article.ID, err)
		return nil, nil
	}
	return &article, nil
}
