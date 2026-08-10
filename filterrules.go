package herald

import (
	"log"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/matthewjhunter/herald/internal/filtermatch"
	"github.com/matthewjhunter/herald/internal/storage"
)

// Read-time evaluation of pattern filter rules (#274).
//
// This lives on a type of its own rather than on Engine because two of the
// consumers -- the CLI's fetch-cycle and cron notification paths -- hold a
// Store and no Engine. Rules that shift the web ranking but not what gets
// notified would be the #259 complaint over again, so both routes go through
// the same code.

// ruleFilter applies a user's filter rules to listing and ranking queries.
type ruleFilter struct {
	store storage.Store
	cfg   *storage.Config
}

func (e *Engine) filters() ruleFilter { return ruleFilter{store: e.store, cfg: e.config} }

// filterPlan is how one request will apply a user's filter rules.
//
// The two evaluators are mutually exclusive by construction. Either SQL does
// the whole job -- sqlThreshold set, matcher nil, exactly as before #274 -- or
// Go does, with the SQL rule join and gate switched off. Never both: the gate
// compares the SUM of a user's matching rules against a threshold, so a rule
// set split across two evaluators leaves neither holding the number being
// compared.
type filterPlan struct {
	matcher      *filtermatch.Matcher // non-nil when Go owns this user's rules
	sqlThreshold *int                 // gate for the SQL path; nil when Go owns it or the gate is off
	goThreshold  *int                 // gate for the Go path; nil when the gate is off
	applySQL     bool                 // whether the SQL rule join should compute a rule-adjusted score
}

// hides reports whether the Go evaluator is doing visibility work. When it is
// not, the date-ordered listing paths can return the store's rows untouched.
func (p filterPlan) hides() bool { return p.matcher != nil && p.goThreshold != nil }

// plan decides how a request applies this user's rules.
//
// A user with no pattern rules -- the common case -- gets a plan indis-
// tinguishable from the pre-#274 behaviour, with no extra queries and no Go
// evaluation. Errors are not fatal: a filter that cannot be resolved falls back
// to showing everything, because failing to hide an article is a far better
// outcome than failing to render the page.
func (rf ruleFilter) plan(userID int64) filterPlan {
	rules, err := rf.store.GetFilterRules(userID, nil)
	if err != nil {
		log.Printf("herald: filter rules unavailable for user %d, showing everything: %v", userID, err)
		return filterPlan{}
	}
	if len(rules) == 0 {
		return filterPlan{}
	}
	threshold := rf.threshold(userID)

	fmRules := make([]filtermatch.Rule, len(rules))
	for i, r := range rules {
		fmRules[i] = filtermatch.Rule{
			ID: r.ID, FeedID: r.FeedID, Axis: r.Axis,
			MatchMode: r.MatchMode, Value: r.Value, Score: r.Score,
		}
	}
	matcher, err := filtermatch.New(fmRules)
	if err != nil {
		// Patterns are compiled at save time, so this is a bug or a hand-edited
		// row. Log it and fall back to the half SQL can still do.
		log.Printf("herald: filter rules for user %d will not compile, using exact rules only: %v", userID, err)
		return filterPlan{sqlThreshold: threshold, applySQL: true}
	}
	if matcher.Empty() {
		return filterPlan{sqlThreshold: threshold, applySQL: true}
	}
	return filterPlan{matcher: matcher, goThreshold: threshold}
}

// threshold returns the user's visibility threshold, or nil when the gate is
// off. Zero means disabled, which is the pre-existing convention.
func (rf ruleFilter) threshold(userID int64) *int {
	prefs, err := rf.store.GetAllUserPreferences(userID)
	if err != nil {
		return nil
	}
	v, ok := prefs["filter_threshold"]
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n == 0 {
		return nil
	}
	return &n
}

