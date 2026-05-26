package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/output"
	"github.com/matthewjhunter/herald/internal/storage"
)

// fakeAI records which model calls happen, and in what order, so tests can
// assert the security check gates the downstream summarizer and curator.
type fakeAI struct {
	mu    sync.Mutex
	calls []string

	sec    *ai.SecurityResult
	secErr error
	sum    string
	sumErr error
	cur    *ai.CurationResult
	curErr error
}

func (f *fakeAI) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeAI) called(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, name)
}

func (f *fakeAI) firstCall() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[0]
}

func (f *fakeAI) SecurityCheck(_ context.Context, _ int64, _, _ string) (*ai.SecurityResult, error) {
	f.record("SecurityCheck")
	return f.sec, f.secErr
}

func (f *fakeAI) SummarizeArticle(_ context.Context, _ int64, _, _ string, _ int) (string, error) {
	f.record("SummarizeArticle")
	return f.sum, f.sumErr
}

func (f *fakeAI) CurateArticle(_ context.Context, _ int64, _, _ string, _ []string) (*ai.CurationResult, error) {
	f.record("CurateArticle")
	return f.cur, f.curErr
}

type readStateCall struct {
	interest *float64
	security *float64
	reason   *string
	flagged  *bool
}

// fakeStore records the persistence calls screenAndScoreArticle makes.
type fakeStore struct {
	mu sync.Mutex

	summary    *storage.ArticleSummary // returned by GetArticleSummary
	summaryErr error

	cacheCalled   bool
	cachedSummary string
	readStates    []readStateCall
	retries       int
}

func (f *fakeStore) GetArticleSummary(_, _ int64) (*storage.ArticleSummary, error) {
	return f.summary, f.summaryErr
}

func (f *fakeStore) UpdateArticleAISummary(_, _ int64, summary string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cacheCalled = true
	f.cachedSummary = summary
	return nil
}

func (f *fakeStore) UpdateReadState(_, _ int64, _ bool, interest, security *float64, reason *string, flagged *bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readStates = append(f.readStates, readStateCall{interest, security, reason, flagged})
	return nil
}

func (f *fakeStore) IncrementAIRetries(_, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retries++
	return nil
}

func testConfig() *storage.Config {
	cfg := &storage.Config{}
	cfg.Thresholds.SecurityMediumScore = 4
	cfg.Thresholds.SecurityScore = 7
	cfg.Summarization.MaxSummaryLength = 200
	cfg.Preferences.Keywords = []string{"go"}
	return cfg
}

func discardFormatter() *output.Formatter {
	return output.NewFormatterWithWriters(output.FormatText, io.Discard, io.Discard)
}

func runScreen(aiProc articleAI, store articleScoreStore) screenOutcome {
	return screenAndScoreArticle(
		context.Background(), aiProc, store, discardFormatter(),
		testConfig(), 1, storage.Article{ID: 1, Title: "T", Content: "body"}, "body content",
	)
}

// The core #90 regression: a passing article is screened BEFORE any downstream
// model call, then summarized and curated.
func TestScreenAndScore_PassRunsSecurityFirstThenScores(t *testing.T) {
	f := &fakeAI{
		sec: &ai.SecurityResult{Safe: true, Score: 9},
		sum: "a usable summary",
		cur: &ai.CurationResult{InterestScore: 8},
	}
	s := &fakeStore{}

	out := runScreen(f, s)

	if !out.scored {
		t.Fatal("expected scored=true for a passing article")
	}
	if out.aiSummary != "a usable summary" {
		t.Errorf("aiSummary = %q, want %q", out.aiSummary, "a usable summary")
	}
	if got := f.firstCall(); got != "SecurityCheck" {
		t.Errorf("first AI call = %q, want SecurityCheck to gate the pipeline", got)
	}
	if !f.called("SummarizeArticle") || !f.called("CurateArticle") {
		t.Errorf("passing article should be summarized and curated; calls=%v", f.calls)
	}
	if !s.cacheCalled || s.cachedSummary != "a usable summary" {
		t.Errorf("expected summary to be cached; cacheCalled=%v cached=%q", s.cacheCalled, s.cachedSummary)
	}
	if len(s.readStates) != 1 || s.readStates[0].interest == nil || *s.readStates[0].interest != 8 {
		t.Errorf("expected one read-state write with interest 8; got %+v", s.readStates)
	}
}

