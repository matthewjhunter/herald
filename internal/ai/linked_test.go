package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateText_RuneSafe(t *testing.T) {
	s := strings.Repeat("é", 10) // 20 bytes
	got := truncateText(s, 11)
	if !utf8.ValidString(got) || got != strings.Repeat("é", 5)+"..." {
		t.Fatalf("cut inside a rune: %q", got)
	}
	if got := truncateText("short", 10); got != "short" {
		t.Fatalf("short text altered: %q", got)
	}
}

// The linked page is a separate block with the host it came from, budgeted
// from what the post leaves of the shared prompt limit.
func TestFencedArticleText_LinkedBlock(t *testing.T) {
	data, err := fencedArticleText(ArticleText{Title: "T", Content: "Heh.", LinkedURL: "https://example.org/story?x=1", LinkedContent: "The linked story."})
	if err != nil {
		t.Fatal(err)
	}
	if data["LinkedHost"] != "example.org" || data["LinkedContent"] != "The linked story." {
		t.Fatalf("linked block = %q / %q", data["LinkedHost"], data["LinkedContent"])
	}

	long := ArticleText{Content: strings.Repeat("a", maxPromptContentLen-10), LinkedURL: "https://example.org/", LinkedContent: strings.Repeat("b", 100)}
	data, _ = fencedArticleText(long)
	if data["LinkedContent"] != strings.Repeat("b", 10)+"..." {
		t.Fatalf("linked page not budgeted from the remainder: %q", data["LinkedContent"])
	}

	full := ArticleText{Content: strings.Repeat("a", maxPromptContentLen+5), LinkedURL: "https://example.org/", LinkedContent: "b"}
	data, _ = fencedArticleText(full)
	if data["LinkedContent"] != "" {
		t.Fatalf("a post that fills the budget must leave the linked page out, got %q", data["LinkedContent"])
	}

	data, _ = fencedArticleText(ArticleText{Content: "c"})
	if data["LinkedContent"] != "" || data["LinkedHost"] != "" {
		t.Fatalf("no linked page must render empty keys, got %q / %q", data["LinkedHost"], data["LinkedContent"])
	}
}

// fencesIn counts opened fences: tags on their own line, not the mention in
// the instructions.
func fencesIn(s string) int { return strings.Count(s, "\n<untrusted-") }

func TestCurationPlan_LinkedPageFencedSeparately(t *testing.T) {
	p := newPlanner(t, nil)
	plan, err := p.CurationPlan(7, ArticleText{Title: "Heh", Content: "Indeed.", LinkedURL: "https://example.org/story", LinkedContent: "The linked story says things."}, nil)
	if err != nil {
		t.Fatal(err)
	}
	user := plan.Request.User
	if fencesIn(user) != 3 || !strings.Contains(user, "Linked page from example.org") {
		t.Fatalf("linked page not in its own labelled fence:\n%s", user)
	}
	post, linked := strings.Index(user, "Indeed."), strings.Index(user, "The linked story")
	if post < 0 || linked < post || !strings.Contains(user[post:linked], "</untrusted-") {
		t.Fatalf("linked page must follow the closed content fence:\n%s", user)
	}
	if !strings.Contains(plan.Request.System, "someone else wrote it") {
		t.Fatal("the stable half must tell the model what the second block is")
	}

	plain, err := p.CurationPlan(7, ArticleText{Title: "T", Content: "C"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fencesIn(plain.Request.User) != 2 || strings.Contains(plain.Request.User, "Linked page from") {
		t.Fatalf("no linked page must render no block:\n%s", plain.Request.User)
	}
}

func TestSummarizeArticle_LinkedPageFencedSeparately(t *testing.T) {
	var prompt string
	p := fakeModelServerFunc(t, nil, func(reqBody string) string {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal([]byte(reqBody), &req)
		prompt = ""
		for _, m := range req.Messages {
			prompt += m.Content + "\n"
		}
		return "Summary."
	})
	ctx := context.Background()
	if _, err := p.SummarizeArticle(ctx, ArticleText{Title: "Heh", Content: "Indeed.", LinkedURL: "https://example.org/story", LinkedContent: "The linked story says things."}, 0); err != nil {
		t.Fatal(err)
	}
	if fencesIn(prompt) != 3 || !strings.Contains(prompt, "Linked page from example.org") || !strings.Contains(prompt, "someone else wrote it") {
		t.Fatalf("linked page not fenced and explained:\n%s", prompt)
	}
	if _, err := p.SummarizeArticle(ctx, ArticleText{Title: "T", Content: "Some body."}, 0); err != nil {
		t.Fatal(err)
	}
	if fencesIn(prompt) != 2 || strings.Contains(prompt, "Linked page from") || strings.Contains(prompt, "someone else wrote it") {
		t.Fatalf("no linked page must render no block or instruction:\n%s", prompt)
	}
}
