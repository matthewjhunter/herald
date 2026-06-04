package ai

import "strings"

// LooksLikeGarbage detects model output that contains training-data artifacts
// or prompt injection patterns rather than a real summary. Small models under
// load sometimes produce this kind of garbled output.
func LooksLikeGarbage(summary string) bool {
	lower := strings.ToLower(summary)
	for _, pattern := range []string{
		"### user:",
		"### assistant:",
		"### instruction:",
		"### promotee",
		"rgb-gpt",
		"beating_json",
		"followeddit.com",
		"your assistant to solve",
		"write an extensive researcher",
	} {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}