// subjects assembles the text each article will be matched against, loading
// normalized authors and categories only when some rule needs them.
func (rf ruleFilter) subjects(articles []storage.Article, m *filtermatch.Matcher) []filtermatch.Subject {
	var authors, categories map[int64][]string
	if m.NeedsMetadata() {
		ids := make([]int64, len(articles))
		for i, a := range articles {
			ids[i] = a.ID
		}
		var err error
		authors, categories, err = rf.store.GetArticleMetadataBatch(ids)
		if err != nil {
			// Rules on the metadata axes will not fire. Say so rather than
			// silently under-filtering.
			log.Printf("herald: article metadata unavailable for filtering: %v", err)
		}
	}

	subjects := make([]filtermatch.Subject, len(articles))
	for i, a := range articles {
		subjects[i] = filtermatch.Subject{
			Title:      a.Title,
			Summary:    a.Summary,
			Content:    a.Content,
			Author:     a.Author,
			Authors:    authors[a.ID],
			Categories: categories[a.ID],
		}
	}
	return subjects
}

// window is how many rows to examine to compose one page of `limit` articles
// starting at `offset`. Rows are hidden after the query returns, so a page has
// to fetch more than it needs to stand a chance of filling.
func (rf ruleFilter) window(limit, offset int) int {
	factor := max(rf.cfg.Limits.FilterOverfetchFactor, 1)
	w := (offset + limit) * factor
	if maxScan := rf.cfg.Limits.FilterMaxScan; maxScan > 0 && w > maxScan {
		w = maxScan
	}
	return w
}

// listFiltered runs a date-ordered listing query through the Go evaluator.
//
// fetch takes a window size and returns rows from the top of the list; the
// offset is applied here, AFTER filtering, because an offset counted in
// unfiltered rows would skip or repeat articles as hidden rows shift the page
// boundary. That is why the window starts at zero and covers offset+limit
// rather than paging in the store.
//
// A page may come back short of limit under an aggressive filter, which is the
// deliberate trade against an unbounded fetch on every page load. See #277 --
// batch reading is where page composition gets designed properly.
func (rf ruleFilter) listFiltered(p filterPlan, limit, offset int, fetch func(window int) ([]storage.Article, error)) ([]storage.Article, error) {
	window := rf.window(limit, offset)
	articles, err := fetch(window)
	if err != nil {
		return nil, err
	}
	if len(articles) == window {
		// Silent truncation would read as "that is all there is".
		log.Printf("herald: filter scan window of %d rows filled; later pages may be incomplete", window)
	}

	subjects := rf.subjects(articles, p.matcher)
	kept := make([]storage.Article, 0, min(limit, len(articles)))
	for i, a := range articles {
		score, _ := p.matcher.Score(a.FeedID, subjects[i])
		if score < *p.goThreshold {
			continue
		}
		kept = append(kept, a)
		if len(kept) >= offset+limit {
			break
		}
	}
	if offset >= len(kept) {
		return nil, nil
	}
	return kept[offset:], nil
}

