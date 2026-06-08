package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/output"
	"github.com/matthewjhunter/herald/internal/storage"
)

// fakeAI implements the AI interface with injectable verdicts/errors and call
// counts, so the stages can be tested without a real model backend.
type fakeAI struct {
	available   bool
	securityFn  func(title, content string) (*ai.SecurityResult, error)
	summarizeFn func(title, content string) (string, error)
	curateFn    func(title, content string) (*ai.CurationResult, error)

	groupSummaryFn func(topic string, articles []ai.GroupSummaryInput) (*ai.GroupSummaryResult, error)
	refineTopicFn  func(summary string) (string, error)

	mu                           sync.Mutex
	secCalls, sumCalls, curCalls int
}

func (f *fakeAI) BackendAvailable() bool { return f.available }

func (f *fakeAI) GenerateGroupSummary(_ context.Context, _ int64, topic string, articles []ai.GroupSummaryInput) (*ai.GroupSummaryResult, error) {
	if f.groupSummaryFn != nil {
		return f.groupSummaryFn(topic, articles)
	}
	return &ai.GroupSummaryResult{Headline: "Headline", Summary: "Summary of the group"}, nil
}

func (f *fakeAI) RefineGroupTopic(_ context.Context, _ int64, summary string) (string, error) {
	if f.refineTopicFn != nil {
		return f.refineTopicFn(summary)
	}
	return "refined topic", nil
}

func (f *fakeAI) SecurityCheck(_ context.Context, title, content string) (*ai.SecurityResult, error) {
	f.mu.Lock()
	f.secCalls++
	f.mu.Unlock()
	if f.securityFn != nil {
		return f.securityFn(title, content)
	}
	return &ai.SecurityResult{Safe: true, Score: 9}, nil
}

func (f *fakeAI) SummarizeArticle(_ context.Context, _ int64, title, content string, _ int) (string, error) {
	f.mu.Lock()
	f.sumCalls++
	f.mu.Unlock()
	if f.summarizeFn != nil {
		return f.summarizeFn(title, content)
	}
	return "a fine summary", nil
}

func (f *fakeAI) CurateArticle(_ context.Context, _ int64, title, content string, _ []string) (*ai.CurationResult, error) {
	f.mu.Lock()
	f.curCalls++
	f.mu.Unlock()
	if f.curateFn != nil {
		return f.curateFn(title, content)
	}
	return &ai.CurationResult{InterestScore: 7.5}, nil
}

func newHarness(t *testing.T, fake *fakeAI) (*Stage, storage.Store, int64) {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
	if err := store.SubscribeUserToFeed(1, feedID); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	cfg := storage.DefaultConfig()
	cfg.Ollama.MaxParallel = 4
	st := &Stage{
		Store:     store,
		AI:        fake,
		Cfg:       cfg,
		Formatter: output.NewFormatterWithWriters(output.FormatText, io.Discard, io.Discard),
		UserID:    1,
	}
	return st, store, feedID
}

func seed(t *testing.T, store storage.Store, feedID int64, guid, content string) storage.Article {
	t.Helper()
	now := time.Now()
	a := &storage.Article{FeedID: feedID, GUID: guid, Title: guid,
		URL: "https://example.com/" + guid, Content: content, PublishedDate: &now}
	id, err := store.AddArticle(a)
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}
	a.ID = id
	return *a
}

func ids(arts []storage.Article) []int64 {
	out := make([]int64, len(arts))
	for i, a := range arts {
		out[i] = a.ID
	}
	return out
}

