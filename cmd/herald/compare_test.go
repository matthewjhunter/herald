package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/herald/internal/storage"
)

// Titles and summaries are feed text and model output over feed text. The
// report goes to a terminal, so nothing in them may reach it as a control
// sequence.
func TestPrintable(t *testing.T) {
	cases := map[string]string{
		"plain title":                        "plain title",
		"clear\x1b[2Jscreen":                 "clear?[2Jscreen",
		"bell\x07 and backspace\x08":         "bell? and backspace?",
		"two\nlines\tand a tab":              "two lines and a tab",
		"c1 \u009b31m csi":                   "c1 ?31m csi",
		"right-to-left \u202e override":      "right-to-left ? override",
		"accents and CJK stay: caf\u00e9 日本": "accents and CJK stay: caf\u00e9 日本",
	}
	for in, want := range cases {
		if got := printable(in); got != want {
			t.Errorf("printable(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteScreenComparison(t *testing.T) {
	var buf bytes.Buffer
	writeScreenComparison(&buf, &storage.ScreenComparison{
		ModelA: "cloud", ModelB: "local", Compared: 4,
		Matrix:       [3][3]int{{1, 0, 1}, {1, 0, 0}, {0, 0, 1}},
		MeanAbsDelta: 3,
		Disagreements: []storage.ScreenDisagreement{
			{ArticleID: 42, Title: "evil\x1b[31m title", ThreatA: 0, ThreatB: 8, CategoryA: "none", CategoryB: "instruction_override"},
		},
	})
	out := buf.String()
	for _, want := range []string{
		"4 articles scanned by both",
		"same bucket: 2 (50.0%)",
		"cloud passes, local blocks: 1",
		"local passes, cloud blocks: 0",
		"mean |threat difference|: 3.00",
		"42", "instruction_override",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("report passed an escape sequence through:\n%q", out)
	}

	buf.Reset()
	writeScreenComparison(&buf, &storage.ScreenComparison{ModelA: "cloud", ModelB: "local"})
	if !strings.Contains(buf.String(), "No article has a scan from both") {
		t.Errorf("empty comparison should say so, got:\n%s", buf.String())
	}
}

func TestWriteSummaryComparison(t *testing.T) {
	var buf bytes.Buffer
	writeSummaryComparison(&buf, &storage.SummaryComparison{
		ModelA: "cloud", ModelB: "local", Compared: 10,
		BothText: 7, OnlyAText: 2, OnlyBText: 1,
		AvgCharsA: 412.4, AvgCharsB: 380, AvgDurationA: 2 * time.Second, AvgDurationB: 3500 * time.Millisecond,
		Samples: []storage.SummaryPair{{ArticleID: 7, Title: "A title", SummaryA: "cloud says", SummaryB: "local\x1b]0;says"}},
	})
	out := buf.String()
	for _, want := range []string{
		"10 articles with a row from both",
		"both produced a summary: 7",
		"only cloud: 2",
		"only local: 1",
		"412", "380", "2.0s", "3.5s",
		"cloud says", "local?]0;says",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}
