package web

import (
	"encoding/json"
	"net/url"
	"os/exec"
	"testing"
)

// jsConfigRequest runs the shipped static/herald.js under node and replays an
// htmx:configRequest event for each case, returning the request URL htmx would
// end up issuing. Node is not a build dependency of herald, so the test skips
// where it is unavailable.
func jsConfigRequest(t *testing.T, cases []map[string]any) []string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping herald.js behaviour test")
	}
	payload, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("marshal cases: %v", err)
	}
	out, err := exec.Command(node, "testdata/heraldjs_harness.js", "static/herald.js", string(payload)).CombinedOutput()
	if err != nil {
		t.Fatalf("node harness: %v\n%s", err, out)
	}
	var results []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		t.Fatalf("parse harness output %q: %v", out, err)
	}
	urls := make([]string, len(results))
	for i, r := range results {
		urls[i] = r.URL
	}
	return urls
}

// TestShowReadToggleAppliesToEveryArticleListView guards the "Show read"
// toggle. htmx reports the full request URL -- query string included -- as
// event.detail.path, so a listener that compares it to "/articles" only ever
// matches the All Articles view. When it did, selecting a feed or group and
// switching to "Show read" refetched the unread-only list: a reader who had
// already read everything in that feed got an empty list instead of their
// history (the bug this test was written for).
func TestShowReadToggleAppliesToEveryArticleListView(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		hideRead bool
		want     map[string]string // query params expected on the final URL
		wantBase string
	}{
		{
			name:     "all articles, show read",
			path:     "/articles",
			wantBase: "/articles",
			want:     map[string]string{"show_read": "1"},
		},
		{
			name:     "feed view, show read",
			path:     "/articles?feed_id=44",
			wantBase: "/articles",
			want:     map[string]string{"feed_id": "44", "show_read": "1"},
		},
		{
			name:     "group view, show read",
			path:     "/articles?group_id=3",
			wantBase: "/articles",
			want:     map[string]string{"group_id": "3", "show_read": "1"},
		},
		{
			name:     "infinite scroll page already carrying show_read",
			path:     "/articles?offset=30&feed_id=44&show_read=1",
			wantBase: "/articles",
			want:     map[string]string{"offset": "30", "feed_id": "44", "show_read": "1"},
		},
		{
			name:     "feed view, hide read",
			path:     "/articles?feed_id=44",
			hideRead: true,
			wantBase: "/articles",
			want:     map[string]string{"feed_id": "44"},
		},
		{
			name:     "hide read strips show_read the URL already carried",
			path:     "/articles?offset=30&feed_id=44&show_read=1",
			hideRead: true,
			wantBase: "/articles",
			want:     map[string]string{"offset": "30", "feed_id": "44"},
		},
		{
			name:     "non-list requests are untouched",
			path:     "/articles/5?from=feed",
			wantBase: "/articles/5",
			want:     map[string]string{"from": "feed"},
		},
	}

	payload := make([]map[string]any, len(cases))
	for i, tc := range cases {
		payload[i] = map[string]any{"path": tc.path, "hideRead": tc.hideRead, "parameters": map[string]string{}}
	}
	urls := jsConfigRequest(t, payload)
	if len(urls) != len(cases) {
		t.Fatalf("harness returned %d results, want %d", len(urls), len(cases))
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(urls[i])
			if err != nil {
				t.Fatalf("parse %q: %v", urls[i], err)
			}
			if u.Path != tc.wantBase {
				t.Errorf("path = %q, want %q (full URL %q)", u.Path, tc.wantBase, urls[i])
			}
			q := u.Query()
			if len(q) != len(tc.want) {
				t.Errorf("query = %v, want %v (full URL %q)", q, tc.want, urls[i])
			}
			for k, want := range tc.want {
				if len(q[k]) != 1 {
					t.Errorf("param %q = %v, want exactly one %q (full URL %q)", k, q[k], want, urls[i])
					continue
				}
				if q.Get(k) != want {
					t.Errorf("param %q = %q, want %q (full URL %q)", k, q.Get(k), want, urls[i])
				}
			}
		})
	}
}
