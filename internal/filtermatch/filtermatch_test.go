package filtermatch

import (
	"slices"
	"testing"
)

func rule(id int64, axis, mode, value string, score int) Rule {
	return Rule{ID: id, Axis: axis, MatchMode: mode, Value: value, Score: score}
}

func feedRule(id, feedID int64, axis, mode, value string, score int) Rule {
	r := rule(id, axis, mode, value, score)
	r.FeedID = &feedID
	return r
}

// The case this whole feature exists for: a feed publishing the same
// auto-generated post every day under a title whose date changes.
func TestMatchesRecurringTitles(t *testing.T) {
	m, err := New([]Rule{
		rule(1, AxisTitle, MatchRegex, `(?i)\bopen thread\b`, -5),
		rule(2, AxisTitle, MatchRegex, `(?i)trump administration day \d+`, -5),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		title string
		want  int
	}{
		{"Sunday August 9th - Open Thread", -5},
		{"Monday August 10th - Open Thread", -5},
		{"August 9th - 2026 Presidential Politics - Trump Administration Day 567", -5},
		{"August 8th - 2026 Presidential Politics - Trump Administration Day 566", -5},
		{"Senate Votes Overnight to Confirm Todd Blanche as U.S. Attorney General", 0},
		{"Employers Using J1 Student Visas for Seasonal Work Pay NO Social Security Taxes", 0},
	} {
		got, _ := m.Score(1, Subject{Title: tc.title})
		if got != tc.want {
			t.Errorf("Score(%q) = %d, want %d", tc.title, got, tc.want)
		}
	}
}

func TestMatchModes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		value   string
		title   string
		matches bool
	}{
		{"exact matches whole string", MatchExact, "Open Thread", "Open Thread", true},
		{"exact does not match a substring", MatchExact, "Open Thread", "Sunday - Open Thread", false},
		{"exact ignores case", MatchExact, "open thread", "Open Thread", true},
		{"substring matches inside", MatchSubstring, "Open Thread", "Sunday - Open Thread", true},
		{"substring ignores case", MatchSubstring, "open THREAD", "Sunday - Open Thread", true},
		{"substring does not match absent text", MatchSubstring, "Open Thread", "Senate Votes", false},
		{"regex matches", MatchRegex, `Day \d+`, "Administration Day 567", true},
		{"regex is case sensitive unless told otherwise", MatchRegex, `day \d+`, "Administration Day 567", false},
		{"regex honours an inline flag", MatchRegex, `(?i)day \d+`, "Administration Day 567", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := New([]Rule{rule(1, AxisTitle, tc.mode, tc.value, -3)})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			score, fired := m.Score(1, Subject{Title: tc.title})
			if tc.matches && (score != -3 || !slices.Equal(fired, []int64{1})) {
				t.Errorf("expected a match, got score %d fired %v", score, fired)
			}
			if !tc.matches && (score != 0 || len(fired) != 0) {
				t.Errorf("expected no match, got score %d fired %v", score, fired)
			}
		})
	}
}

func TestAxes(t *testing.T) {
	subject := Subject{
		Title:      "Sunday August 9th - Open Thread",
		Summary:    "A new daily thread for discussion.",
		Content:    "<p>Users are encouraged to post anything relating to the topic.</p>",
		Author:     "Sundance",
		Authors:    []string{"Sundance", "Guest Contributor"},
		Categories: []string{"Uncategorized", "Open Threads"},
	}

	for _, tc := range []struct {
		axis    string
		value   string
		matches bool
	}{
		{AxisTitle, "open thread", true},
		{AxisTitle, "encouraged", false},
		{AxisSummary, "daily thread", true},
		{AxisSummary, "open thread", false},
		{AxisContent, "encouraged to post", true},
		{AxisContent, "daily thread", false},
		{AxisAuthor, "sundance", true},
		{AxisAuthor, "guest contributor", true},
		{AxisAuthor, "margaret", false},
		{AxisCategory, "uncategorized", true},
		{AxisCategory, "open threads", true},
		{AxisCategory, "security", false},
		{AxisTag, "uncategorized", true},
	} {
		t.Run(tc.axis+"/"+tc.value, func(t *testing.T) {
			m, err := New([]Rule{rule(1, tc.axis, MatchSubstring, tc.value, -2)})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			score, _ := m.Score(1, subject)
			if got := score != 0; got != tc.matches {
				t.Errorf("axis %s value %q: matched=%v, want %v", tc.axis, tc.value, got, tc.matches)
			}
		})
	}
}

