package web

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// smokeManifestPath is the committed route manifest, consumed by the smolder
// CLI (gate + live runs) and kept in sync by this test.
const smokeManifestPath = "smoke-manifest.json"

// TestSmokeManifestUpToDate asserts the committed manifest matches the routes
// NewRouter actually registers. Regenerate after adding or changing routes:
//
//	UPDATE_SMOKE_MANIFEST=1 go test ./internal/web/ -run TestSmokeManifestUpToDate
//
// This is the drift gate for the route surface: a new route that isn't
// reflected here fails CI until the manifest is regenerated (which also
// surfaces the route to the smolder coverage gate). NewRouter only records
// specs at registration time and never touches the engine/validator, so a nil
// engine is sufficient to enumerate the surface without a database.
func TestSmokeManifestUpToDate(t *testing.T) {
	got, err := NewRouter(nil, nil, "", nil).Registry().Manifest().MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	if os.Getenv("UPDATE_SMOKE_MANIFEST") != "" {
		if err := os.WriteFile(smokeManifestPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", smokeManifestPath)
		return
	}

	want, err := os.ReadFile(smokeManifestPath)
	if err != nil {
		t.Fatalf("read %s (regenerate with UPDATE_SMOKE_MANIFEST=1): %v", smokeManifestPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("smoke manifest drift: routes changed but %s is stale.\n"+
			"Regenerate: UPDATE_SMOKE_MANIFEST=1 go test ./cmd/herald-web/ -run TestSmokeManifestUpToDate\n%s",
			smokeManifestPath, lineDiff(string(want), string(got)))
	}
}

// lineDiff reports lines present in want-but-not-got and got-but-not-want, so a
// drift failure pinpoints which routes were added or removed regardless of
// ordering. It is a set difference, not a positional diff.
func lineDiff(want, got string) string {
	wantSet := map[string]bool{}
	for _, l := range strings.Split(want, "\n") {
		wantSet[l] = true
	}
	gotSet := map[string]bool{}
	for _, l := range strings.Split(got, "\n") {
		gotSet[l] = true
	}
	var b strings.Builder
	b.WriteString("lines only in committed (want):\n")
	for _, l := range strings.Split(want, "\n") {
		if !gotSet[l] {
			b.WriteString("  - " + l + "\n")
		}
	}
	b.WriteString("lines only in generated (got):\n")
	for _, l := range strings.Split(got, "\n") {
		if !wantSet[l] {
			b.WriteString("  + " + l + "\n")
		}
	}
	return b.String()
}
