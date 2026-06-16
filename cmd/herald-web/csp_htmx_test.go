package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTemplatesAvoidEvalHTMX guards the CSP contract. The app's
// Content-Security-Policy (see securityHeaders in middleware.go) deliberately
// omits 'unsafe-eval'. htmx compiles hx-on attribute bodies and js:-prefixed
// hx-vals via new Function(), which that CSP blocks -- so any such usage fails
// silently in the browser (the bug that motivated this test: clicking an
// article stopped marking it read because the row's hx-on never ran).
//
// Behaviors that previously used hx-on now live as delegated listeners in
// static/herald.js, keyed off data-* attributes. If you reach for hx-on or
// js: hx-vals again, this test fails on purpose: add the behavior to herald.js
// instead, or grant 'unsafe-eval' (and weaken the CSP) consciously.
func TestTemplatesAvoidEvalHTMX(t *testing.T) {
	matches, err := filepath.Glob("templates/*.html")
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no templates found; wrong working directory?")
	}

	banned := []string{`hx-on`, `hx-vals="js:`, `hx-vals='js:`}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(data)
		for _, needle := range banned {
			if strings.Contains(body, needle) {
				t.Errorf("%s contains eval-dependent htmx %q, which the CSP (no 'unsafe-eval') blocks; "+
					"move the behavior into static/herald.js as a delegated listener", filepath.Base(path), needle)
			}
		}
	}
}
