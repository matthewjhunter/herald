package herald

import (
	"fmt"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

// End-to-end hiding through the engine (#274). The evaluator itself is tested
// in internal/filtermatch; what these cover is the wiring -- that a pattern
// rule reaches the listing paths, that the SQL gate is off when Go owns the
// rules, that paging stays consistent, and that one user's rules cannot touch
// another's reader.

// addArticles inserts n articles with the given titles and marks them scored,
// so they behave like articles curation has already seen.
func addFilterTestArticles(t *testing.T, e *Engine, userID, feedID int64, titles ...string) []int64 {
	t.Helper()
	ids := make([]int64, len(titles))
	for i, title := range titles {
		// Descending publish times so list order matches the argument order.
		published := time.Now().Add(-time.Duration(i) * time.Hour)
		id, err := e.store.AddArticle(&storage.Article{
			FeedID:        feedID,
			GUID:          fmt.Sprintf("guid-%s-%d", title, i),
			Title:         title,
			URL:           fmt.Sprintf("https://example.com/%d", i),
			Content:       "Article body.",
			Summary:       "Article summary.",
			Author:        "Sundance",
			PublishedDate: &published,
		})
		if err != nil {
			t.Fatalf("AddArticle(%q): %v", title, err)
		}
		if err := e.store.SetInterestScore(userID, id, 5.0, "test-model", "hash"); err != nil {
			t.Fatalf("SetInterestScore: %v", err)
		}
		ids[i] = id
	}
	return ids
}

func setFilterThreshold(t *testing.T, e *Engine, userID int64, threshold int) {
	t.Helper()
	if err := e.SetPreference(userID, "filter_threshold", fmt.Sprint(threshold)); err != nil {
		t.Fatalf("SetPreference(filter_threshold): %v", err)
	}
}

func titlesOf(articles []Article) []string {
	out := make([]string, len(articles))
	for i, a := range articles {
		out[i] = a.Title
	}
	return out
}

// The case the feature exists for, end to end.
func TestPatternRuleHidesRecurringPosts(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "The Last Refuge")
	addFilterTestArticles(t, engine, 1, feedID,
		"Sunday August 9th - Open Thread",
		"August 9th - 2026 Presidential Politics - Trump Administration Day 567",
		"Senate Votes Overnight to Confirm Todd Blanche as U.S. Attorney General",
	)

	// Without a threshold the gate is off, so nothing is hidden.
	articles, err := engine.GetUnreadArticles(1, 50, 0, false)
	if err != nil {
		t.Fatalf("GetUnreadArticles: %v", err)
	}
	if len(articles) != 3 {
		t.Fatalf("before rules: got %d articles, want 3", len(articles))
	}

	feed := feedID
	for _, pattern := range []string{`(?i)\bopen thread\b`, `(?i)trump administration day \d+`} {
		if _, err := engine.AddFilterRule(1, FilterRule{
			FeedID: &feed, Axis: AxisTitle, MatchMode: MatchRegex, Value: pattern, Score: -5,
		}); err != nil {
			t.Fatalf("AddFilterRule(%q): %v", pattern, err)
		}
	}
	setFilterThreshold(t, engine, 1, 0)

	// A threshold of 0 disables the gate, matching the pre-existing convention.
	if articles, _ = engine.GetUnreadArticles(1, 50, 0, false); len(articles) != 3 {
		t.Errorf("threshold 0: got %d articles, want 3 (gate disabled)", len(articles))
	}

	// Rules summing to -5 fall below a threshold of -1.
	setFilterThreshold(t, engine, 1, -1)

	articles, err = engine.GetUnreadArticles(1, 50, 0, false)
	if err != nil {
		t.Fatalf("GetUnreadArticles: %v", err)
	}
	if len(articles) != 1 || articles[0].Title != "Senate Votes Overnight to Confirm Todd Blanche as U.S. Attorney General" {
		t.Errorf("got %v, want only the Senate article", titlesOf(articles))
	}
}

