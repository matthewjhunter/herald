// Package filtermatch evaluates the pattern half of a user's filter rules
// (#274).
//
// Filter rules are split between two evaluators. An exact comparison against a
// metadata axis is an indexed CITEXT equality and stays in SQL, where it costs
// nothing. Everything else -- any substring or regex rule, and any rule against
// the article's own text -- is evaluated here, in Go, against rows the query
// has already returned.
//
// Go rather than Postgres because article text is attacker-supplied. Postgres
// regex backtracks, so a pattern that is merely careless, run over content a
// feed controls, is a wedged page load. Go's regexp is RE2: linear in the input
// with no catastrophic backtracking. The cost that remains is I/O, not CPU.
//
// Read time rather than ingest time because a filter rule should take effect
// when it is saved. Materializing matches would mean a new rule did nothing
// until the next processing cycle -- the complaint #259 was filed about, in a
// new costume. Materialization is available later as a pure cache if profiling
// asks for it; it must never become the semantics.
//
// The split is enforced from both sides: filterRuleMatch in internal/storage
// qualifies on match_mode = 'exact', and New drops any rule that belongs to
// SQL. A rule counted by both would have its score applied twice.
package filtermatch

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Match modes and axes. These mirror the constants in internal/storage; this
// package deliberately does not import it, so the hot path stays free of the
// storage layer's dependencies.
const (
	MatchExact     = "exact"
	MatchSubstring = "substring"
	MatchRegex     = "regex"

	AxisAuthor   = "author"
	AxisCategory = "category"
	AxisTag      = "tag"
	AxisTitle    = "title"
	AxisSummary  = "summary"
	AxisContent  = "content"
)

// Rule is the subset of a filter rule the evaluator needs.
type Rule struct {
	ID        int64
	FeedID    *int64 // nil = applies to every feed
	Axis      string
	MatchMode string
	Value     string
	Score     int
}

// evaluatedInGo reports whether this rule belongs to this package rather than
// to the SQL matcher. Exact matching on a metadata axis is the SQL half; the
// text axes have no SQL implementation at all, so even an exact rule against
// one lands here.
func (r Rule) evaluatedInGo() bool {
	if r.MatchMode != "" && r.MatchMode != MatchExact {
		return true
	}
	switch r.Axis {
	case AxisAuthor, AxisCategory, AxisTag:
		return false
	default:
		return true
	}
}

// Subject is the article text a rule is matched against.
//
// Author and Authors are both present because feeds disagree about where the
// byline goes: many populate only the free-text field the item carried, others
// only a structured author element. A pattern rule on the author axis matches
// either.
type Subject struct {
	Title      string
	Summary    string
	Content    string
	Author     string   // the item's free-text author, as published
	Authors    []string // normalized authors, from article_authors
	Categories []string // normalized categories and tags, from article_categories
}

// Matcher holds one user's compiled pattern rules. It is immutable once built
// and safe for concurrent use.
//
// Rules are grouped by axis rather than kept in one list, so each field is
// read, lowercased and prefiltered once per article instead of once per rule.
// With rules at the per-user quota that is the difference between a page load
// the reader notices and one they do not.
type Matcher struct {
	axes          []*axisRules
	count         int
	needsMetadata bool
}

type compiledRule struct {
	Rule
	order int            // position among this user's rules, for stable reporting
	re    *regexp.Regexp // nil unless MatchMode is MatchRegex
}

// axisRules is every rule matching on one axis, plus the shortcuts that let
// most articles be dismissed without running them individually.
type axisRules struct {
	axis string
	// anyRe is the union of this axis's regex patterns. RE2 matches an
	// alternation in a single pass, so when it does not match -- the common
	// case, since filters exist to catch a minority of articles -- every regex
	// rule on the axis is answered at once. nil when there are none, or when
	// the union failed to compile, in which case the rules simply run
	// individually.
	anyRe        *regexp.Regexp
	rules        []compiledRule
	needsLowered bool // some rule here is exact or substring, which compare case-insensitively
}

