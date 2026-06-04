package pipeline

import (
	"context"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

// clusterCohortLimit caps how many recent ungrouped articles the cluster stage
// considers in one pass. Newest-first, so fresh breaking news is never starved.
const clusterCohortLimit = 500

// drainBatch is how many articles each backfill stage pulls per query.
const drainBatch = 100

// RunFresh processes the articles fetched this cycle through every stage, in
// order, threading each stage's output into the next. The set is naturally
// bounded by the fetch size, so there is no per-stage limit. An article that
// cannot advance a stage (blocked, failed, skipped) is simply left in its
// current database state and picked up later by RunBackfill — there is no
// separate bookkeeping. Curation runs on every security-passed article
// (it does not need a summary); embedding runs on the summarized set so the
// cluster cohort is fully prepared.
// It returns the number of articles that passed the security screen this cycle
// (the daemon's "processed" metric).
func (s *Stage) RunFresh(ctx context.Context, fresh []storage.Article) (int, error) {
	passed := s.Security(ctx, fresh)
	summarized := s.Summarize(ctx, passed)
	s.Curate(ctx, passed)
	s.Embed(ctx, summarized)
	return len(passed), s.clusterRecent(ctx)
}

// RunBackfill drains pending work left by prior cycles — articles stranded
// between stages by transient failures or a backend that was down when they
// were fetched. Each stage pulls its own state-driven query in batches,
// newest-first. The whole backfill is skipped when the LLM backend is
// unavailable; the fresh path's per-stage skips and this top-level check keep an
// outage from spinning.
func (s *Stage) RunBackfill(ctx context.Context) error {
	if !s.AI.BackendAvailable() {
		s.Formatter.Warning("pipeline: skipping backfill for user %d — AI backend unavailable (breaker open)", s.UserID)
		return nil
	}

	if err := s.drain(
		func(limit int) ([]storage.Article, error) { return s.Store.GetUnscoredArticlesForUser(s.UserID, limit) },
		func(arts []storage.Article) { s.Security(ctx, arts) },
	); err != nil {
		return err
	}
	if err := s.drain(
		func(limit int) ([]storage.Article, error) {
			return s.Store.GetUnsummarizedScoredArticles(s.UserID, s.Cfg.Thresholds.SecurityScore, limit)
		},
		func(arts []storage.Article) { s.Summarize(ctx, arts) },
	); err != nil {
		return err
	}
	if err := s.drain(
		func(limit int) ([]storage.Article, error) {
			return s.Store.GetUnscoredCurationArticles(s.UserID, s.Cfg.Thresholds.SecurityScore, limit)
		},
		func(arts []storage.Article) { s.Curate(ctx, arts) },
	); err != nil {
		return err
	}
	if s.Embedder != nil {
		if err := s.drain(
			func(limit int) ([]storage.Article, error) {
				return s.Store.GetArticlesWithoutEmbeddings(s.Embedder.Model(), limit)
			},
			func(arts []storage.Article) { s.Embed(ctx, arts) },
		); err != nil {
			return err
		}
	}

	return s.clusterRecent(ctx)
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
	if s.Embedder == nil {
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
