package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestLooksLikeTopicLabel(t *testing.T) {
	valid := []string{
		"Russia Strikes Kyiv in Missile Barrage",
		"Federal Reserve Holds Rates Steady",
		"Israel-Lebanon Border Escalation",
		"Harvard Antisemitism Investigation",
	}
	for _, s := range valid {
		if !looksLikeTopicLabel(s) {
			t.Errorf("expected valid label: %q", s)
		}
	}

	invalid := []string{
		// The exact production bug: a conversational request stored as a label.
		"Please provide the summary of related news articles so I can generate the topic label.",
		"I can generate a label once you share the articles.",
		"I need the article summaries first.",
		"Sorry, I cannot do that.",
		"As an AI, I'm unable to summarize without input.",
		"What articles would you like me to label?",
		"Here is the topic label:",
		"",
		strings.Repeat("x", 150), // too long to be a label
	}
	for _, s := range invalid {
		if looksLikeTopicLabel(s) {
			t.Errorf("expected invalid (conversational/too-long): %q", s)
		}
	}
}

// The summary carries its provenance out of the call: the model actually sent
// (the processor default when no tier names one), the hash of the resolved
// prompt, and a wall-clock duration for the model call -- so the pipeline can
// record what produced the text rather than what is current later.
func TestSummarizeArticle_ReportsProvenance(t *testing.T) {
	var calls int32
	var sentModel string
	p := fakeModelServerFunc(t, &calls, func(reqBody string) string {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal([]byte(reqBody), &req)
		sentModel = req.Model
		return "  A short summary.  "
	})

	got, err := p.SummarizeArticle(context.Background(), "Title", "Some article body text long enough to summarize.", 200)
	if err != nil {
		t.Fatalf("SummarizeArticle: %v", err)
	}
	if got.Summary != "A short summary." {
		t.Errorf("summary: got %q", got.Summary)
	}
	if got.Model != "test-model" || got.Model != sentModel {
		t.Errorf("model: got %q, server saw %q, want test-model", got.Model, sentModel)
	}
	want, err := p.promptLoader.Resolve(0, PromptTypeSummarization)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.PromptHash == "" || got.PromptHash != want.Hash {
		t.Errorf("prompt hash: got %q, want %q", got.PromptHash, want.Hash)
	}
	if got.Duration <= 0 {
		t.Errorf("duration: got %v, want > 0", got.Duration)
	}
}
