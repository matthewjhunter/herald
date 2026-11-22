package main

import (
	"strings"
	"testing"
)

const sample = `# comment
docs
plans/
/systemd
./githooks
.github
README.md
*.swp
herald.db*
`

func rules(t *testing.T, text string) ignore {
	t.Helper()
	ig, err := parseDockerignore(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return ig
}

func TestClassify(t *testing.T) {
	ig := rules(t, sample)
	for _, tc := range []struct {
		name    string
		changed []string
		image   bool
	}{
		{"nothing changed", nil, false},
		{"blank lines only", []string{"", "  "}, false},

		// Excluded from the build context, so not in the image.
		{"a file in an ignored directory", []string{"docs/architecture.md"}, false},
		{"deep in an ignored directory", []string{".github/workflows/docker.yml"}, false},
		{"entry written with a trailing slash", []string{"plans/001.md"}, false},
		{"entry written with a leading slash", []string{"systemd/herald.service"}, false},
		{"entry written with a leading dot-slash", []string{"githooks/pre-commit"}, false},
		{"an ignored file", []string{"README.md"}, false},
		{"several ignored paths", []string{"README.md", "docs/a.md", ".github/x.yml"}, false},

		// In the build context.
		{"source", []string{"internal/web/routes.go"}, true},
		{"dependencies", []string{"go.mod", "go.sum"}, true},
		{"something new at the root", []string{"newthing/file.txt"}, true},
		{"one real change among ignored ones", []string{"docs/a.md", "engine.go"}, true},

		// A name that only looks like an ignored one is not ignored.
		// .dockerignore entries are rooted, unlike .gitignore.
		{"nested directory of the same name", []string{"internal/docs/page.md"}, true},
		{"nested file of the same name", []string{"internal/README.md"}, true},
		{"a path sharing a prefix", []string{"docs-site/index.md", "README.md.tmpl"}, true},

		// A glob excludes files from the build, but this tool does not
		// evaluate globs, so a path only a glob covers still builds.
		{"covered only by a glob", []string{"notes.swp"}, true},
		{"covered only by a prefix glob", []string{"herald.db-wal"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := ig.classify(tc.changed)
			if got != tc.image {
				t.Errorf("classify(%v) = %v, want %v", tc.changed, got, tc.image)
			}
		})
	}
}

// Docker reads the Dockerfile and .dockerignore itself, so they shape the
// image even when .dockerignore lists them (a common habit).
func TestBuildInputsAlwaysCount(t *testing.T) {
	ig := rules(t, "Dockerfile\n.dockerignore\ndocs\n")
	for _, p := range []string{"Dockerfile", ".dockerignore"} {
		if got, _ := ig.classify([]string{p}); !got {
			t.Errorf("a change to %s must build", p)
		}
	}
}

// A negated pattern re-includes part of an excluded path. Rather than model
// that, treat the whole file as unreadable: everything builds.
func TestNegationDisablesSkipping(t *testing.T) {
	ig := rules(t, "docs\n!docs/embedded.md\n")
	if got, _ := ig.classify([]string{"docs/other.md"}); !got {
		t.Error("with a negated pattern present, nothing may be skipped")
	}
}

func TestNoDockerignoreMeansEverythingBuilds(t *testing.T) {
	var ig ignore // what a missing file yields
	if got, _ := ig.classify([]string{"docs/a.md"}); !got {
		t.Error("with no .dockerignore, every change builds")
	}
	if got, _ := ig.classify(nil); got {
		t.Error("no changes still means nothing to build")
	}
}

func TestClassifyNamesTheFirstPathThatBuilds(t *testing.T) {
	ig := rules(t, sample)
	_, why := ig.classify([]string{"docs/a.md", "engine.go", "types.go"})
	if why != "engine.go" {
		t.Errorf("reason = %q, want engine.go", why)
	}
}

func TestOutputFormat(t *testing.T) {
	if got := output(true); got != "image=true\n" {
		t.Errorf("output = %q", got)
	}
	if got := output(false); got != "image=false\n" {
		t.Errorf("output = %q", got)
	}
}
