package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
)

func TestRegenerateSummaries(t *testing.T) {
	fake := &fakeAI{available: true}
	fake.summarizeFn = func(string, string) (string, error) { return "local summary", nil }
	st, store, feedID := newHarness(t, fake)
	pass := func(a storage.Article) storage.Article {
		t.Helper()
		if err := store.ScreenArticleSecurity(a.ID, 1, "none", false, false); err != nil {
			t.Fatal(err)
		}
		return a
	}

	// Summarized by another model: gets a second summary, which takes over.
	cloud := pass(seed(t, store, feedID, "cloud", summarizableBody))
	if err := store.UpdateArticleAISummary(cloud.ID, "cloud summary", storage.SummaryProvenance{Model: "cloud-model"}); err != nil {
		t.Fatal(err)
	}
	// Already summarized by the target model: left alone.
	done := pass(seed(t, store, feedID, "done", summarizableBody))
	if err := store.UpdateArticleAISummary(done.ID, "earlier local summary", storage.SummaryProvenance{Model: "fake-model"}); err != nil {
		t.Fatal(err)
	}
	// Summarized before models were recorded: unknown origin, so regenerated.
	legacy := pass(seed(t, store, feedID, "legacy", summarizableBody))
	if err := store.UpdateArticleAISummary(legacy.ID, "legacy summary", storage.SummaryProvenance{}); err != nil {
		t.Fatal(err)
	}
	// Never summarized.
	fresh := pass(seed(t, store, feedID, "fresh", summarizableBody))
	// Too little prose for any model: marked once against the target model.
	short := pass(seed(t, store, feedID, "short", "tiny"))
	// Did not pass the screen: never sent to the summarizer.
	blocked := seed(t, store, feedID, "blocked", summarizableBody)
	if err := store.ScreenArticleSecurity(blocked.ID, 8, "none", false, false); err != nil {
		t.Fatal(err)
	}

	var progress []RegenResult
	res, err := st.RegenerateSummaries(context.Background(), Regen{Model: "fake-model", Progress: func(r RegenResult) { progress = append(progress, r) }})
	if err != nil {
		t.Fatalf("RegenerateSummaries: %v", err)
	}
	if res.Attempted != 4 || res.Recorded != 4 {
		t.Errorf("result = %+v, want 4 attempted and 4 recorded (cloud, legacy, fresh, short)", res)
	}
	if fake.sumCalls != 3 {
		t.Errorf("model calls = %d, want 3: the short article is skipped without one", fake.sumCalls)
	}
	if len(progress) == 0 || progress[len(progress)-1] != res {
		t.Errorf("progress = %+v, want it to end on the final result %+v", progress, res)
	}

	for name, a := range map[string]storage.Article{"cloud": cloud, "legacy": legacy, "fresh": fresh} {
		cur, _ := store.GetArticleSummary(a.ID)
		if cur == nil || cur.AISummary != "local summary" || cur.Model != "fake-model" {
			t.Errorf("%s: current summary = %+v, want the regenerated one", name, cur)
		}
	}
	hist, _ := store.ListArticleSummaries(cloud.ID)
	if len(hist) != 2 || hist[1].AISummary != "cloud summary" || hist[1].Model != "cloud-model" || hist[1].IsCurrent {
		t.Errorf("cloud history = %+v, want the cloud summary kept beside the new one", hist)
	}
	if hist, _ := store.ListArticleSummaries(done.ID); len(hist) != 1 || hist[0].AISummary != "earlier local summary" {
		t.Errorf("already-done article history = %+v, want it untouched", hist)
	}
	if cur, _ := store.GetArticleSummary(short.ID); cur == nil || cur.SkipReason == "" || cur.Model != "fake-model" {
		t.Errorf("short article = %+v, want a skip marker naming the target model", cur)
	}
	if hist, _ := store.ListArticleSummaries(blocked.ID); len(hist) != 0 {
		t.Errorf("blocked article got %d summary rows, want none", len(hist))
	}

	// A second run finds nothing left to do, the short article included.
	fake.sumCalls = 0
	res, err = st.RegenerateSummaries(context.Background(), Regen{Model: "fake-model"})
	if err != nil || res.Attempted != 0 || fake.sumCalls != 0 {
		t.Errorf("second run = %+v, %v with %d model calls; want nothing to do", res, err, fake.sumCalls)
	}
}

func TestRegenerateSummaries_LimitAndFailures(t *testing.T) {
	fake := &fakeAI{available: true}
	st, store, feedID := newHarness(t, fake)
	for _, g := range []string{"a", "b", "c", "d", "e"} {
		a := seed(t, store, feedID, g, summarizableBody)
		if err := store.ScreenArticleSecurity(a.ID, 1, "none", false, false); err != nil {
			t.Fatal(err)
		}
	}

	// A run where every call fails must end, not circle the same articles.
	fake.summarizeFn = func(string, string) (string, error) { return "", errors.New("model fell over") }
	res, err := st.RegenerateSummaries(context.Background(), Regen{Model: "fake-model"})
	if err != nil || res.Attempted != 5 || res.Recorded != 0 {
		t.Fatalf("all-failing run = %+v, %v; want 5 attempted, none recorded, and an end", res, err)
	}

	fake.summarizeFn = nil
	res, err = st.RegenerateSummaries(context.Background(), Regen{Model: "fake-model", Limit: 2})
	if err != nil || res.Attempted != 2 || res.Recorded != 2 {
		t.Fatalf("limited run = %+v, %v; want exactly 2", res, err)
	}

	// With the backend down nothing is attempted and the caller is told why.
	fake.available = false
	res, err = st.RegenerateSummaries(context.Background(), Regen{Model: "fake-model"})
	if !errors.Is(err, ai.ErrBackendUnavailable) || res.Attempted != 0 {
		t.Fatalf("backend-down run = %+v, %v; want ErrBackendUnavailable and nothing attempted", res, err)
	}
}

