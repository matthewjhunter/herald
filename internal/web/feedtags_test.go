package web

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
)

// TestParseDigestFormTags parses both explicit feeds and followed tags.
func TestParseDigestFormTags(t *testing.T) {
	form := url.Values{
		"name":               {"Sec Brief"},
		"schedule":           {"daily"},
		"min_interest_score": {"7.5"},
		"max_articles":       {"50"},
		"feed_ids":           {"12", "47", "bad"},
		"tag_names":          {"security", "  ", "news"},
		"prompt":             {"  do it  "},
	}
	req := httptest.NewRequest("POST", "/newsletters", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	name, schedule, _, prompt, cfg := parseDigestForm(req)
	if name != "Sec Brief" || schedule != "daily" {
		t.Errorf("name/schedule = %q/%q", name, schedule)
	}
	if prompt != "do it" {
		t.Errorf("prompt = %q, want trimmed 'do it'", prompt)
	}
	if len(cfg.IncludeFeeds) != 2 || cfg.IncludeFeeds[0] != 12 || cfg.IncludeFeeds[1] != 47 {
		t.Errorf("IncludeFeeds = %v, want [12 47] (bad dropped)", cfg.IncludeFeeds)
	}
	if len(cfg.IncludeTags) != 2 || cfg.IncludeTags[0] != "security" || cfg.IncludeTags[1] != "news" {
		t.Errorf("IncludeTags = %v, want [security news] (blank dropped)", cfg.IncludeTags)
	}
	if cfg.MinInterestScore != 7.5 || cfg.MaxArticles != 50 {
		t.Errorf("min/max = %v/%d", cfg.MinInterestScore, cfg.MaxArticles)
	}
}

// TestNewsletterListFragmentPicker exercises the edit-form picker inside the real
// newsletter loop: per-feed FeedTags lookup, tag pre-selection, and the summary line.
func TestNewsletterListFragmentPicker(t *testing.T) {
	tags := []string{"Security"}
	data := newslettersManageData{
		Feeds:    []herald.Feed{{ID: 12, Title: "Krebs"}, {ID: 47, Title: "Sports"}},
		FeedTags: map[int64][]string{12: {"Security"}},
		AllTags:  []string{"Security", "Sports"},
		Newsletters: []storage.Newsletter{{
			ID: 5, Name: "Sec Brief", Schedule: "daily",
			Config: storage.NewsletterConfig{MaxArticles: 50, IncludeFeeds: []int64{47}, IncludeTags: tags},
		}},
	}
	body := render(t, "newsletter_list_fragment", data)
	for _, want := range []string{
		`data-picker="5"`,           // edit-form picker keyed by newsletter ID
		`value="Security" selected`, // followed tag pre-selected in this digest
		`value="47" data-title="Sports" data-tags="null" selected`, // explicit feed selected
		"tags: Security", // summary line lists the followed tag
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

// TestFeedPickerTemplate renders the no-JS baseline: two native multi-selects
// with the right pre-selected feeds and followed tags, plus per-feed tag data.
func TestFeedPickerTemplate(t *testing.T) {
	body := render(t, "feed_picker", map[string]any{
		"Suffix":   "new",
		"Feeds":    []map[string]any{{"ID": int64(12), "Title": "Krebs"}, {"ID": int64(47), "Title": "Sports"}},
		"FeedTags": map[int64][]string{12: {"Security"}},
		"AllTags":  []string{"Security", "Sports"},
		"SelFeeds": []int64{47},
		"SelTags":  []string{"Security"},
	})
	for _, want := range []string{
		`name="tag_names"`,
		`name="feed_ids"`,
		`value="Security" selected`,        // followed tag pre-selected
		`value="47" data-title="Sports" `,  // feed option with title
		`data-tags="[&#34;Security&#34;]"`, // per-feed tags JSON (escaped)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// Feed 47 is the selected one; feed 12 is not.
	if !strings.Contains(body, `value="47" data-title="Sports" data-tags="null" selected`) {
		t.Errorf("feed 47 should be selected with null tags:\n%s", body)
	}
}

// TestFeedTagsCellTemplate renders the per-feed tag cell: chips with remove forms,
// the add-tag input, and HTML-escaping of tag text.
func TestFeedTagsCellTemplate(t *testing.T) {
	body := render(t, "feed_tags_cell", map[string]any{
		"FeedID": int64(42),
		"Tags":   []string{"Security", "a<b>"},
	})
	for _, want := range []string{
		`id="feed-tags-cell-42"`,
		"Security",
		`hx-delete="/feeds/42/tags"`,
		`hx-post="/feeds/42/tags"`,
		`name="tag"`,
		`list="all-tags-list"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// Tag text is escaped (no raw markup injected).
	if strings.Contains(body, "a<b>") {
		t.Errorf("tag not HTML-escaped:\n%s", body)
	}
	if !strings.Contains(body, "a&lt;b&gt;") {
		t.Errorf("expected escaped tag a&lt;b&gt;:\n%s", body)
	}
}