// highInterest returns unread articles whose rule-adjusted score reaches the
// threshold, ranked as the reader's interest list ranks them.
//
// On the SQL path this is the pre-#274 query unchanged. On the Go path the
// arithmetic moves out of SQL entirely: the store returns raw interest scores,
// the rule sum is added here, the result is clamped to the 0-10 scale every
// consumer reads, then decayed for recency and sorted. Clamping is why the raw
// score is needed -- it is not invertible, so a clamped score cannot be
// adjusted after the fact.
//
// The candidate window is ordered by undecorated score, so an article the
// model rated poorly and a rule boosts hard can fall outside it. Rules that
// demote -- which is what nearly all of them do -- are unaffected.
// applyGate follows each caller's pre-existing behaviour: the reader's list
// hides what the threshold hides, while the briefing and the CLI notification
// paths have always ranked with rules but never hidden, and still do.
func (rf ruleFilter) highInterest(userID int64, threshold float64, limit, offset int, applyGate bool) ([]storage.Article, []float64, error) {
	p := rf.plan(userID)
	if !applyGate {
		p.sqlThreshold, p.goThreshold = nil, nil
	}
	if p.matcher == nil {
		return rf.store.GetArticlesByInterestScore(userID, threshold, limit, offset, p.sqlThreshold, p.applySQL)
	}

	window := rf.window(limit, offset)
	articles, raw, err := rf.store.GetScoredUnreadArticles(userID, window)
	if err != nil {
		return nil, nil, err
	}
	if len(articles) == window {
		log.Printf("herald: interest ranking scanned its full window of %d rows; lower-ranked matches may be missing", window)
	}

	type scored struct {
		article storage.Article
		score   float64
		decayed float64
	}
	subjects := rf.subjects(articles, p.matcher)
	now := time.Now()
	ranked := make([]scored, 0, len(articles))
	for i, a := range articles {
		ruleScore, _ := p.matcher.Score(a.FeedID, subjects[i])
		if p.goThreshold != nil && ruleScore < *p.goThreshold {
			continue // hidden by the visibility gate
		}
		// Same arithmetic as effectiveScoreExpr: add, then clamp to 0-10.
		score := math.Min(10, math.Max(0, raw[i]+float64(ruleScore)))
		if score < threshold {
			continue
		}
		ranked = append(ranked, scored{
			article: a,
			score:   score,
			decayed: score * storage.RecencyDecay(a.PublishedDate, a.FetchedDate, now),
		})
	}

	// Stable so that equal scores keep the store's ordering rather than
	// shuffling between page loads.
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].decayed > ranked[j].decayed })
	if offset >= len(ranked) {
		return nil, nil, nil
	}
	ranked = ranked[offset:min(offset+limit, len(ranked))]

	outArticles := make([]storage.Article, len(ranked))
	outScores := make([]float64, len(ranked))
	for i, r := range ranked {
		outArticles[i] = r.article
		outScores[i] = r.decayed
	}
	return outArticles, outScores, nil
}

// HighInterestArticles returns a user's rule-adjusted high-interest articles
// for a caller that has a Store but no Engine.
//
// The CLI's notification paths are the reason this is exported: a rule that
// changes the web ranking but not what gets sent would leave the surface that
// actually reaches the reader unfiltered.
func HighInterestArticles(store storage.Store, cfg *storage.Config, userID int64, threshold float64, limit, offset int) ([]storage.Article, []float64, error) {
	// The visibility gate stays off here, as it always has been on this path.
	return ruleFilter{store: store, cfg: cfg}.highInterest(userID, threshold, limit, offset, false)
}

// hiddenUnreadCount reports how many unread articles this user's filter rules
// are currently suppressing, for the whole reader or one feed.
//
// The reader's unread counts have never applied the visibility gate, so a
// filtered list has always been able to show fewer articles than the badge
// promises. Rather than make the counts exact -- which would mean evaluating
// every unread article on every page load -- the discrepancy is surfaced.
// "12 unread, 7 hidden" tells the reader something true and useful, and gives
// them a reason to go and look at what their rules are eating.
//
// Counted over the same bounded window everything else here uses, so it is an
// indicator rather than an audited figure: with more unread articles than the
// window covers, it undercounts.
func (rf ruleFilter) hiddenUnreadCount(userID, feedID int64) int {
	p := rf.plan(userID)
	if !p.hides() {
		return 0
	}
	window := rf.window(rf.cfg.Limits.FilterMaxScan, 0)

	var (
		articles []storage.Article
		err      error
	)
	if feedID > 0 {
		articles, err = rf.store.GetUnreadArticlesByFeed(userID, feedID, window, 0, nil, false)
	} else {
		articles, err = rf.store.GetUnreadArticlesForUser(userID, window, 0, nil, false)
	}
	if err != nil {
		return 0
	}

	subjects := rf.subjects(articles, p.matcher)
	hidden := 0
	for i, a := range articles {
		if score, _ := p.matcher.Score(a.FeedID, subjects[i]); score < *p.goThreshold {
			hidden++
		}
	}
	return hidden
}
