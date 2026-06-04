package ai

import "testing"

func TestLooksLikeGarbage(t *testing.T) {
	garbage := []string{
		"### User: Write me a poem about cats",
		"### Assistant: Here is the answer",
		"### Instruction: Follow these steps",
		"some text ### Promotee'' more garbage",
		"RGB-GPT-Juneau.com/Craft a detailed analysis",
		"to beating_json|> more text",
		"followeddit.com/questions stuff",
		"your assistant to solve itinerantly",
		"Write an extensive researcher in New York",
	}
	for _, s := range garbage {
		if !LooksLikeGarbage(s) {
			t.Errorf("expected garbage: %q", s)
		}
	}

	clean := []string{
		"The Federal Reserve held interest rates steady amid mixed economic signals.",
		"A new study suggests that coffee consumption may reduce the risk of heart disease.",
		"The user reported the bug to the assistant manager.",
	}
	for _, s := range clean {
		if LooksLikeGarbage(s) {
			t.Errorf("false positive on clean text: %q", s)
		}
	}
}
