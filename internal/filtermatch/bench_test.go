package filtermatch

import (
	"fmt"
	"strings"
	"testing"
)

// A user at the pattern-rule quota, scoring a full page of articles. This is
// the worst realistic case for the read path, and the number it produces is
// what justifies not caching Matchers per user: if building and running one is
// cheap next to the query that fetched the rows, there is nothing to cache.
func benchRules(n int) []Rule {
	rules := make([]Rule, n)
	for i := range rules {
		rules[i] = Rule{
			ID:        int64(i + 1),
			Axis:      AxisTitle,
			MatchMode: MatchRegex,
			Value:     fmt.Sprintf(`(?i)\bpattern %d\b`, i),
			Score:     -1,
		}
	}
	return rules
}

func benchSubjects(n int) []Subject {
	subjects := make([]Subject, n)
	body := strings.Repeat("The article body, of unremarkable length. ", 100)
	for i := range subjects {
		subjects[i] = Subject{
			Title:      fmt.Sprintf("August 9th - 2026 Presidential Politics - Trump Administration Day %d", i),
			Summary:    "A new daily thread for discussion is being introduced.",
			Content:    body,
			Author:     "Sundance",
			Categories: []string{"Uncategorized"},
		}
	}
	return subjects
}

// Building the matcher is included deliberately: it is done per request.
func BenchmarkScorePage(b *testing.B) {
	for _, pageSize := range []int{50, 150} {
		b.Run(fmt.Sprintf("50rules/%darticles", pageSize), func(b *testing.B) {
			rules := benchRules(50)
			subjects := benchSubjects(pageSize)
			b.ResetTimer()
			for b.Loop() {
				m, err := New(rules)
				if err != nil {
					b.Fatal(err)
				}
				for _, s := range subjects {
					m.Score(1, s)
				}
			}
		})
	}
}

// The content axis is the expensive one: full bodies rather than titles.
func BenchmarkScorePageContentAxis(b *testing.B) {
	rules := benchRules(50)
	for i := range rules {
		rules[i].Axis = AxisContent
	}
	subjects := benchSubjects(50)
	b.ResetTimer()
	for b.Loop() {
		m, err := New(rules)
		if err != nil {
			b.Fatal(err)
		}
		for _, s := range subjects {
			m.Score(1, s)
		}
	}
}

// How the content axis scales with rule count, which is what decides its quota.
func BenchmarkContentAxisByRuleCount(b *testing.B) {
	for _, n := range []int{1, 3, 5, 10, 50} {
		b.Run(fmt.Sprintf("%drules", n), func(b *testing.B) {
			rules := benchRules(n)
			for i := range rules {
				rules[i].Axis = AxisContent
			}
			subjects := benchSubjects(50)
			b.ResetTimer()
			for b.Loop() {
				m, _ := New(rules)
				for _, s := range subjects {
					m.Score(1, s)
				}
			}
		})
	}
}
