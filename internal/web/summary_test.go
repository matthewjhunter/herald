package web

import (
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"
)

func render(t *testing.T, name string, data any) string {
	t.Helper()
	rec := httptest.NewRecorder()
	(&handlers{}).renderFragment(rec, name, data)
	return rec.Body.String()
}

// TestAISummaryListTemplate forces template parse and checks the list pane.
func TestAISummaryListTemplate(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		body := render(t, "ai_summary_list", summaryListData{Enabled: false})
		if !strings.Contains(body, "not configured") || !strings.Contains(body, "disabled") {
			t.Errorf("disabled list wrong:\n%s", body)
		}
	})
	t.Run("empty", func(t *testing.T) {
		body := render(t, "ai_summary_list", summaryListData{Enabled: true})
		for _, w := range []string{"No summaries yet", "/summary/generate"} {
			if !strings.Contains(body, w) {
				t.Errorf("missing %q:\n%s", w, body)
			}
		}
		if strings.Contains(body, "Edit prompt") {
			t.Errorf("inline prompt editor should be gone:\n%s", body)
		}
	})
	t.Run("rows: done is clickable into reading pane, generating polls", func(t *testing.T) {
		body := render(t, "ai_summary_list", summaryListData{
			Enabled:    true,
			Generating: true,
			Rows: []summaryRow{
				{ID: 7, Label: "Jun 4, 2026 · 2:30 PM", Status: "done", ArticleCount: 12},
				{ID: 8, Label: "Jun 4, 2026 · 3:00 PM", Status: "generating"},
			},
		})
		for _, w := range []string{
			"Jun 4, 2026 · 2:30 PM", "12 articles",
			`hx-get="/summary/7"`, `hx-target="#reading-pane"`, // done row opens in reading pane
			"generating…",           // generating row label
			`hx-trigger="every 3s"`, // list polls while generating
		} {
			if !strings.Contains(body, w) {
				t.Errorf("missing %q:\n%s", w, body)
			}
		}
		// The generating row (id 8) must not be clickable.
		if strings.Contains(body, `hx-get="/summary/8"`) {
			t.Errorf("generating row should not be clickable:\n%s", body)
		}
	})
}

// TestAISummaryDetailTemplate forces template parse and checks the reading pane.
func TestAISummaryDetailTemplate(t *testing.T) {
	t.Run("done", func(t *testing.T) {
		body := render(t, "ai_summary_detail", summaryDetailData{
			ID: 7, Status: "done", Headline: "Daily Brief", ArticleCount: 12,
			SanitizedHTML: template.HTML("<p>the digest</p>"),
		})
		for _, w := range []string{"Daily Brief", "the digest", "Mark all 12 as read", `hx-post="/summary/7/mark-read"`} {
			if !strings.Contains(body, w) {
				t.Errorf("missing %q:\n%s", w, body)
			}
		}
	})
	t.Run("failed", func(t *testing.T) {
		body := render(t, "ai_summary_detail", summaryDetailData{ID: 7, Status: "failed", Error: "backend timeout"})
		if !strings.Contains(body, "Generation failed") || !strings.Contains(body, "backend timeout") {
			t.Errorf("failed detail wrong:\n%s", body)
		}
	})
	t.Run("generating polls itself", func(t *testing.T) {
		body := render(t, "ai_summary_detail", summaryDetailData{ID: 7, Status: "generating"})
		if !strings.Contains(body, `hx-get="/summary/7"`) || !strings.Contains(body, `hx-trigger="every 3s"`) {
			t.Errorf("generating detail should poll:\n%s", body)
		}
	})
}