// compiledPatterns memoizes compiled expressions across users and requests.
// Patterns are immutable strings, so there is nothing to invalidate, and the
// per-user pattern quota bounds how many distinct ones can accumulate.
var compiledPatterns sync.Map // string -> *regexp.Regexp

func compile(pattern string) (*regexp.Regexp, error) {
	if v, ok := compiledPatterns.Load(pattern); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	compiledPatterns.Store(pattern, re)
	return re, nil
}

// New builds a Matcher for a user's rules, or returns nil when SQL can handle
// all of them.
//
// A nil Matcher is the instruction to use the SQL path unchanged, and it is the
// common case: most users have no pattern rules at all. When even one rule does
// need Go, the Matcher takes over ALL of that user's rules, including the exact
// metadata ones SQL could have matched, and the caller must then disable the
// SQL rule join and the SQL visibility gate.
//
// All-or-nothing rather than splitting the work is forced by the visibility
// gate, which compares the SUM of a user's matching rules against a threshold.
// A split leaves neither side holding the whole sum: SQL cannot see the pattern
// rules, and Go would have to be told the SQL subtotal for every row. Doing
// every rule in one place is both simpler and the only version that is right.
// The cost is that these users re-evaluate their exact rules in Go, which is a
// handful of string comparisons.
//
// A pattern that does not compile is an error rather than a rule that quietly
// matches nothing. Rules are validated at save time, so reaching this is a bug
// or a hand-edited database, and either deserves to be noisy.
func New(rules []Rule) (*Matcher, error) {
	if !slices.ContainsFunc(rules, Rule.evaluatedInGo) {
		return nil, nil
	}
	m := &Matcher{}
	byAxis := make(map[string]*axisRules)
	for _, r := range rules {
		cr := compiledRule{Rule: r, order: m.count}
		if r.MatchMode == MatchRegex {
			re, err := compile(r.Value)
			if err != nil {
				return nil, fmt.Errorf("filter rule %d: invalid pattern %q: %w", r.ID, r.Value, err)
			}
			cr.re = re
		}
		if r.Axis == AxisAuthor || r.Axis == AxisCategory || r.Axis == AxisTag {
			m.needsMetadata = true
		}

		ax := byAxis[r.Axis]
		if ax == nil {
			ax = &axisRules{axis: r.Axis}
			byAxis[r.Axis] = ax
			m.axes = append(m.axes, ax)
		}
		ax.rules = append(ax.rules, cr)
		if r.MatchMode != MatchRegex {
			ax.needsLowered = true
		}
		m.count++
	}
	for _, ax := range m.axes {
		ax.buildUnion()
	}
	return m, nil
}

// buildUnion compiles the alternation of this axis's regex patterns. It is a
// prefilter only: a union match still runs the individual rules, because the
// caller needs to know WHICH fired. The win is on the articles that match
// nothing, which is nearly all of them.
//
// Skipped below two patterns, where the union is the same work twice. A union
// that will not compile is not an error -- the individual patterns already
// compiled, so the rules are correct and only the shortcut is lost.
func (ax *axisRules) buildUnion() {
	var patterns []string
	for _, r := range ax.rules {
		if r.re != nil {
			patterns = append(patterns, "(?:"+r.Value+")")
		}
	}
	if len(patterns) < 2 {
		return
	}
	if re, err := compile(strings.Join(patterns, "|")); err == nil {
		ax.anyRe = re
	}
}

// Empty reports whether there is nothing for this matcher to do. A nil
// Matcher -- what New returns when SQL can handle every rule -- is empty, so
// callers can hold one without nil checks at each use.
func (m *Matcher) Empty() bool { return m == nil || m.count == 0 }

