package pipeline

import (
	"context"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

// clusterCohortLimit caps how many recent ungrouped articles the cluster stage
// considers in one pass. Newest-first, so fresh breaking news is never starved.
const clusterCohortLimit = 500

// drainBatch is how many articles each stage pulls per query.
const drainBatch = 100

// Run executes the full staged pipeline for the user: it drives each stage from
// its own state-driven query, in order, so any pending article advances one
// stage at a time. An article fetched this cycle flows through all five stages
// in a single call — security marks it scored, the summarize query then sees it,
// then curate, then embed, then cluster — and every query is newest-first, so
// fresh articles are processed ahead of older backlog. Work left by prior cycles
// (stranded by a transient failure or a backend that was down) drains the same
// way. The whole run is skipped when the LLM backend is unavailable; that check
// plus the per-stage skips keep an outage from spinning.
// Run returns the number of articles that newly passed the security screen this
// call — the daemon's "processed" metric.
func (s *Stage) Run(ctx context.Context) (int, error) {
	if !s.AI.BackendAvailable() {
		s.Formatter.Warning("pipeline: skipping run for user %d — AI backend unavailable (breaker open)", s.UserID)
		return 0, nil
	}

	processed := 0
	if err := s.drain(
		func(limit int) ([]storage.Article, error) { return s.Store.GetUnscoredArticlesForUser(s.UserID, limit) },
		func(arts []storage.Article) { processed += len(s.Security(ctx, arts)) },
	); err != nil {
		return processed, err
	}
	if err := s.drain(
		func(limit int) ([]storage.Article, error) {
			return s.Store.GetUnsummarizedScoredArticles(s.UserID, s.Cfg.Thresholds.SecurityScore, limit)
		},
		func(arts []storage.Article) { s.Summarize(ctx, arts) },
	); err != nil {
		return processed, err
	}
	if err := s.drain(
		func(limit int) ([]storage.Article, error) {
			return s.Store.GetUnscoredCurationArticles(s.UserID, s.Cfg.Thresholds.SecurityScore, limit)
		},
		func(arts []storage.Article) { s.Curate(ctx, arts) },
	); err != nil {
		return processed, err
	}
	if s.Embedder != nil {
		if err := s.drain(
			func(limit int) ([]storage.Article, error) {
				return s.Store.GetArticlesWithoutEmbeddings(s.Embedder.Model(), limit)
			},
			func(arts []storage.Article) { s.Embed(ctx, arts) },
		); err != nil {
			return processed, err
		}
	}

	return processed, s.clusterRecent(ctx)
}

// drain repeatedly fetches a batch and processes the articles it has not yet
// attempted this cycle, stopping when a fetch yields nothing new. Attempting
// each article at most once per cycle bounds the work and, crucially, prevents
// an infinite loop when a stage cannot advance its input — a transient failure
// or a breaker that opens mid-drain (the stage then no-ops, the articles stay
// in the queue, and the next fetch returns the same now-attempted set).
func (s *Stage) drain(fetch func(limit int) ([]storage.Article, error), process func([]storage.Article)) error {
	attempted := make(map[int64]bool)
	for {
		arts, err := fetch(drainBatch)
		if err != nil {
			return err
		}
		if len(arts) == 0 {
			return nil
		}
		fresh := arts[:0:0]
		for _, a := range arts {
			if !attempted[a.ID] {
				attempted[a.ID] = true
				fresh = append(fresh, a)
			}
		}
		if len(fresh) == 0 {
			return nil // no progress possible — every article already attempted
		}
		process(fresh)
		if len(arts) < drainBatch {
			return nil
		}
	}
}

// clusterRecent runs the cluster stage over the user's recent ungrouped,
// embedded articles — the same cohort for both the fresh and backfill paths, so
// freshly-embedded articles cluster alongside late-arriving siblings from
// earlier cycles. No-op when embedding is unconfigured.
func (s *Stage) clusterRecent(ctx context.Context) error {
	if s.Embedder == nil || !s.Cfg.Grouping.Enabled {
		return nil
	}
	since := time.Now().Add(-s.recencyWindow())
	cohort, err := s.Store.GetUngroupedEmbeddedArticles(s.UserID, s.Embedder.Model(), s.Cfg.Thresholds.SecurityScore, since, clusterCohortLimit)
	if err != nil {
		return err
	}
	return s.Cluster(ctx, cohort)
}

func (s *Stage) recencyWindow() time.Duration {
	h := s.Cfg.Grouping.RecencyWindowHours
	if h <= 0 {
		h = 48
	}
	return time.Duration(h) * time.Hour
}
