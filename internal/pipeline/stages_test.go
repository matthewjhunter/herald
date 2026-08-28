package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/output"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/matthewjhunter/herald/internal/storagetest"
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

func (f *fakeAI) BackendAvailable() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available
}

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
	return &ai.SecurityResult{Threat: 1}, nil
}

func (f *fakeAI) SummarizeArticle(_ context.Context, title, content string, _ int) (string, error) {
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
	store, cleanup := storagetest.NewStore(t)
	t.Cleanup(cleanup)

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
			return &ai.SecurityResult{Threat: 8}, nil // hard block
		case strings.HasPrefix(title, "medium"):
			return &ai.SecurityResult{Threat: 5}, nil // between 4 and 7
		default:
			return &ai.SecurityResult{Threat: 1}, nil
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
	cur, _ := store.GetUnscoredCurationArticles(1, 3.0, 10)
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
	// The retry budget is spent at claim time now (#233): each RunSecurity cycle
	// claims the article once, incrementing its attempt. A genuine verdict failure
	// keeps that attempt (so after three cycles it drops out of the queue); a
	// backend-unavailable error refunds it (#100), so the article keeps coming
	// back. These drive RunSecurity (the real claim-based drain), not Security()
	// directly, since the claim is where the attempt is counted.
	t.Run("backend unavailable does not increment", func(t *testing.T) {
		fake := &fakeAI{available: true, securityFn: func(string, string) (*ai.SecurityResult, error) {
			return nil, ai.ErrBackendUnavailable
		}}
		st, store, feedID := newHarness(t, fake)
		seed(t, store, feedID, "x", strings.Repeat("x", 500))
		for range 4 {
			st.RunSecurity(context.Background()) //nolint:errcheck
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
		seed(t, store, feedID, "x", strings.Repeat("x", 500))
		for range 3 {
			st.RunSecurity(context.Background()) //nolint:errcheck
		}
		unscored, _ := store.GetUnscreenedArticles(10)
		if len(unscored) != 0 {
			t.Fatalf("article should drop out after 3 verdict failures, got %v", ids(unscored))
		}
	})
}

// TestSecurityStopsClaimingWhenBreakerTripsMidDrain guards the #233 interaction:
// the claim counts an attempt per article, so if the drain kept claiming after
// the breaker opens mid-batch (while Security self-skips), a backend outage would
// burn the retry budget on unscreened articles and strand the queue. Here the
// breaker trips on the first call (during batch 1); with the guard, later batches
// are never claimed, so an oldest (batch-2) article keeps its full 3-attempt
// budget. Without the guard it would have been claimed once (budget 2).
func TestSecurityStopsClaimingWhenBreakerTripsMidDrain(t *testing.T) {
	fake := &fakeAI{available: true}
	fake.securityFn = func(string, string) (*ai.SecurityResult, error) {
		fake.mu.Lock()
		fake.available = false // breaker opens on the first real failure
		fake.mu.Unlock()
		return nil, ai.ErrBackendUnavailable
	}
	st, store, feedID := newHarness(t, fake)

	// One definitively-oldest article (lands in batch 2), plus a full first batch
	// of newer ones so the drain must fetch a second batch to reach the old one.
	old := time.Now().Add(-72 * time.Hour)
	oldID, err := store.AddArticle(&storage.Article{
		FeedID: feedID, GUID: "oldest", Title: "oldest",
		URL: "https://example.com/oldest", Content: strings.Repeat("x", 500), PublishedDate: &old,
	})
	if err != nil {
		t.Fatalf("seed oldest: %v", err)
	}
	for i := 0; i < drainBatch+4; i++ {
		seed(t, store, feedID, fmt.Sprintf("n%d", i), strings.Repeat("x", 500))
	}

	if _, err := st.RunSecurity(context.Background()); err != nil {
		t.Fatalf("RunSecurity: %v", err)
	}

	// Probe the oldest article's remaining claim budget: with an expired lease it
	// is claimable (3 - attempts) times before the attempts<3 cap excludes it. 3
	// means no attempt was burned -- the guard stopped the drain before batch 2.
	if got := remainingClaimBudget(t, store, oldID); got != 3 {
		t.Fatalf("oldest article claim budget = %d, want 3 (a mid-drain breaker burned the retry budget)", got)
	}
}

// remainingClaimBudget counts how many times articleID can still be claimed
// (lease expired) before the attempts cap excludes it: 3 - current attempts.
func remainingClaimBudget(t *testing.T, store storage.Store, articleID int64) int {
	t.Helper()
	for n := 0; n <= 4; n++ {
		got, err := store.ClaimUnscreenedArticles(500, 0)
		if err != nil {
			t.Fatalf("claim probe: %v", err)
		}
		found := false
		for _, a := range got {
			if a.ID == articleID {
				found = true
				break
			}
		}
		if !found {
			return n
		}
	}
	return 99 // never excluded -- unexpected
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

func TestSecurityStageRefundsClaimWhenBreakerOpensAfterClaim(t *testing.T) {
	// Reproduce the batch-drop leak: the drain claims a batch (attempt +1, claim
	// stamped) while the backend looks available, then the breaker opens before
	// the security stage runs. The stage must refund every already-claimed article
	// instead of silently dropping the batch. Without the refund, an olla restart
	// burns the retry budget on in-flight articles and parks them at attempts>=3
	// with their claims still held -- the 2026-07-15 rescore stall, where a single
	// restart window left ~7.9k articles stranded.
	fake := &fakeAI{available: false}
	st, store, feedID := newHarness(t, fake)
	art := seed(t, store, feedID, "x", strings.Repeat("x", 500))

	// Stand in for the drain's claim, which happened before the breaker opened.
	claimed, err := store.ClaimUnscreenedArticles(500, securityClaimLeaseSeconds)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed article, got %d", len(claimed))
	}

	// Breaker is open: the stage must skip the backend but give the claim back.
	if passed := st.Security(context.Background(), claimed); len(passed) != 0 {
		t.Fatalf("expected no articles to advance with breaker open, got %v", ids(passed))
	}
	if fake.secCalls != 0 {
		t.Fatalf("expected zero security calls with breaker open, got %d", fake.secCalls)
	}
	// Fully refunded: the retry budget is intact (3), so the article is not parked.
	if got := remainingClaimBudget(t, store, art.ID); got != 3 {
		t.Fatalf("claim budget after breaker-open skip = %d, want 3 (batch drop burned the retry budget)", got)
	}
}

func TestSecurityStageRefundsClaimWhenContextCancelled(t *testing.T) {
	// #245: the drain claims a batch (attempt +1, claim stamped), then the ctx is
	// cancelled mid-batch (daemon shutdown). Every claimed article must still be
	// refunded -- the old mapArticles ctx-cancel break stranded the un-dispatched
	// tail with a burned attempt and a held claim.
	fake := &fakeAI{available: true}
	st, store, feedID := newHarness(t, fake)
	art := seed(t, store, feedID, "x", strings.Repeat("x", 500))

	claimed, err := store.ClaimUnscreenedArticles(500, securityClaimLeaseSeconds)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed article, got %d", len(claimed))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the stage runs, standing in for a mid-batch shutdown

	if passed := st.Security(ctx, claimed); len(passed) != 0 {
		t.Fatalf("expected no articles to advance under a cancelled ctx, got %v", ids(passed))
	}
	if fake.secCalls != 0 {
		t.Fatalf("expected zero security calls under a cancelled ctx, got %d", fake.secCalls)
	}
	if got := remainingClaimBudget(t, store, art.ID); got != 3 {
		t.Fatalf("claim budget after cancelled-ctx skip = %d, want 3 (stranded tail burned the retry budget)", got)
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
		return &ai.SecurityResult{Threat: 1}, nil
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

// summarizableBody is prose long enough to clear the summarize stage's
// no-prose floor, so tests exercise the behavior they name.
const summarizableBody = "The council voted seven to two on Tuesday to approve the new " +
	"drainage district, ending a dispute that had run since the spring floods " +
	"washed out two county roads and a rail crossing."

func TestSummarizeStage(t *testing.T) {
	st, store, feedID := newHarness(t, &fakeAI{available: true})
	mustPass := func(a storage.Article) {
		// Put the article into the security-passed state the summarize stage expects.
		if err := store.ScreenArticleSecurity(a.ID, 1, "none", false, false); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("caches a good summary", func(t *testing.T) {
		st.AI.(*fakeAI).summarizeFn = func(string, string) (string, error) { return "good summary", nil }
		a := seed(t, store, feedID, "good", summarizableBody)
		mustPass(a)
		out := st.Summarize(context.Background(), []storage.Article{a})
		if len(out) != 1 {
			t.Fatalf("expected article to advance, got %v", ids(out))
		}
		got, _ := store.GetArticleSummary(a.ID)
		if got == nil || got.AISummary != "good summary" {
			t.Fatalf("summary not cached: %+v", got)
		}
	})

	t.Run("garbled summary is a transient skip", func(t *testing.T) {
		st.AI.(*fakeAI).summarizeFn = func(string, string) (string, error) { return "### Assistant: junk", nil }
		a := seed(t, store, feedID, "garbled", summarizableBody)
		mustPass(a)
		out := st.Summarize(context.Background(), []storage.Article{a})
		if len(out) != 0 {
			t.Fatalf("garbled summary must not advance the article, got %v", ids(out))
		}
		if got, _ := store.GetArticleSummary(a.ID); got != nil {
			t.Fatalf("garbled summary must not be cached, got %+v", got)
		}
	})

	t.Run("summary longer than content is a deterministic skip", func(t *testing.T) {
		st.AI.(*fakeAI).summarizeFn = func(_, content string) (string, error) {
			return content + " and then a great deal more text than the source", nil
		}
		a := seed(t, store, feedID, "toolong", summarizableBody)
		mustPass(a)
		out := st.Summarize(context.Background(), []storage.Article{a})
		if len(out) != 1 {
			t.Fatalf("deterministically-skipped article should still advance, got %v", ids(out))
		}
		// Marked skipped (summary row exists) so it won't be retried.
		got, _ := store.GetArticleSummary(a.ID)
		if got == nil {
			t.Fatal("expected a skip row recorded for the over-length summary")
		}
	})

	// A body with no prose in it — an editorial cartoon whose whole content is
	// an image, or an excerpt that is nothing but the feed plugin's "appeared
	// first on" footer — gives the model nothing to compress, and a model given
	// nothing describes the boilerplate instead. Skip it rather than caching an
	// invented summary.
	t.Run("body with no summarizable prose is skipped without a model call", func(t *testing.T) {
		cases := map[string]string{
			"image only":       `<p><img src="https://example.com/cartoon.jpg" alt="a cartoon"></p>`,
			"boilerplate only": `<p>The post <a href="https://example.com/a">A Title</a> appeared first on <a href="https://example.com">Example</a>.</p>`,
		}
		for name, body := range cases {
			t.Run(name, func(t *testing.T) {
				a := seed(t, store, feedID, "noprose-"+name, body)
				mustPass(a)
				before := st.AI.(*fakeAI).sumCalls
				out := st.Summarize(context.Background(), []storage.Article{a})
				if len(out) != 1 {
					t.Fatalf("skipped article should still advance, got %v", ids(out))
				}
				if st.AI.(*fakeAI).sumCalls != before {
					t.Error("a body with no prose should not reach the model")
				}
				got, _ := store.GetArticleSummary(a.ID)
				if got == nil {
					t.Fatal("expected a skip row so the article is not retried forever")
				}
				if got.AISummary != "" {
					t.Errorf("expected no summary text, got %q", got.AISummary)
				}
			})
		}
	})

	t.Run("already-summarized article passes through without a model call", func(t *testing.T) {
		a := seed(t, store, feedID, "cached", "body")
		mustPass(a)
		if err := store.UpdateArticleAISummary(a.ID, "preexisting"); err != nil {
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
		if err := store.ScreenArticleSecurity(a.ID, 1, "none", false, false); err != nil {
			t.Fatal(err)
		}

		out := st.Curate(context.Background(), []storage.Article{a})
		if len(out) != 1 {
			t.Fatalf("expected article curated, got %v", ids(out))
		}
		// Left the curation queue and still counts as security-passed.
		cur, _ := store.GetUnscoredCurationArticles(1, 3.0, 10)
		if len(cur) != 0 {
			t.Fatalf("curated article should leave the curation queue, got %v", ids(cur))
		}
		scored, _ := store.GetUnsummarizedScoredArticles(3.0, 10)
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
		if err := store.ScreenArticleSecurity(a.ID, 1, "none", false, false); err != nil {
			t.Fatal(err)
		}

		st.Curate(context.Background(), []storage.Article{a})

		var ev struct {
			Event          string  `json:"event"`
			SecurityThreat float64 `json:"security_threat"`
			InterestScore  float64 `json:"interest_score"`
		}
		if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
			t.Fatalf("decode processed event: %v (out=%q)", err, buf.String())
		}
		if ev.Event != "article_processed" {
			t.Fatalf("event = %q, want article_processed", ev.Event)
		}
		// The curate stage must surface the verdict the security stage wrote,
		// not a 0 placeholder — the event feeds logs and notifications (#119).
		// The article was screened at threat 1 (setup below), so that is what the
		// event must carry.
		if ev.SecurityThreat != 1 {
			t.Errorf("security_threat = %v, want 1 (persisted verdict)", ev.SecurityThreat)
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
		if err := store.ScreenArticleSecurity(a.ID, 1, "none", false, false); err != nil {
			t.Fatal(err)
		}

		st.Curate(context.Background(), []storage.Article{a})

		// Success must be visible in the daemon log (stderr) with the actual
		// scores, not only as a structured stdout event.
		got := errBuf.String()
		if !strings.Contains(got, "Info:") {
			t.Errorf("expected an info-level success line, got: %q", got)
		}
		if !strings.Contains(got, "interest=8.0") || !strings.Contains(got, "security=1.0") {
			t.Errorf("info line missing actual scores, got: %q", got)
		}
	})

	t.Run("curation failure leaves the article for a later cycle", func(t *testing.T) {
		fake := &fakeAI{available: true, curateFn: func(string, string) (*ai.CurationResult, error) {
			return nil, errors.New("model timeout")
		}}
		st, store, feedID := newHarness(t, fake)
		a := seed(t, store, feedID, "x", "body text")
		if err := store.ScreenArticleSecurity(a.ID, 1, "none", false, false); err != nil {
			t.Fatal(err)
		}
		out := st.Curate(context.Background(), []storage.Article{a})
		if len(out) != 0 {
			t.Fatalf("failed curation should not advance, got %v", ids(out))
		}
		cur, _ := store.GetUnscoredCurationArticles(1, 3.0, 10)
		if len(cur) != 1 {
			t.Fatalf("failed article should still await curation, got %v", ids(cur))
		}
	})
}
