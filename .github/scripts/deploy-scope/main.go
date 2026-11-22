// deploy-scope decides whether a push has anything to build, from the list of
// files the push changed.
//
// It prints image=true when something that goes into the image differs, and
// image=false when nothing does, in which case there is nothing to build or
// deploy.
//
// The only source of truth is .dockerignore. A changed path is skipped when
// .dockerignore keeps it out of the build context, and only then: the build
// cannot see such a file, so building with or without the change gives the
// same image. There is no second list to keep in step. To stop a path from
// triggering builds, add it to .dockerignore.
//
// It errs toward building. It understands plain entries (a file or directory
// at the repository root) and nothing else:
//
//   - a glob entry excludes files from the build but is not evaluated here, so
//     a path only a glob covers still builds;
//   - a negated entry ("!path") turns skipping off altogether;
//   - with no .dockerignore, every change builds;
//   - a change to the Dockerfile or to .dockerignore always builds, whether or
//     not .dockerignore lists them.
//
// Reads changed paths, one per line, from standard input. Run from the
// repository root:
//
//	git diff --name-only HEAD~1 HEAD | go run .github/scripts/deploy-scope/main.go
//
// An optional argument names the ignore file (default .dockerignore).
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// buildInputs are read by docker itself, outside the build context, so they
// shape the image even when .dockerignore excludes them.
var buildInputs = []string{"Dockerfile", ".dockerignore"}

// ignore is what could be learned from a .dockerignore. The zero value skips
// nothing.
type ignore struct {
	entries []string // plain rooted paths, no trailing slash
	usable  bool     // false: no file, or one this tool cannot reason about
}

func parseDockerignore(r io.Reader) (ignore, error) {
	ig := ignore{usable: true}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "!") {
			ig.usable = false
			continue
		}
		if strings.ContainsAny(line, `*?[\`) {
			continue
		}
		line = strings.TrimPrefix(line, "./")
		line = strings.Trim(line, "/")
		if line == "" || line == "." {
			continue
		}
		ig.entries = append(ig.entries, line)
	}
	return ig, sc.Err()
}

// excluded reports whether the build context leaves path out: it is a listed
// file, or lies inside a listed directory. Entries are rooted, so "docs"
// covers docs/x.md and not internal/docs/x.md or docs-site/x.md.
func (ig ignore) excluded(path string) bool {
	if !ig.usable {
		return false
	}
	for _, e := range ig.entries {
		if path == e || strings.HasPrefix(path, e+"/") {
			return true
		}
	}
	return false
}

// classify reports whether any changed path goes into the image, and the
// first one that does.
func (ig ignore) classify(changed []string) (image bool, why string) {
	for _, p := range changed {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		for _, b := range buildInputs {
			if p == b {
				return true, p
			}
		}
		if !ig.excluded(p) {
			return true, p
		}
	}
	return false, ""
}

func output(image bool) string {
	return fmt.Sprintf("image=%t\n", image)
}

func load(path string) (ignore, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ignore{}, nil
	}
	if err != nil {
		return ignore{}, err
	}
	defer f.Close() //nolint:errcheck // read-only
	return parseDockerignore(f)
}

func main() {
	path := ".dockerignore"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	ig, err := load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deploy-scope:", err)
		os.Exit(1)
	}

	var changed []string
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		changed = append(changed, sc.Text())
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "deploy-scope:", err)
		os.Exit(1)
	}

	image, why := ig.classify(changed)
	if image {
		fmt.Fprintf(os.Stderr, "deploy-scope: %s is in the image\n", why)
	} else {
		fmt.Fprintln(os.Stderr, "deploy-scope: nothing in the image changed")
	}
	fmt.Print(output(image))
}
