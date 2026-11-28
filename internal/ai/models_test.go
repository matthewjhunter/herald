package ai

import "testing"

func TestConfiguredModels(t *testing.T) {
	p, err := NewAIProcessor("http://unused.invalid", "sec-model", "cur-model", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.ScreenModel(); got != "sec-model" {
		t.Errorf("ScreenModel = %q, want the security model", got)
	}
	// Summaries have no model of their own: they borrow the curation model
	// unless the summarization prompt names one.
	got, err := p.SummaryModel()
	if err != nil || got != "cur-model" {
		t.Errorf("SummaryModel = %q, %v; want the curation model", got, err)
	}
	plan, err := p.SecurityPlan("title", "Some content to screen for the plan.")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Model != p.ScreenModel() {
		t.Errorf("SecurityPlan model %q and ScreenModel %q disagree", plan.Model, p.ScreenModel())
	}
}
