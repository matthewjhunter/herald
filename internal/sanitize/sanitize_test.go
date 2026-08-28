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

func TestText(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		mustDrop []string
		mustKeep []string
	}{
		{
			name:     "strips tags but keeps prose",
			in:       `<p>Real <b>news</b> here.</p><script>steal()</script>`,
			mustDrop: []string{"<p>", "<b>", "steal()"},
			mustKeep: []string{"Real", "news", "here."},
		},
		{
			// An editorial cartoon is a body of one image: flattening it to the
			// empty string leaves the model narrating nothing, so say an image
			// is there and carry its alt text and caption.
			name:     "image-only body announces the image and its text",
			in:       `<figure><img src="https://example.com/cartoon.jpg" alt="Two men in a waiting room"><figcaption>Big Brother is watching.</figcaption></figure>`,
			mustDrop: []string{"<img", "cartoon.jpg"},
			mustKeep: []string{"[image: Two men in a waiting room]", "Big Brother is watching."},
		},
		{
			name:     "image without alt text still announces itself",
			in:       `<p><img src="https://example.com/cartoon.jpg"></p>`,
			mustKeep: []string{"[image]"},
		},
		{
			name:     "alt text cannot smuggle markup",
			in:       `<img src="x.jpg" alt="&lt;script&gt;alert(1)&lt;/script&gt;">`,
			mustDrop: []string{"<script>", "</script>"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Text(tt.in)
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

// The placeholder must not let a hostile feed inject unbounded text into the
// model's input under the guise of an alt attribute.
func TestTextCapsAltLength(t *testing.T) {
	long := strings.Repeat("a", 5000)
	got := Text(`<img src="x.jpg" alt="` + long + `">`)
	if len(got) > 400 {
		t.Errorf("expected alt text to be capped, got %d chars", len(got))
	}
}
