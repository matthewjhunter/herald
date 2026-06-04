package ai

import (
	"strings"
	"testing"
)

func TestLooksLikeTopicLabel(t *testing.T) {
	valid := []string{
		"Russia Strikes Kyiv in Missile Barrage",
		"Federal Reserve Holds Rates Steady",
		"Israel-Lebanon Border Escalation",
		"Harvard Antisemitism Investigation",
	}
	for _, s := range valid {
		if !looksLikeTopicLabel(s) {
			t.Errorf("expected valid label: %q", s)
		}
	}

	invalid := []string{
		// The exact production bug: a conversational request stored as a label.
		"Please provide the summary of related news articles so I can generate the topic label.",
		"I can generate a label once you share the articles.",
		"I need the article summaries first.",
		"Sorry, I cannot do that.",
		"As an AI, I'm unable to summarize without input.",
		"What articles would you like me to label?",
		"Here is the topic label:",
		"",
		strings.Repeat("x", 150), // too long to be a label
	}
	for _, s := range invalid {
		if looksLikeTopicLabel(s) {
			t.Errorf("expected invalid (conversational/too-long): %q", s)
		}
	}
}