// NeedsMetadata reports whether any rule matches on the author, category or
// tag axes, which live in their own tables and cost an extra query to load. A
// user whose rules are all on title or summary should not pay for it.
func (m *Matcher) NeedsMetadata() bool { return m != nil && m.needsMetadata }

// NeedsContent reports whether any rule matches on the article body. Callers
// that can avoid selecting it should ask first.
func (m *Matcher) NeedsContent() bool {
	return m != nil && slices.ContainsFunc(m.axes, func(ax *axisRules) bool { return ax.axis == AxisContent })
}

// Score returns the summed score of every rule matching this article, and the
// ids of the rules that fired, in rule order.
//
// Scores are additive and order-independent: a global rule and a feed-scoped
// rule matching the same article both apply, which is the same arithmetic the
// SQL side does with SUM.
func (m *Matcher) Score(feedID int64, s Subject) (int, []int64) {
	if m == nil {
		return 0, nil
	}
	var (
		total int
		fired []compiledRule
	)
	for _, ax := range m.axes {
		total += ax.score(feedID, s, &fired)
	}
	if fired == nil {
		return total, nil
	}
	// Report in rule order rather than axis order, so the caller sees the same
	// sequence the user's rule list has.
	slices.SortFunc(fired, func(a, b compiledRule) int { return a.order - b.order })
	ids := make([]int64, len(fired))
	for i, r := range fired {
		ids[i] = r.ID
	}
	return total, ids
}

// score evaluates one axis, reading and folding its text once for all of the
// axis's rules.
func (ax *axisRules) score(feedID int64, s Subject, fired *[]compiledRule) int {
	var values []string
	switch ax.axis {
	case AxisTitle:
		values = []string{s.Title}
	case AxisSummary:
		values = []string{s.Summary}
	case AxisContent:
		values = []string{s.Content}
	case AxisAuthor:
		// Both, because feeds disagree about where the byline goes.
		values = append(append(make([]string, 0, len(s.Authors)+1), s.Author), s.Authors...)
	case AxisCategory, AxisTag:
		values = s.Categories
	default:
		return 0
	}

	f := fields{values: values}
	if ax.needsLowered {
		f.lowered = make([]string, len(values))
		for i, v := range values {
			f.lowered[i] = strings.ToLower(v)
		}
	}

	// One alternation pass answers every regex rule on this axis at once when
	// it fails, which is the usual outcome.
	skipRegex := ax.anyRe != nil && !f.anyMatches(ax.anyRe)

	total := 0
	for _, r := range ax.rules {
		if r.FeedID != nil && *r.FeedID != feedID {
			continue
		}
		if r.re != nil && skipRegex {
			continue
		}
		if r.matches(f) {
			total += r.Score
			*fired = append(*fired, r)
		}
	}
	return total
}

// fields is one axis's text for one article: the values as published, plus a
// lowercased copy when some rule needs one. Folding a 4KB article body once per
// axis instead of once per rule is most of why this type exists.
type fields struct {
	values  []string
	lowered []string
}

func (f fields) anyMatches(re *regexp.Regexp) bool {
	return slices.ContainsFunc(f.values, re.MatchString)
}

// matches compares a rule against every value on its axis.
//
// Exact and substring are case-insensitive, matching the CITEXT columns the
// SQL half compares against; a user who writes "open thread" expects it to
// catch "Open Thread". Regex is left exactly as written, because case is part
// of what a pattern expresses -- (?i) is how a user asks for the other
// behaviour, and silently forcing it would take that choice away.
func (r compiledRule) matches(f fields) bool {
	if r.MatchMode == MatchRegex {
		return r.re != nil && f.anyMatches(r.re)
	}
	needle := strings.ToLower(r.Value)
	for i, v := range f.values {
		if v == "" {
			continue
		}
		if r.MatchMode == MatchSubstring {
			if strings.Contains(f.lowered[i], needle) {
				return true
			}
		} else if f.lowered[i] == needle { // MatchExact, and "" meaning it
			return true
		}
	}
	return false
}
