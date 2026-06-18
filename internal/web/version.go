package web

import (
	"runtime/debug"
	"time"
)

// version and buildTime are optionally injected at build time via ldflags.
// When not set (or when a full 40-char hash is injected), init() normalises
// them using Go's embedded VCS build info.
//
// ldflags example:
//
//	-ldflags "-X github.com/matthewjhunter/herald/internal/web.version=${GIT_SHA} -X github.com/matthewjhunter/herald/internal/web.buildTime=${BUILD_TIME}"
var version = "dev"
var buildTime = "unknown"

func init() {
	// Truncate any full commit hash (40 chars) down to the conventional 7.
	if len(version) > 12 {
		version = version[:7]
	}
	// Fall back to Go's embedded VCS metadata when ldflags weren't used.
	if version == "dev" || buildTime == "unknown" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					if version == "dev" && len(s.Value) >= 7 {
						version = s.Value[:7]
					}
				case "vcs.time":
					if buildTime == "unknown" {
						if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
							buildTime = t.UTC().Format("2006-01-02T15:04Z")
						}
					}
				}
			}
		}
	}
}
