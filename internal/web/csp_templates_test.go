package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The strict Content-Security-Policy (see reportOnlyHeaders) refuses inline
// script, inline style and style attributes, so the templates carry none. Script
// lives in static/*.js, presentation in static/*.css, and behavior that used to
// be an inline attribute is a delegated listener in static/herald.js keyed off
// a data attribute. If this fails, move the new code to a file.
func TestTemplatesCarryNoInlineCode(t *testing.T) {
	var (
		scriptTag = regexp.MustCompile(`(?is)<script\b([^>]*)>`)
		srcAttr   = regexp.MustCompile(`(?i)\ssrc=`)
		styleTag  = regexp.MustCompile(`(?i)<style[\s>]`)
		styleAttr = regexp.MustCompile(`(?i)\sstyle=`)
		handler   = regexp.MustCompile(`(?i)\son[a-z]+=`)
	)
	files, err := filepath.Glob("templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		name := filepath.Base(path)
		for _, m := range scriptTag.FindAllStringSubmatch(s, -1) {
			if !srcAttr.MatchString(" " + m[1]) {
				t.Errorf("%s: inline <script>", name)
			}
		}
		if styleTag.MatchString(s) {
			t.Errorf("%s: inline <style> element", name)
		}
		if styleAttr.MatchString(s) {
			t.Errorf("%s: style attribute (use a class)", name)
		}
		if handler.MatchString(s) {
			t.Errorf("%s: inline event handler attribute (use a data attribute and a delegated listener in herald.js)", name)
		}
	}
}

// Every utility class the templates use has a rule, and every stylesheet and
// script the templates name exists in the embedded static directory. A typo in
// either would silently drop a style or a behavior.
func TestTemplatesReferenceOnlyDefinedAssets(t *testing.T) {
	css, err := os.ReadFile("static/utilities.css")
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.(u-[a-z0-9-]+)\s*\{`).FindAllStringSubmatch(string(css), -1) {
		defined[m[1]] = true
	}
	classAttr := regexp.MustCompile(`class="([^"]*)"`)
	action := regexp.MustCompile(`\{\{[^}]*\}\}`)
	asset := regexp.MustCompile(`(?:src|href)="(/static/[^"?]+)`)
	files, _ := filepath.Glob("templates/*.html")
	for _, path := range files {
		b, _ := os.ReadFile(path)
		name := filepath.Base(path)
		for _, m := range classAttr.FindAllStringSubmatch(string(b), -1) {
			for _, c := range strings.Fields(action.ReplaceAllString(m[1], " ")) {
				if strings.HasPrefix(c, "u-") && !defined[c] {
					t.Errorf("%s uses .%s, which utilities.css does not define", name, c)
				}
			}
		}
		for _, m := range asset.FindAllStringSubmatch(string(b), -1) {
			if _, err := os.Stat("static/" + strings.TrimPrefix(m[1], "/static/")); err != nil {
				t.Errorf("%s names %s: %v", name, m[1], err)
			}
		}
	}
}

// The gauge's arc boundaries are numbers from the server; the template hands
// them over as data attributes and herald.js sets the custom properties, because
// a style attribute would be refused.
func TestGaugeUsesDataAttributesNotAStyleAttribute(t *testing.T) {
	b, err := os.ReadFile("templates/feed_sidebar.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `data-grey-end="{{.GreyEnd}}"`) || !strings.Contains(s, `data-yellow-end="{{.YellowEnd}}"`) {
		t.Error("the gauge does not carry its boundaries as data attributes")
	}
	js, _ := os.ReadFile("static/herald.js")
	if !strings.Contains(string(js), "--grey-end") || !strings.Contains(string(js), "--yellow-end") {
		t.Error("herald.js does not apply the gauge boundaries")
	}
}
