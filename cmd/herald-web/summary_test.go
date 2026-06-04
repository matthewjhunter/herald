package main

import (
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAISummaryTemplate forces template parsing (h.init uses template.Must) and
// checks each status branch of the AI Summary fragment renders as expected.
func TestAISummaryTemplate(t *testing.T) {
	h := &handlers{}
	cases := []struct {
		name string
		data summaryViewData
		want []string
		deny []string
	}{
		{
			name: "disabled",
			data: summaryViewData{Enabled: false},
			want: []string{"not configured", "disabled"},
		},
		{
			name: "empty",
			data: summaryViewData{Enabled: true, Prompt: "do the thing"},
			want: []string{"No summary yet", "do the thing", "Edit prompt"},
		},
		{
			name: "generating",
			data: summaryViewData{Enabled: true, Status: "generating"},
			want: []string{`hx-trigger="every 3s"`, "Generating summary"},
		},
		{
			name: "done",
			data: summaryViewData{Enabled: true, Status: "done", Headline: "Daily Brief",
				ArticleCount: 3, SanitizedHTML: template.HTML("<p>the digest</p>")},
			want: []string{"Daily Brief", "the digest", "Mark all 3 as read", "/summary/mark-read"},
			deny: []string{"every 3s"},
		},
		{
			name: "failed",
			data: summaryViewData{Enabled: true, Status: "failed", Error: "backend timeout"},
			want: []string{"Generation failed", "backend timeout"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.renderFragment(rec, "ai_summary_view", tc.data)
			body := rec.Body.String()
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("expected %q in output:\n%s", w, body)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(body, d) {
					t.Errorf("did not expect %q in output:\n%s", d, body)
				}
			}
		})
	}
}
