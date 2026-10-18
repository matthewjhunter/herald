package web

import (
	"encoding/json"
	"os/exec"
	"testing"
)

type promptToggleStep struct {
	Step      string `json:"step"`
	Prevented bool   `json:"prevented"`
	Hidden    bool   `json:"hidden"`
}

// jsPromptToggle runs the shipped static/herald.js under node and replays a
// sequence of clicks and loads against the summary "prompt" link. Node is not
// a build dependency of herald, so the test skips where it is unavailable.
func jsPromptToggle(t *testing.T, toggle bool, steps []string) []promptToggleStep {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping herald.js behaviour test")
	}
	payload, err := json.Marshal(map[string]any{"toggle": toggle, "steps": steps})
	if err != nil {
		t.Fatalf("marshal case: %v", err)
	}
	out, err := exec.Command(node, "testdata/heraldjs_prompt_toggle_harness.js", "static/herald.js", string(payload)).CombinedOutput()
	if err != nil {
		t.Fatalf("node harness: %v\n%s", err, out)
	}
	var results []promptToggleStep
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("parse harness output %q: %v", out, err)
	}
	if len(results) != len(steps) {
		t.Fatalf("harness returned %d results, want %d", len(results), len(steps))
	}
	return results
}

// The "prompt" link under a summary fetches the prompt once; after that each
// click hides or shows what was loaded without another request. The first
// click must reach the server (the target is empty), and once the prompt is
// in the page, clicks alternate hidden/visible and never refetch.
func TestSummaryPromptLinkTogglesAfterFirstLoad(t *testing.T) {
	got := jsPromptToggle(t, true, []string{"click", "load", "click", "click", "click"})
	want := []promptToggleStep{
		{Step: "click", Prevented: false, Hidden: false},
		{Step: "load", Prevented: false, Hidden: false},
		{Step: "click", Prevented: true, Hidden: true},
		{Step: "click", Prevented: true, Hidden: false},
		{Step: "click", Prevented: true, Hidden: true},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Other htmx links whose target already has content are not hijacked: the
// behavior is keyed off the data-summary-prompt-toggle marker, not off "the
// target is non-empty".
func TestSummaryPromptToggleIgnoresOtherLinks(t *testing.T) {
	got := jsPromptToggle(t, false, []string{"click", "load", "click"})
	for i, s := range got {
		if s.Prevented || s.Hidden {
			t.Errorf("step %d: unmarked link was affected: %+v", i, s)
		}
	}
}