func TestSecurityStageGatesAndMarks(t *testing.T) {
	fake := &fakeAI{available: true}
	fake.securityFn = func(title, _ string) (*ai.SecurityResult, error) {
		switch {
		case strings.HasPrefix(title, "block"):
			return &ai.SecurityResult{Safe: true, Score: 2}, nil // hard block
		case strings.HasPrefix(title, "medium"):
			return &ai.SecurityResult{Safe: true, Score: 5}, nil // between 4 and 7
		default:
			return &ai.SecurityResult{Safe: true, Score: 9}, nil
		}
	}
	st, store, feedID := newHarness(t, fake)

	pass := seed(t, store, feedID, "pass", strings.Repeat("x", 500))
	block := seed(t, store, feedID, "block", strings.Repeat("x", 500))
	medium := seed(t, store, feedID, "medium", strings.Repeat("x", 500))
	short := seed(t, store, feedID, "short", "tiny")
	empty := seed(t, store, feedID, "empty", "")

	passed := st.Security(context.Background(), []storage.Article{pass, block, medium, short, empty})

	if got := ids(passed); len(got) != 1 || got[0] != pass.ID {
		t.Fatalf("expected only %d to pass, got %v", pass.ID, got)
	}
	// The passing article awaits curation; nothing was summarized.
	if fake.sumCalls != 0 {
		t.Fatalf("security stage must not summarize, got %d summarize calls", fake.sumCalls)
	}
	cur, _ := store.GetUnscoredCurationArticles(1, 7.0, 10)
	if len(cur) != 1 || cur[0].ID != pass.ID {
		t.Fatalf("expected article %d awaiting curation, got %v", pass.ID, ids(cur))
	}
	// All five left the unscored (security) queue: pass is scored, the rest terminal.
	unscored, _ := store.GetUnscreenedArticles(10)
	if len(unscored) != 0 {
		t.Fatalf("expected unscored queue empty, got %v", ids(unscored))
	}
}

func TestSecurityRetryBudget(t *testing.T) {
	// A backend-unavailable error must NOT burn the retry budget (#100): the
	// article keeps coming back. A genuine verdict failure does, so after three
	// it drops out of the unscored queue.
	t.Run("backend unavailable does not increment", func(t *testing.T) {
		fake := &fakeAI{available: true, securityFn: func(string, string) (*ai.SecurityResult, error) {
			return nil, ai.ErrBackendUnavailable
		}}
		st, store, feedID := newHarness(t, fake)
		art := seed(t, store, feedID, "x", strings.Repeat("x", 500))
		for range 4 {
			st.Security(context.Background(), []storage.Article{art})
		}
		unscored, _ := store.GetUnscreenedArticles(10)
		if len(unscored) != 1 {
			t.Fatalf("backend-unavailable article should still be retryable, got %v", ids(unscored))
		}
	})

	t.Run("verdict failure burns the budget", func(t *testing.T) {
		fake := &fakeAI{available: true, securityFn: func(string, string) (*ai.SecurityResult, error) {
			return nil, errors.New("unparseable verdict")
		}}
		st, store, feedID := newHarness(t, fake)
		art := seed(t, store, feedID, "x", strings.Repeat("x", 500))
		for range 3 {
			st.Security(context.Background(), []storage.Article{art})
		}
		unscored, _ := store.GetUnscreenedArticles(10)
		if len(unscored) != 0 {
			t.Fatalf("article should drop out after 3 verdict failures, got %v", ids(unscored))
		}
	})
}

func TestSecurityStageSkipsWhenBreakerOpen(t *testing.T) {
	fake := &fakeAI{available: false}
	st, store, feedID := newHarness(t, fake)
	art := seed(t, store, feedID, "x", strings.Repeat("x", 500))

	passed := st.Security(context.Background(), []storage.Article{art})
	if len(passed) != 0 {
		t.Fatalf("expected no articles to advance with breaker open, got %v", ids(passed))
	}
	if fake.secCalls != 0 {
		t.Fatalf("expected zero security calls with breaker open, got %d", fake.secCalls)
	}
	unscored, _ := store.GetUnscreenedArticles(10)
	if len(unscored) != 1 {
		t.Fatalf("article should remain unscored after a skipped stage, got %v", ids(unscored))
	}
}

func TestArticleContentSanitizes(t *testing.T) {
	a := storage.Article{
		Content:       `<p>Breaking news body.</p><script>steal()</script>`,
		LinkedContent: `<a href="https://example.com" onclick="x()">more</a>`,
	}
	got := articleContent(a)
	for _, bad := range []string{"<script", "steal()", "onclick", "x()"} {
		if strings.Contains(got, bad) {
			t.Errorf("articleContent should strip %q, got: %s", bad, got)
		}
	}
	for _, want := range []string{"Breaking news body.", "https://example.com", "more"} {
		if !strings.Contains(got, want) {
			t.Errorf("articleContent should keep %q, got: %s", want, got)
		}
	}
}

