package storage

import (
	"strings"
	"testing"
)

// Pattern matching for filter rules (#274). These tests cover the schema half:
// what the table will now accept and store. The evaluation half is in Go, above
// the storage layer, and is tested there.

// Every rule that existed before #274 was an exact match, and none of them were
// rewritten by the migration. The default has to carry that meaning forward, or
// the upgrade silently changes what those rules do.
func TestFilterRuleMatchModeDefaultsToExact(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	if _, err := store.AddFilterRule(&FilterRule{UserID: 1, Axis: "author", Value: "Alice", Score: 5}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}

	rules, err := store.GetFilterRules(1, nil)
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

func TestFilterRulePatternModesRoundTrip(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	for _, tc := range []struct {
		axis  string
		mode  string
		value string
	}{
		{"title", MatchRegex, `(?i)\bopen thread\b`},
		{"title", MatchSubstring, "Open Thread"},
		{"summary", MatchRegex, `day \d+`},
		{"content", MatchSubstring, "sponsored post"},
		{"author", MatchRegex, `^Sundance$`},
		{"category", MatchSubstring, "uncategor"},
		{"tag", MatchRegex, `sec(urity)?`},
	} {
		t.Run(tc.axis+"/"+tc.mode, func(t *testing.T) {
			id, err := store.AddFilterRule(&FilterRule{
				UserID:    1,
				Axis:      tc.axis,
				MatchMode: tc.mode,
				Value:     tc.value,
				Score:     -5,
			})
			if err != nil {
				t.Fatalf("AddFilterRule: %v", err)
			}

			rules, err := store.GetFilterRules(1, nil)
			if err != nil {
				t.Fatalf("GetFilterRules: %v", err)
			}
			var got *FilterRule
			for i := range rules {
				if rules[i].ID == id {
					got = &rules[i]
				}
			}
			if got == nil {
				t.Fatalf("rule %d not returned", id)
			}
			if got.Axis != tc.axis || got.MatchMode != tc.mode || got.Value != tc.value {
				t.Errorf("round trip: axis=%q mode=%q value=%q, want %q/%q/%q",
					got.Axis, got.MatchMode, got.Value, tc.axis, tc.mode, tc.value)
			}
		})
	}
}

// A substring rule for "open thread" and a regex rule for the same string are
// different rules, so match_mode has to be part of the uniqueness key. Before
// #274 the key was (user, feed, axis, value) and the second insert would have
// been rejected as a duplicate.
func TestFilterRuleUniquePerMatchMode(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	base := func(mode string) *FilterRule {
		return &FilterRule{UserID: 1, Axis: "title", MatchMode: mode, Value: "open thread", Score: -3}
	}

	for _, mode := range []string{MatchExact, MatchSubstring, MatchRegex} {
		if _, err := store.AddFilterRule(base(mode)); err != nil {
			t.Fatalf("AddFilterRule(%s): %v", mode, err)
		}
	}

	// Same mode twice is still a duplicate.
	if _, err := store.AddFilterRule(base(MatchRegex)); err == nil {
		t.Error("expected duplicate error for a second regex rule with the same axis and value")
	}
}

func TestFilterRuleRejectsUnknownMatchMode(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	_, err := store.AddFilterRule(&FilterRule{
		UserID: 1, Axis: "title", MatchMode: "glob", Value: "x*", Score: 1,
	})
	if err == nil {
		t.Fatal("expected the match_mode CHECK constraint to reject 'glob'")
	}
}

func TestFilterRuleRejectsUnknownAxis(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	_, err := store.AddFilterRule(&FilterRule{
		UserID: 1, Axis: "byline", MatchMode: MatchExact, Value: "x", Score: 1,
	})
	if err == nil {
		t.Fatal("expected the axis CHECK constraint to reject 'byline'")
	}
}

// The SQL matcher and the Go evaluator partition the rule set between them:
// SQL takes exact matches on the metadata axes, Go takes everything else. If
// the SQL side stopped filtering on match_mode it would also match the pattern
// rules -- as exact comparisons, wrongly -- and every such rule would be
// counted twice in the effective score.
func TestSQLMatcherIsLimitedToExactMode(t *testing.T) {
	if !strings.Contains(filterRuleMatch("?"), "fr.match_mode = 'exact'") {
		t.Error("the SQL matcher no longer restricts itself to exact-mode rules; pattern rules will be double counted")
	}
}
