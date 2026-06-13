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

// RunSecurity screens every not-yet-screened article exactly once, globally. The
// security verdict is a property of the content, shared by all subscribers
// (#141), so this runs once per cycle rather than once per user — and it checks
// the breaker a single time, draining the global queue newest-first. Self-skips
// with one log line when the backend is down, instead of one blocked call per
// article (#111). Returns the number of articles that newly passed the screen
// this call — the daemon's "processed" metric.
func (s *Stage) RunSecurity(ctx context.Context) (int, error) {
	if !s.AI.BackendAvailable() {
		s.Formatter.Warning("pipeline: skipping security pass — AI backend unavailable (breaker open)")
		return 0, nil
	}
	processed := 0
	err := s.drain(
		func(limit int) ([]storage.Article, error) { return s.Store.GetUnscreenedArticles(limit) },
		func(arts []storage.Article) { processed += len(s.Security(ctx, arts)) },
	)
	return processed, err
}

// RunSummaries summarizes every security-passed article that lacks a cached
// summary, exactly once, globally. Like the security verdict (#141), the
// summary is a property of the article, shared by all subscribers (#162), so
// this runs once per cycle rather than once per user — one breaker check,
// draining the global queue newest-first. Returns the number of articles
// attempted this call.
func (s *Stage) RunSummaries(ctx context.Context) (int, error) {
	if !s.AI.BackendAvailable() {
		s.Formatter.Warning("pipeline: skipping summarize pass — AI backend unavailable (breaker open)")
		return 0, nil
	}
	processed := 0
	err := s.drain(
		func(limit int) ([]storage.Article, error) {
			return s.Store.GetUnsummarizedScoredArticles(s.Cfg.Thresholds.SecurityScore, limit)
		},
		func(arts []storage.Article) { processed += len(s.Summarize(ctx, arts)) },
	)
	return processed, err
}

// Run executes the per-user pipeline: it drives each stage from its own
// state-driven query, in order, so any pending article advances one stage at a
// time. Security screening and summarization are NOT here — they are the
// global RunSecurity and RunSummaries passes, which run once per cycle before
// the per-user pipelines. This pipeline starts at curation, reading the
// article-level security verdict to decide which articles it scores for this
// user. Every query is newest-first, so fresh articles are processed ahead of
// older backlog; work left by prior cycles drains the same way. The whole run
// is skipped when the LLM backend is unavailable.
func (s *Stage) Run(ctx context.Context) error {
	if !s.AI.BackendAvailable() {
		s.Formatter.Warning("pipeline: skipping run for user %d — AI backend unavailable (breaker open)", s.UserID)
		return nil
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
