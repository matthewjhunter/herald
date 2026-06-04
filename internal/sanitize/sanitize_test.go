package sanitize

import (
	"strings"
	"testing"
)

func TestHTML(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		mustDrop []string
		mustKeep []string
	}{
		{
			name:     "strips script tags but keeps surrounding prose",
			in:       `<p>Real news here.</p><script src="https://rumble.com/embedJS/widgets.js"></script>`,
			mustDrop: []string{"<script", "widgets.js"},
			mustKeep: []string{"Real news here."},
		},
		{
			name:     "strips inline event handlers",
			in:       `<a href="https://example.com" onclick="steal()">link</a>`,
			mustDrop: []string{"onclick", "steal()"},
			mustKeep: []string{"href", "example.com", "link"},
		},
		{
			name:     "strips javascript: URIs",
			in:       `<a href="javascript:alert(1)">click</a>`,
			mustDrop: []string{"javascript:", "alert(1)"},
			mustKeep: []string{"click"},
		},
		{
			name:     "preserves links so URL signal survives for the security scan",
			in:       `<p>See <a href="https://malware.example/payload">this</a>.</p>`,
			mustKeep: []string{"https://malware.example/payload", "this"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HTML(tt.in)
			for _, d := range tt.mustDrop {
				if strings.Contains(got, d) {
					t.Errorf("output should not contain %q\n got: %s", d, got)
				}
			}
			for _, k := range tt.mustKeep {
				if !strings.Contains(got, k) {
					t.Errorf("output should contain %q\n got: %s", k, got)
				}
			}
		})
	}
}
