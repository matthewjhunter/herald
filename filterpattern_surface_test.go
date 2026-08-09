package herald

import "testing"

// The surfaces beyond the reader's own lists (#274): the Fever sync API, which
// is somebody's phone, and the hidden count that tells a reader their filters
// are eating something.

func TestFeverItemsRespectFilterRules(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	addFilterTestArticles(t, engine, 1, feedID, "Open Thread", "Real News")

	// No rules: Fever sees everything.
	rows, err := engine.GetFeverItems(1, 0, 0, nil, 50)
	if err != nil {
		t.Fatalf("GetFeverItems: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("before rules: got %d items, want 2", len(rows))
	}

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "open thread", Score: -5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	setFilterThreshold(t, engine, 1, -1)

	rows, err = engine.GetFeverItems(1, 0, 0, nil, 50)
	if err != nil {
		t.Fatalf("GetFeverItems: %v", err)
	}
	if len(rows) != 1 || rows[0].Title != "Real News" {
		got := make([]string, len(rows))
		for i, r := range rows {
			got[i] = r.Title
		}
		t.Errorf("got %v, want only [Real News]: an article hidden in the reader must not sync to a phone", got)
	}
}

// Filtering must not leak across users on the sync API either.
func TestFeverItemsScopedToTheRuleOwner(t *testing.T) {
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

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "open thread", Score: -5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	setFilterThreshold(t, engine, 1, -1)
	setFilterThreshold(t, engine, 2, -1)

	if rows, _ := engine.GetFeverItems(1, 0, 0, nil, 50); len(rows) != 1 {
		t.Errorf("owner got %d items, want 1", len(rows))
	}
	rows, err := engine.GetFeverItems(2, 0, 0, nil, 50)
	if err != nil {
		t.Fatalf("GetFeverItems(user 2): %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("other user got %d items, want 2", len(rows))
	}
}

// A reader who cannot tell that filters are eating their feed has no way to
// discover a rule that is too broad.
func TestReaderGaugeReportsHiddenArticles(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	addFilterTestArticles(t, engine, 1, feedID,
		"Open Thread A", "Open Thread B", "Real News")
	gauge, err := engine.GetReaderGauge(1, 0)
	if err != nil {
		t.Fatalf("GetReaderGauge: %v", err)
	}
	if gauge.Hidden != 0 {
		t.Errorf("with no rules, Hidden = %d, want 0", gauge.Hidden)
	}

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "open thread", Score: -5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}

	// A rule without a threshold hides nothing, so there is nothing to report.
	if gauge, _ = engine.GetReaderGauge(1, 0); gauge.Hidden != 0 {
		t.Errorf("with the gate off, Hidden = %d, want 0", gauge.Hidden)
	}

	setFilterThreshold(t, engine, 1, -1)
	gauge, err = engine.GetReaderGauge(1, 0)
	if err != nil {
		t.Fatalf("GetReaderGauge: %v", err)
	}
	if gauge.Hidden != 2 {
		t.Errorf("Hidden = %d, want 2", gauge.Hidden)
	}
}

func TestReaderGaugeHiddenIsScopedToTheFeed(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	noisy := subscribeDirect(t, engine, 1, "https://example.com/noisy.xml", "Noisy")
	quiet := subscribeDirect(t, engine, 1, "https://example.com/quiet.xml", "Quiet")
	addFilterTestArticles(t, engine, 1, noisy, "Open Thread A", "Open Thread B")
	addFilterTestArticles(t, engine, 1, quiet, "Real News")

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "open thread", Score: -5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	setFilterThreshold(t, engine, 1, -1)

	if g, _ := engine.GetReaderGauge(1, noisy); g.Hidden != 2 {
		t.Errorf("noisy feed: Hidden = %d, want 2", g.Hidden)
	}
	if g, _ := engine.GetReaderGauge(1, quiet); g.Hidden != 0 {
		t.Errorf("quiet feed: Hidden = %d, want 0", g.Hidden)
	}
}

// The gauge's hidden count must not be charged to a user who has no rules.
func TestReaderGaugeHiddenIsPerUser(t *testing.T) {
	engine, cleanup := newTestEngine(t)
	defer cleanup()

	feedID := subscribeDirect(t, engine, 1, "https://example.com/feed.xml", "Feed")
	if err := engine.store.SubscribeUserToFeed(2, feedID); err != nil {
		t.Fatalf("SubscribeUserToFeed(2): %v", err)
	}
	ids := addFilterTestArticles(t, engine, 1, feedID, "Open Thread")
	for _, id := range ids {
		if err := engine.store.SetInterestScore(2, id, 5.0, "test-model", "hash"); err != nil {
			t.Fatalf("SetInterestScore(user 2): %v", err)
		}
	}

	if _, err := engine.AddFilterRule(1, FilterRule{
		Axis: AxisTitle, MatchMode: MatchSubstring, Value: "open thread", Score: -5,
	}); err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	setFilterThreshold(t, engine, 1, -1)

	if g, _ := engine.GetReaderGauge(1, 0); g.Hidden != 1 {
		t.Errorf("rule owner: Hidden = %d, want 1", g.Hidden)
	}
	if g, _ := engine.GetReaderGauge(2, 0); g.Hidden != 0 {
		t.Errorf("other user: Hidden = %d, want 0", g.Hidden)
	}
}
