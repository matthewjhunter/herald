package storage

import (
	"testing"
	"time"
)

func TestCompareScreens(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	scan := func(id int64, model string, threat float64) {
		t.Helper()
		if err := store.RecordArticleScreen(ArticleScreen{ArticleID: id, Model: model, Via: "sync", Threat: threat, Category: "none", Effective: false}); err != nil {
			t.Fatalf("RecordArticleScreen: %v", err)
		}
	}
	agreePass := historyArticle(t, store, "agree-pass")
	aPassBBlock := historyArticle(t, store, "a-pass-b-block")
	aBorderBPass := historyArticle(t, store, "a-border-b-pass")
	agreeBlock := historyArticle(t, store, "agree-block")
	onlyA := historyArticle(t, store, "only-a")

	// An older scan by A that its newer one supersedes: only the latest counts.
	scan(agreePass, "A", 9)
	scan(agreePass, "A", 0)
	scan(agreePass, "B", 0)
	scan(aPassBBlock, "A", 0)
	scan(aPassBBlock, "B", 8)
	scan(aBorderBPass, "A", 5)
	scan(aBorderBPass, "B", 1)
	scan(agreeBlock, "A", 9)
	scan(agreeBlock, "B", 9)
	scan(onlyA, "A", 7)

	cmp, err := store.CompareScreens("A", "B", 3, 6, 10)
	if err != nil {
		t.Fatalf("CompareScreens: %v", err)
	}
	if cmp.Compared != 4 {
		t.Fatalf("compared = %d, want 4 (the article only A scanned is left out)", cmp.Compared)
	}
	want := [3][3]int{{1, 0, 1}, {1, 0, 0}, {0, 0, 1}}
	if cmp.Matrix != want {
		t.Errorf("matrix = %v, want %v", cmp.Matrix, want)
	}
	if cmp.Agree() != 2 {
		t.Errorf("agree = %d, want 2", cmp.Agree())
	}
	if cmp.MeanAbsDelta != 3 {
		t.Errorf("mean |delta| = %v, want 3", cmp.MeanAbsDelta)
	}
	if len(cmp.Disagreements) != 2 || cmp.Disagreements[0].ArticleID != aPassBBlock ||
		cmp.Disagreements[0].ThreatA != 0 || cmp.Disagreements[0].ThreatB != 8 || cmp.Disagreements[0].Title != "a-pass-b-block" {
		t.Errorf("disagreements = %+v, want the widest gap first", cmp.Disagreements)
	}

	models, err := store.ListScreenModels()
	if err != nil || len(models) != 2 || models[0].Model != "A" || models[0].Rows != 6 || models[1].Model != "B" || models[1].Rows != 4 {
		t.Errorf("screen models = %+v, %v", models, err)
	}
}

func TestCompareSummaries(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	both := historyArticle(t, store, "both")
	bSkipped := historyArticle(t, store, "b-skipped")
	onlyA := historyArticle(t, store, "only-a")

	sum := func(id int64, model, text string, d time.Duration) {
		t.Helper()
		if err := store.UpdateArticleAISummary(id, text, SummaryProvenance{Model: model, Duration: d}); err != nil {
			t.Fatalf("UpdateArticleAISummary: %v", err)
		}
	}
	sum(both, "A", "12345678", 2*time.Second)
	sum(both, "B", "1234", 4*time.Second)
	sum(bSkipped, "A", "A managed it", time.Second)
	if err := store.MarkSummarizationSkipped(bSkipped, "summary longer than content (9 > 5)", "B"); err != nil {
		t.Fatal(err)
	}
	sum(onlyA, "A", "alone", time.Second)

	cmp, err := store.CompareSummaries("A", "B", 10)
	if err != nil {
		t.Fatalf("CompareSummaries: %v", err)
	}
	if cmp.Compared != 2 || cmp.BothText != 1 || cmp.OnlyAText != 1 || cmp.OnlyBText != 0 || cmp.NeitherText != 0 {
		t.Errorf("counts = %+v, want 2 compared: 1 both, 1 only A", cmp)
	}
	if cmp.AvgCharsA != 8 || cmp.AvgCharsB != 4 {
		t.Errorf("average length = %v / %v, want 8 / 4 over the article both summarized", cmp.AvgCharsA, cmp.AvgCharsB)
	}
	if cmp.AvgDurationA != 1500*time.Millisecond || cmp.AvgDurationB != 4*time.Second {
		t.Errorf("average duration = %v / %v, want 1.5s / 4s", cmp.AvgDurationA, cmp.AvgDurationB)
	}
	if len(cmp.Samples) != 1 || cmp.Samples[0].ArticleID != both || cmp.Samples[0].SummaryA != "12345678" || cmp.Samples[0].SummaryB != "1234" {
		t.Errorf("samples = %+v, want the one article both summarized", cmp.Samples)
	}
}