func TestSecurityStageScansSanitizedContent(t *testing.T) {
	// The security model must judge the sanitized, user-visible content — not raw
	// feed HTML — so embedded widget scripts don't read as malicious code (#121).
	var seen string
	fake := &fakeAI{available: true, securityFn: func(_, content string) (*ai.SecurityResult, error) {
		seen = content
		return &ai.SecurityResult{Safe: true, Score: 9}, nil
	}}
	st, store, feedID := newHarness(t, fake)
	body := `<script src="https://rumble.com/widgets.js"></script><p>` + strings.Repeat("real news ", 60) + `</p>`
	a := seed(t, store, feedID, "x", body)

	st.Security(context.Background(), []storage.Article{a})

	if strings.Contains(seen, "<script") || strings.Contains(seen, "widgets.js") {
		t.Errorf("security model received unsanitized content: %s", seen)
	}
	if !strings.Contains(seen, "real news") {
		t.Errorf("security model should still see the visible prose, got: %s", seen)
	}
}

func TestSummarizeStage(t *testing.T) {
	st, store, feedID := newHarness(t, &fakeAI{available: true})
	mustPass := func(a storage.Article) {
		// Put the article into the security-passed state the summarize stage expects.
		if err := store.ScreenArticleSecurity(a.ID, 9, "ok", false); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("caches a good summary", func(t *testing.T) {
		st.AI.(*fakeAI).summarizeFn = func(string, string) (string, error) { return "good summary", nil }
		a := seed(t, store, feedID, "good", "the full article body is here")
		mustPass(a)
		out := st.Summarize(context.Background(), []storage.Article{a})
		if len(out) != 1 {
			t.Fatalf("expected article to advance, got %v", ids(out))
		}
		got, _ := store.GetArticleSummary(1, a.ID)
		if got == nil || got.AISummary != "good summary" {
			t.Fatalf("summary not cached: %+v", got)
		}
	})

	t.Run("garbled summary is a transient skip", func(t *testing.T) {
		st.AI.(*fakeAI).summarizeFn = func(string, string) (string, error) { return "### Assistant: junk", nil }
		a := seed(t, store, feedID, "garbled", "the full article body is here")
		mustPass(a)
		out := st.Summarize(context.Background(), []storage.Article{a})
		if len(out) != 0 {
			t.Fatalf("garbled summary must not advance the article, got %v", ids(out))
		}
		if got, _ := store.GetArticleSummary(1, a.ID); got != nil {
			t.Fatalf("garbled summary must not be cached, got %+v", got)
		}
	})

	t.Run("summary longer than content is a deterministic skip", func(t *testing.T) {
		st.AI.(*fakeAI).summarizeFn = func(_, content string) (string, error) {
			return content + " and then a great deal more text than the source", nil
		}
		a := seed(t, store, feedID, "toolong", "short body")
		mustPass(a)
		out := st.Summarize(context.Background(), []storage.Article{a})
		if len(out) != 1 {
			t.Fatalf("deterministically-skipped article should still advance, got %v", ids(out))
		}
		// Marked skipped (summary row exists) so it won't be retried.
		got, _ := store.GetArticleSummary(1, a.ID)
		if got == nil {
			t.Fatal("expected a skip row recorded for the over-length summary")
		}
	})

	t.Run("already-summarized article passes through without a model call", func(t *testing.T) {
		a := seed(t, store, feedID, "cached", "body")
		mustPass(a)
		if err := store.UpdateArticleAISummary(1, a.ID, "preexisting"); err != nil {
			t.Fatal(err)
		}
		before := st.AI.(*fakeAI).sumCalls
		out := st.Summarize(context.Background(), []storage.Article{a})
		if len(out) != 1 {
			t.Fatalf("cached article should advance, got %v", ids(out))
		}
		if st.AI.(*fakeAI).sumCalls != before {
			t.Fatal("already-summarized article should not call the model")
		}
	})
}

func TestCurateStage(t *testing.T) {
	t.Run("records interest and preserves security", func(t *testing.T) {
		fake := &fakeAI{available: true, curateFn: func(string, string) (*ai.CurationResult, error) {
			return &ai.CurationResult{InterestScore: 8.0}, nil
		}}
		st, store, feedID := newHarness(t, fake)
		a := seed(t, store, feedID, "x", "body text")
		if err := store.ScreenArticleSecurity(a.ID, 9, "ok", false); err != nil {
			t.Fatal(err)
		}

		out := st.Curate(context.Background(), []storage.Article{a})
		if len(out) != 1 {
			t.Fatalf("expected article curated, got %v", ids(out))
		}
		// Left the curation queue and still counts as security-passed.
		cur, _ := store.GetUnscoredCurationArticles(1, 7.0, 10)
		if len(cur) != 0 {
			t.Fatalf("curated article should leave the curation queue, got %v", ids(cur))
		}
		scored, _ := store.GetUnsummarizedScoredArticles(1, 7.0, 10)
		if len(scored) != 1 {
			t.Fatalf("security score must survive curation, got %v", ids(scored))
		}
	})

	t.Run("processed event reports the persisted security score, not zero", func(t *testing.T) {
		fake := &fakeAI{available: true, curateFn: func(string, string) (*ai.CurationResult, error) {
			return &ai.CurationResult{InterestScore: 8.0}, nil
		}}
		st, store, feedID := newHarness(t, fake)
		var buf bytes.Buffer
		st.Formatter = output.NewFormatterWithWriters(output.FormatJSON, &buf, io.Discard)
		a := seed(t, store, feedID, "x", "body text")
		if err := store.ScreenArticleSecurity(a.ID, 9, "ok", false); err != nil {
			t.Fatal(err)
		}

		st.Curate(context.Background(), []storage.Article{a})

		var ev struct {
			Event         string  `json:"event"`
			SecurityScore float64 `json:"security_score"`
			InterestScore float64 `json:"interest_score"`
		}
		if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
			t.Fatalf("decode processed event: %v (out=%q)", err, buf.String())
		}
		if ev.Event != "article_processed" {
			t.Fatalf("event = %q, want article_processed", ev.Event)
		}
		// The curate stage must surface the verdict the security stage wrote,
		// not a 0 placeholder — the event feeds logs and notifications (#119).
		if ev.SecurityScore != 9 {
			t.Errorf("security_score = %v, want 9 (persisted verdict)", ev.SecurityScore)
		}
		if ev.InterestScore != 8 {
			t.Errorf("interest_score = %v, want 8", ev.InterestScore)
		}
	})

	t.Run("logs scored result at info level on success", func(t *testing.T) {
		fake := &fakeAI{available: true, curateFn: func(string, string) (*ai.CurationResult, error) {
			return &ai.CurationResult{InterestScore: 8.0}, nil
		}}
		st, store, feedID := newHarness(t, fake)
		var errBuf bytes.Buffer
		st.Formatter = output.NewFormatterWithWriters(output.FormatJSON, io.Discard, &errBuf)
		a := seed(t, store, feedID, "headline here", "body text")
		if err := store.ScreenArticleSecurity(a.ID, 9, "ok", false); err != nil {
			t.Fatal(err)
		}

		st.Curate(context.Background(), []storage.Article{a})

		// Success must be visible in the daemon log (stderr) with the actual
		// scores, not only as a structured stdout event.
		got := errBuf.String()
		if !strings.Contains(got, "Info:") {
			t.Errorf("expected an info-level success line, got: %q", got)
		}
		if !strings.Contains(got, "interest=8.0") || !strings.Contains(got, "security=9.0") {
			t.Errorf("info line missing actual scores, got: %q", got)
		}
	})

	t.Run("curation failure leaves the article for a later cycle", func(t *testing.T) {
		fake := &fakeAI{available: true, curateFn: func(string, string) (*ai.CurationResult, error) {
			return nil, errors.New("model timeout")
		}}
		st, store, feedID := newHarness(t, fake)
		a := seed(t, store, feedID, "x", "body text")
		if err := store.ScreenArticleSecurity(a.ID, 9, "ok", false); err != nil {
			t.Fatal(err)
		}
		out := st.Curate(context.Background(), []storage.Article{a})
		if len(out) != 0 {
			t.Fatalf("failed curation should not advance, got %v", ids(out))
		}
		cur, _ := store.GetUnscoredCurationArticles(1, 7.0, 10)
		if len(cur) != 1 {
			t.Fatalf("failed article should still await curation, got %v", ids(cur))
		}
	})
}