// Every listing path has to honour the rules, not just the one that was wired
// first. A path that forgets stops filtering silently.
func TestPatternRuleAppliesToEveryListingPath(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	ids := addFilterTestArticles(t, engine, 1, feedID, "Open Thread", "Real News")
	if err := engine.StarArticle(1, ids[0], true); err != nil {
		t.Fatalf("StarArticle: %v", err)
	}
	if err := engine.StarArticle(1, ids[1], true); err != nil {
		t.Fatalf("StarArticle: %v", err)
	}

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "open thread", Score: -5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	setFilterThreshold(t, engine, 1, -1)

	for _, tc := range []struct {
		name string
		list func() ([]Article, error)
	}{
		{"unread", func() ([]Article, error) { return engine.GetUnreadArticles(1, 50, 0, false) }},
		{"by feed", func() ([]Article, error) { return engine.GetUnreadArticlesByFeed(1, feedID, 50, 0, false) }},
		{"starred", func() ([]Article, error) { return engine.GetStarredArticles(1, 50, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			articles, err := tc.list()
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(articles) != 1 || articles[0].Title != "Real News" {
				t.Errorf("got %v, want only [Real News]", titlesOf(articles))
			}
		})
	}
}

// Pages are composed after filtering, so an offset counted in unfiltered rows
// would skip or repeat articles as hidden rows shift the boundary.
func TestFilteredPaginationIsConsistent(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	// Alternate hidden and kept so naive offsetting is guaranteed to go wrong.
	var titles []string
	for i := range 10 {
		titles = append(titles, fmt.Sprintf("Open Thread %d", i), fmt.Sprintf("Keeper %d", i))
	}
	addFilterTestArticles(t, engine, 1, feedID, titles...)

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "open thread", Score: -5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	setFilterThreshold(t, engine, 1, -1)

	seen := map[string]bool{}
	for page := range 5 {
		articles, err := engine.GetUnreadArticles(1, 2, page*2, false)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, a := range articles {
			if seen[a.Title] {
				t.Errorf("page %d repeated %q", page, a.Title)
			}
			seen[a.Title] = true
		}
	}
	if len(seen) != 10 {
		t.Errorf("paged through %d articles, want all 10 keepers: %v", len(seen), seen)
	}
	for i := range 10 {
		if !seen[fmt.Sprintf("Keeper %d", i)] {
			t.Errorf("Keeper %d was never returned", i)
		}
	}
}

// A filter rule leaking across users is a security bug, not a display bug.
func TestPatternRulesAreScopedToTheirOwner(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	if err := engine.store.SubscribeUserToFeed(2, feedID); err != nil {
		t.Fatalf("SubscribeUserToFeed(2): %v", err)
	}
	ids := addFilterTestArticles(t, engine, 1, feedID, "Open Thread", "Real News")
	for _, id := range ids {
		if err := engine.store.SetInterestScore(2, id, 5.0, "test-model", "hash"); err != nil {
			t.Fatalf("SetInterestScore(user 2): %v", err)
		}
	}

	// User 1 hides open threads. User 2 has no rules at all.
	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "open thread", Score: -5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	setFilterThreshold(t, engine, 1, -1)
	setFilterThreshold(t, engine, 2, -1)

	if articles, _ := engine.GetUnreadArticles(1, 50, 0, false); len(articles) != 1 {
		t.Errorf("owner sees %v, want only the unfiltered article", titlesOf(articles))
	}
	articles, err := engine.GetUnreadArticles(2, 50, 0, false)
	if err != nil {
		t.Fatalf("GetUnreadArticles(user 2): %v", err)
	}
	if len(articles) != 2 {
		t.Errorf("other user sees %v, want both articles: one user's rule must not filter another's reader", titlesOf(articles))
	}
}

// A user with only exact metadata rules must keep the untouched SQL path: no
// pattern evaluation, no extra queries, and the same results as before #274.
func TestExactOnlyRulesKeepTheSQLPath(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	ids := addFilterTestArticles(t, engine, 1, feedID, "Boosted", "Ordinary")
	if err := engine.store.StoreArticleAuthors(ids[0], []storage.ArticleAuthor{{Name: "Alice"}}); err != nil {
		t.Fatalf("StoreArticleAuthors: %v", err)
	}

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisAuthor, MatchMode: MatchExact, Value: "Alice", Score: 5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	setFilterThreshold(t, engine, 1, 1)

	if p := engine.filters().plan(1); p.matcher != nil || p.sqlThreshold == nil {
		t.Errorf("exact-only rules should plan the SQL path: matcher=%v sqlThreshold=%v", p.matcher, p.sqlThreshold)
	}

	articles, err := engine.GetUnreadArticles(1, 50, 0, false)
	if err != nil {
		t.Fatalf("GetUnreadArticles: %v", err)
	}
	if len(articles) != 1 || articles[0].Title != "Boosted" {
		t.Errorf("got %v, want only [Boosted]", titlesOf(articles))
	}
}

// Once any rule needs Go, Go owns the whole set -- including the exact ones SQL
// could have matched. If the SQL join were left on, those rules would count
// twice and the gate would compare against the wrong number.
func TestExactAndPatternRulesAreNotCountedTwice(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	ids := addFilterTestArticles(t, engine, 1, feedID, "Open Thread", "Real News")
	for _, id := range ids {
		if err := engine.store.StoreArticleAuthors(id, []storage.ArticleAuthor{{Name: "Sundance"}}); err != nil {
			t.Fatalf("StoreArticleAuthors: %v", err)
		}
	}

	// -2 from the exact author rule, -2 more from the pattern rule on the open
	// thread. Counted once each, the open thread sits at -4 and the other at
	// -2; a threshold of -3 separates them. Counted twice, both fall below it.
	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisAuthor, MatchMode: MatchExact, Value: "Sundance", Score: -2,
	}); err != nil {
		t.Fatalf("AddFilterRule(exact): %v", err)
	}
	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "open thread", Score: -2,
	}); err != nil {
		t.Fatalf("AddFilterRule(pattern): %v", err)
	}
	setFilterThreshold(t, engine, 1, -3)

	articles, err := engine.GetUnreadArticles(1, 50, 0, false)
	if err != nil {
		t.Fatalf("GetUnreadArticles: %v", err)
	}
	if len(articles) != 1 || articles[0].Title != "Real News" {
		t.Errorf("got %v, want only [Real News] -- rules are being double counted", titlesOf(articles))
	}
}
