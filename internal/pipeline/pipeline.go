package pipeline

import (
	"context"
	"sync"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/output"
	"github.com/matthewjhunter/herald/internal/sanitize"
	"github.com/matthewjhunter/herald/internal/storage"
)

// AI is the subset of *ai.AIProcessor the pipeline stages call. It is declared
// on the consumer side so stages can be unit-tested with a fake that records
// which model calls happen and injects verdicts and errors. BackendAvailable
// lets a stage skip itself with a single log line when the circuit breaker is
// open, instead of attempting (and logging) one blocked call per article.
type AI interface {
	SecurityCheck(ctx context.Context, title, content string) (*ai.SecurityResult, error)
	SummarizeArticle(ctx context.Context, userID int64, title, content string, maxSummaryLength int) (string, error)
	CurateArticle(ctx context.Context, userID int64, title, content string, keywords []string) (*ai.CurationResult, error)
	GenerateGroupSummary(ctx context.Context, userID int64, topic string, articles []ai.GroupSummaryInput) (*ai.GroupSummaryResult, error)
	RefineGroupTopic(ctx context.Context, userID int64, groupSummary string) (string, error)
	BackendAvailable() bool
}

// Embedder is the subset of *ai.GroupMatcher the embed stage calls. EmbedRecord
// returns (nil, nil) — not an error — when the body is too short to embed
// meaningfully; the embed stage treats that as a deterministic skip.
type Embedder interface {
	Model() string
	EmbedRecord(ctx context.Context, fields []embedding.Field, body string) ([]float32, error)
}

// Stage runs the staged AI pipeline for a single user. Construct one per user
// per cycle. Embedding is global (keyed by article + model, not per user), so
// the embed stage is idempotent across users sharing a feed.
type Stage struct {
	Store     storage.Store
	AI        AI
	Embedder  Embedder // nil when embedding is not configured; embed/cluster stages no-op
	Cfg       *storage.Config
	Formatter *output.Formatter
	UserID    int64

	// BuildEmbedInput builds the (fields, body) record embedded for an article.
	// Injected so the pipeline package does not import the root herald package
	// (which imports pipeline) — set to herald.BuildArticleEmbedInput at wiring.
	BuildEmbedInput func(storage.Article) ([]embedding.Field, string)
}

// maxParallel is the per-stage concurrency bound (Ollama.MaxParallel, floored
// at 1) — the same limit the old per-article pipeline used.
func (s *Stage) maxParallel() int {
	return max(s.Cfg.Ollama.MaxParallel, 1)
}

// mapArticles runs fn over each input article with bounded concurrency and
// returns, in input order, the articles for which fn returned a non-nil result
// (i.e. those that advanced to the next stage). fn must be safe for concurrent
// use; it only touches the store and the AI backend, both of which are.
func (s *Stage) mapArticles(ctx context.Context, in []storage.Article, fn func(context.Context, storage.Article) *storage.Article) []storage.Article {
	results := make([]*storage.Article, len(in))
	sem := make(chan struct{}, s.maxParallel())
	var wg sync.WaitGroup
	for i, article := range in {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, article storage.Article) {
			defer func() { <-sem; wg.Done() }()
			results[i] = fn(ctx, article)
		}(i, article)
	}
	wg.Wait()

	out := make([]storage.Article, 0, len(in))
	for _, r := range results {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}

// articleContent assembles the text fed to the AI stages: the article's content
// (falling back to its RSS summary), with any separately-fetched full text
// appended. It returns "" only when there is no base content at all — the
// caller treats that as a skip. Mirrors the assembly the per-article pipeline
// did inline before the staged refactor.
func articleContent(a storage.Article) string {
	content := a.Content
	if content == "" {
		content = a.Summary
	}
	if content == "" {
		return ""
	}
	if a.LinkedContent != "" {
		content = content + "\n\n" + a.LinkedContent
	}
	// Sanitize so every AI stage judges exactly what the web view renders:
	// scripts and event handlers stripped — which also stops the security model
	// from flagging legitimate embedded widgets (Rumble/Twitter players) as
	// malicious — while links and visible prose are preserved (#121). The raw
	// HTML stays in storage as the source of truth; this is a per-read view.
	// May return "" when the body was nothing but markup, which the caller
	// treats as a skip.
	return sanitize.HTML(content)
}