// The author axis reaches both the normalized names and the feed's free-text
// author field, because plenty of feeds populate only the latter. Exact-mode
// rules, matched in SQL, see only the normalized table -- a difference the UI
// has to be honest about.
func TestAuthorAxisReachesFreeTextAuthor(t *testing.T) {
	m, err := New([]Rule{rule(1, AxisAuthor, MatchRegex, `^Sundance$`, -4)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	score, _ := m.Score(1, Subject{Author: "Sundance"})
	if score != -4 {
		t.Errorf("free-text author: score = %d, want -4", score)
	}
	score, _ = m.Score(1, Subject{Authors: []string{"Sundance"}})
	if score != -4 {
		t.Errorf("normalized author: score = %d, want -4", score)
	}
}

// Scores are additive, and a global rule and a feed-scoped rule matching the
// same article both apply.
func TestScoresAreAdditive(t *testing.T) {
	m, err := New([]Rule{
		rule(1, AxisTitle, MatchSubstring, "open thread", -3),
		feedRule(2, 7, AxisTitle, MatchSubstring, "sunday", -2),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	score, fired := m.Score(7, Subject{Title: "Sunday - Open Thread"})
	if score != -5 {
		t.Errorf("score = %d, want -5", score)
	}
	if !slices.Equal(fired, []int64{1, 2}) {
		t.Errorf("fired = %v, want [1 2]", fired)
	}
}

// A feed-scoped rule must not touch another feed's articles.
func TestFeedScoping(t *testing.T) {
	m, err := New([]Rule{feedRule(1, 7, AxisTitle, MatchSubstring, "open thread", -3)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if score, _ := m.Score(7, Subject{Title: "Open Thread"}); score != -3 {
		t.Errorf("own feed: score = %d, want -3", score)
	}
	if score, _ := m.Score(8, Subject{Title: "Open Thread"}); score != 0 {
		t.Errorf("other feed: score = %d, want 0", score)
	}
}

// A user whose rules are all exact metadata matches keeps the SQL path
// untouched, which is the common case and must stay free.
func TestNoMatcherWhenSQLCanHandleEverything(t *testing.T) {
	m, err := New([]Rule{
		rule(1, AxisAuthor, MatchExact, "Sundance", -5),
		rule(2, AxisCategory, MatchExact, "Uncategorized", -5),
		rule(3, AxisTag, MatchExact, "Uncategorized", -5),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m != nil {
		t.Fatal("expected a nil matcher: SQL can match all three rules")
	}
	// A nil matcher is usable without a nil check.
	if !m.Empty() || m.NeedsMetadata() || m.NeedsContent() {
		t.Error("a nil matcher should report empty and need nothing")
	}
	if score, fired := m.Score(1, Subject{Author: "Sundance"}); score != 0 || fired != nil {
		t.Errorf("nil matcher scored %d %v, want 0 and nil", score, fired)
	}
}

// Once ANY rule needs Go, the matcher takes over all of them -- including the
// exact metadata rules SQL could have matched. Splitting the rule set would
// leave neither evaluator holding the whole sum the visibility gate compares
// against. The caller's side of that bargain is disabling the SQL rule join;
// if it does not, these rules are counted twice.
func TestOnePatternRuleTakesOverTheWholeSet(t *testing.T) {
	m, err := New([]Rule{
		rule(1, AxisAuthor, MatchExact, "Sundance", -5),
		rule(2, AxisTitle, MatchRegex, `(?i)open thread`, -3),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.Empty() {
		t.Fatal("expected a matcher")
	}
	if !m.NeedsMetadata() {
		t.Error("the exact author rule came along, so authors must be loaded for it")
	}

	score, fired := m.Score(1, Subject{Author: "Sundance", Title: "Sunday - Open Thread"})
	if score != -8 {
		t.Errorf("score = %d, want -8 (both rules)", score)
	}
	if !slices.Equal(fired, []int64{1, 2}) {
		t.Errorf("fired = %v, want [1 2]", fired)
	}
}

// Exact mode on a TEXT axis has no SQL implementation, so it does belong here.
func TestExactOnTextAxisIsEvaluatedHere(t *testing.T) {
	m, err := New([]Rule{rule(1, AxisTitle, MatchExact, "Open Thread", -5)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.Empty() {
		t.Fatal("exact-mode title rule should be evaluated in Go")
	}
	if score, _ := m.Score(1, Subject{Title: "Open Thread"}); score != -5 {
		t.Errorf("score = %d, want -5", score)
	}
}

func TestNewRejectsUncompilablePattern(t *testing.T) {
	if _, err := New([]Rule{rule(1, AxisTitle, MatchRegex, `(unclosed`, -1)}); err == nil {
		t.Fatal("expected New to reject an uncompilable pattern")
	}
}

func TestEmptyMatcher(t *testing.T) {
	m, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if !m.Empty() {
		t.Error("a matcher with no rules should report Empty")
	}
	if score, fired := m.Score(1, Subject{Title: "anything"}); score != 0 || fired != nil {
		t.Errorf("score = %d fired = %v, want 0 and nil", score, fired)
	}
}

// The evaluator reads article text supplied by a feed. It must be safe against
// whatever a feed puts there.
func TestHostileInput(t *testing.T) {
	m, err := New([]Rule{rule(1, AxisContent, MatchRegex, `(a+)+b`, -1)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A pattern that would backtrack catastrophically under a PCRE engine. RE2
	// runs it in linear time; if this ever hangs, the engine has been swapped.
	long := make([]byte, 40000)
	for i := range long {
		long[i] = 'a'
	}
	if score, _ := m.Score(1, Subject{Content: string(long)}); score != 0 {
		t.Errorf("score = %d, want 0", score)
	}
}

// Which axes a user's rules actually reference decides how much per-article
// data has to be assembled for them. A user with no metadata pattern rules
// should not cause authors and categories to be fetched.
func TestNeedsMetadata(t *testing.T) {
	textOnly, _ := New([]Rule{rule(1, AxisTitle, MatchRegex, `x`, -1)})
	if textOnly.NeedsMetadata() {
		t.Error("title-only rules should not require author/category lookups")
	}

	withCategory, _ := New([]Rule{rule(1, AxisCategory, MatchSubstring, "x", -1)})
	if !withCategory.NeedsMetadata() {
		t.Error("a category pattern rule requires the normalized categories")
	}

	withAuthor, _ := New([]Rule{rule(1, AxisAuthor, MatchSubstring, "x", -1)})
	if !withAuthor.NeedsMetadata() {
		t.Error("an author pattern rule requires the normalized authors")
	}
}