// A hard-blocked article must never reach the summarizer or curator, and no
// summary may be cached for it.
func TestScreenAndScore_HardBlockSkipsDownstream(t *testing.T) {
	for _, tc := range []struct {
		name string
		sec  *ai.SecurityResult
	}{
		{"unsafe", &ai.SecurityResult{Safe: false, Score: 9}},
		{"below medium threshold", &ai.SecurityResult{Safe: true, Score: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAI{sec: tc.sec}
			s := &fakeStore{}

			out := runScreen(f, s)

			if out.scored {
				t.Error("hard-blocked article must not be scored")
			}
			if f.called("SummarizeArticle") {
				t.Error("hard-blocked article must NOT be summarized")
			}
			if f.called("CurateArticle") {
				t.Error("hard-blocked article must NOT be curated")
			}
			if s.cacheCalled {
				t.Error("no summary may be cached for a blocked article")
			}
			if len(s.readStates) != 1 || s.readStates[0].flagged != nil {
				t.Errorf("expected one read-state write with flagged=nil; got %+v", s.readStates)
			}
		})
	}
}

// A medium-flagged article (passes lower but not full threshold) is flagged for
// audit and must NOT be summarized or curated.
func TestScreenAndScore_MediumFlagSkipsDownstream(t *testing.T) {
	f := &fakeAI{sec: &ai.SecurityResult{Safe: true, Score: 5}}
	s := &fakeStore{}

	out := runScreen(f, s)

	if out.scored {
		t.Error("medium-flagged article must not be scored")
	}
	if f.called("SummarizeArticle") || f.called("CurateArticle") {
		t.Errorf("medium-flagged article must skip downstream models; calls=%v", f.calls)
	}
	if len(s.readStates) != 1 || s.readStates[0].flagged == nil || !*s.readStates[0].flagged {
		t.Errorf("expected one read-state write with flagged=true; got %+v", s.readStates)
	}
}

// A failed security check re-queues the article and runs nothing downstream.
func TestScreenAndScore_SecurityErrorRetriesAndSkips(t *testing.T) {
	f := &fakeAI{secErr: errors.New("model unreachable")}
	s := &fakeStore{}

	out := runScreen(f, s)

	if out.scored {
		t.Error("article must not be scored when the security check errors")
	}
	if f.called("SummarizeArticle") || f.called("CurateArticle") {
		t.Errorf("no downstream model calls on security error; calls=%v", f.calls)
	}
	if s.retries != 1 {
		t.Errorf("expected one retry increment, got %d", s.retries)
	}
	if len(s.readStates) != 0 {
		t.Errorf("no read-state write on transient security error; got %+v", s.readStates)
	}
}

// A passing article whose summary is already cached must not be re-summarized,
// but is still curated and scored.
func TestScreenAndScore_UsesCachedSummary(t *testing.T) {
	f := &fakeAI{
		sec: &ai.SecurityResult{Safe: true, Score: 9},
		cur: &ai.CurationResult{InterestScore: 6},
	}
	s := &fakeStore{summary: &storage.ArticleSummary{AISummary: "cached summary"}}

	out := runScreen(f, s)

	if !out.scored {
		t.Fatal("expected scored=true")
	}
	if out.aiSummary != "cached summary" {
		t.Errorf("aiSummary = %q, want cached summary", out.aiSummary)
	}
	if f.called("SummarizeArticle") {
		t.Error("should reuse the cached summary, not call SummarizeArticle")
	}
	if s.cacheCalled {
		t.Error("should not re-cache an existing summary")
	}
}
