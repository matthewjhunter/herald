package main

import (
	"strings"
	"testing"
)

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