func TestRegenerateScreens_ShadowOnly(t *testing.T) {
	fake := &fakeAI{available: true}
	// The new model disagrees with every existing verdict, in both directions.
	fake.securityFn = func(title, _ string) (*ai.SecurityResult, error) {
		r := &ai.SecurityResult{Threat: 0, Category: "none", Model: "fake-screen", PromptHash: "h"}
		switch {
		case strings.HasPrefix(title, "clean"):
			r.Threat, r.Category = 8, "instruction_override"
		case strings.HasPrefix(title, "legacy"):
			r.Threat = 5
		}
		return r, nil
	}
	st, store, feedID := newHarness(t, fake)
	body := strings.Repeat("x", 500)

	// Passed by another model; the new one would block it.
	clean := seed(t, store, feedID, "clean", body)
	// Blocked by another model; the new one sees nothing.
	hostile := seed(t, store, feedID, "hostile", body)
	for id, threat := range map[int64]float64{clean.ID: 0, hostile.ID: 9} {
		if err := store.RecordArticleScreen(storage.ArticleScreen{ArticleID: id, Model: "cloud-model", Via: "sync", Threat: threat, Category: "none", Effective: true}); err != nil {
			t.Fatal(err)
		}
	}
	// Screened before models were recorded.
	legacy := seed(t, store, feedID, "legacy", body)
	if err := store.ScreenArticleSecurity(legacy.ID, 2, "none", false, false); err != nil {
		t.Fatal(err)
	}
	// Already scanned by the target model.
	done := seed(t, store, feedID, "done", body)
	if err := store.RecordArticleScreen(storage.ArticleScreen{ArticleID: done.ID, Model: "fake-screen", Via: "sync", Threat: 1, Category: "none", Effective: true}); err != nil {
		t.Fatal(err)
	}
	// Not screened yet: that is the live screen's job, with its claims.
	unscreened := seed(t, store, feedID, "unscreened", body)

	res, err := st.RegenerateScreens(context.Background(), Regen{Model: "fake-screen"})
	if err != nil {
		t.Fatalf("RegenerateScreens: %v", err)
	}
	if res.Attempted != 3 || res.Recorded != 3 || fake.secCalls != 3 {
		t.Errorf("result = %+v with %d model calls, want 3 and 3 (clean, hostile, legacy)", res, fake.secCalls)
	}

	// Nothing that gates an article moved.
	scores, _ := store.GetArticleSecurityScores([]int64{clean.ID, hostile.ID, legacy.ID})
	if scores[clean.ID] != 0 || scores[hostile.ID] != 9 || scores[legacy.ID] != 2 {
		t.Errorf("gating verdicts after a shadow run = %v, want 0, 9 and 2 unchanged", scores)
	}
	if left, _ := store.GetUnscreenedArticles(10); len(left) != 1 || left[0].ID != unscreened.ID {
		t.Errorf("unscreened queue = %v, want the unscreened article still waiting", ids(left))
	}

	for _, tc := range []struct {
		name    string
		id      int64
		rows    int
		threat  float64
		flagged bool
	}{
		{"clean", clean.ID, 2, 8, false},
		{"hostile", hostile.ID, 2, 0, false},
		{"legacy", legacy.ID, 1, 5, true}, // borderline under the default thresholds
	} {
		scans, _ := store.ListArticleScreens(tc.id)
		if len(scans) != tc.rows {
			t.Fatalf("%s: %d screen rows, want %d", tc.name, len(scans), tc.rows)
		}
		got := scans[0]
		if got.Model != "fake-screen" || got.Effective || got.Threat != tc.threat || got.Flagged != tc.flagged || got.Via != storage.ModelOutputViaSync {
			t.Errorf("%s: newest scan = %+v, want a shadow by fake-screen, threat %v flagged %v", tc.name, got, tc.threat, tc.flagged)
		}
	}
	// The completions go to the corpus like any other scan's.
	if outs, _ := store.ListModelOutputsByArticle(clean.ID); len(outs) != 0 {
		// fakeAI returns no chunks, so there is nothing to record; this guards
		// against a row being invented for a call that reported none.
		t.Errorf("corpus rows = %d, want none from a result with no chunks", len(outs))
	}

	fake.secCalls = 0
	res, err = st.RegenerateScreens(context.Background(), Regen{Model: "fake-screen"})
	if err != nil || res.Attempted != 0 || fake.secCalls != 0 {
		t.Errorf("second run = %+v, %v with %d model calls; want nothing to do", res, err, fake.secCalls)
	}
}

// A scan that comes back attributed to some other model than the one the run
// is filling in would never satisfy the queue, and would be re-run forever.
func TestRegenerateScreens_RefusesAMismatchedModel(t *testing.T) {
	fake := &fakeAI{available: true}
	fake.securityFn = func(string, string) (*ai.SecurityResult, error) {
		return &ai.SecurityResult{Threat: 0, Model: "something-else"}, nil
	}
	st, store, feedID := newHarness(t, fake)
	a := seed(t, store, feedID, "a", strings.Repeat("x", 500))
	if err := store.ScreenArticleSecurity(a.ID, 1, "none", false, false); err != nil {
		t.Fatal(err)
	}
	res, err := st.RegenerateScreens(context.Background(), Regen{Model: "fake-screen"})
	if err == nil {
		t.Fatalf("mismatched model accepted: %+v", res)
	}
	if scans, _ := store.ListArticleScreens(a.ID); len(scans) != 0 {
		t.Errorf("mismatched scan was recorded: %+v", scans)
	}
}
