package herald

import (
	"strings"
	"testing"
)

// Validation for pattern filter rules (#274). A rule is accepted here or not at
// all: the reader must never be handed a pattern that fails to compile deeper
// in the pipeline, where there is no user to tell.

func TestAddFilterRuleAcceptsTextAxes(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	for _, axis := range []string{"title", "summary", "content"} {
		if _, err := engine.AddFilterRule(1, FilterRule{
			Axis: axis, MatchMode: MatchSubstring, Value: "open thread", Score: -5,
		}); err != nil {
			t.Errorf("AddFilterRule(%s): %v", axis, err)
		}
	}
}

// An omitted mode has to mean exact, because every rule written before #274
// omitted it.
func TestAddFilterRuleDefaultsToExactMode(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	if _, err := engine.AddFilterRule(1, FilterRule{Axis: "author", Value: "Alice", Score: 5}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}

	rules, err := engine.GetFilterRules(1, nil)
	if err != nil {
		t.Fatalf("GetFilterRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].MatchMode != MatchExact {
		t.Errorf("match mode = %q, want %q", rules[0].MatchMode, MatchExact)
	}
}

func TestAddFilterRuleRejectsUnknownMatchMode(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	_, err := engine.AddFilterRule(1, FilterRule{
		Axis: "title", MatchMode: "glob", Value: "x*", Score: 1,
	})
	if err == nil {
		t.Fatal("expected an error for an unknown match mode")
	}
	if !strings.Contains(err.Error(), "glob") {
		t.Errorf("error should name the offending mode, got: %v", err)
	}
}

// The pattern is compiled at save time so the error reaches the person who can
// fix it. RE2 rejects lookahead, which is the mistake a user bringing PCRE
// habits will make first.
func TestAddFilterRuleRejectsUncompilablePattern(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	for _, pattern := range []string{
		`(unclosed`,
		`a{2,1}`,
		`(?=lookahead)`,
	} {
		_, err := engine.AddFilterRule(1, FilterRule{
			Axis: "title", MatchMode: MatchRegex, Value: pattern, Score: -1,
		})
		if err == nil {
			t.Errorf("expected %q to be rejected as an invalid pattern", pattern)
		}
	}
}

func TestAddFilterRuleAcceptsValidPattern(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	for _, pattern := range []string{
		`(?i)\bopen thread\b`,
		`trump administration day \d+`,
	} {
		if _, err := engine.AddFilterRule(1, FilterRule{
			Axis: "title", MatchMode: MatchRegex, Value: pattern, Score: -5,
		}); err != nil {
			t.Errorf("AddFilterRule(%q): %v", pattern, err)
		}
	}
}

// A substring or regex rule is evaluated in Go against every candidate row, so
// pattern rules are metered separately and far more tightly than exact rules,
// which are an indexed equality in SQL. The general MaxFilterRulesPerUser
// default of 1000 is a sane ceiling for the latter and a denial of service for
// the former.
func TestPatternFilterRuleQuotaIsSeparate(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	engine.config.Limits.MaxPatternFilterRulesPerUser = 2

	for i, value := range []string{"one", "two"} {
		if _, err := engine.AddFilterRule(1, FilterRule{
			Axis: "title", MatchMode: MatchSubstring, Value: value, Score: -1,
		}); err != nil {
			t.Fatalf("pattern rule %d: %v", i, err)
		}
	}

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: "title", MatchMode: MatchSubstring, Value: "three", Score: -1,
	}); err == nil {
		t.Error("expected the third pattern rule to be refused")
	}

	// Exact rules are metered by MaxFilterRulesPerUser and must be unaffected.
	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: "author", MatchMode: MatchExact, Value: "Alice", Score: 5,
	}); err != nil {
		t.Errorf("exact rule refused by the pattern quota: %v", err)
	}
}

// Content rules scan full article bodies, measured at roughly 2ms per rule per
// 50-article page against about 30us for a title rule. They get a ceiling of
// their own, well under the general pattern quota.
func TestContentFilterRuleQuotaIsTighter(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	engine.config.Limits.MaxPatternFilterRulesPerUser = 50
	engine.config.Limits.MaxContentFilterRulesPerUser = 2

	for i, value := range []string{"one", "two"} {
		if _, err := engine.AddFilterRule(1, FilterRule{
			Axis: AxisContent, MatchMode: MatchSubstring, Value: value, Score: -1,
		}); err != nil {
			t.Fatalf("content rule %d: %v", i, err)
		}
	}

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisContent, MatchMode: MatchSubstring, Value: "three", Score: -1,
	}); err == nil {
		t.Error("expected the third content rule to be refused")
	}

	// Title rules are cheap and keep the general pattern quota.
	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "three", Score: -1,
	}); err != nil {
		t.Errorf("title rule refused by the content quota: %v", err)
	}
}
