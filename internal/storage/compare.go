package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/matthewjhunter/herald/internal/storage/db"
)

// Model comparison over the summary and screen histories (migration 0024).
// Each side is a model's latest row per article, and only articles with a row
// from both models are counted.

// Screen buckets, by the thresholds the caller passes.
const (
	ScreenBucketPass = iota
	ScreenBucketBorderline
	ScreenBucketBlock
)

// ScreenComparison is two models' verdicts on the same articles.
type ScreenComparison struct {
	ModelA, ModelB string
	// Compared is how many articles both models scanned.
	Compared int
	// Matrix[a][b] counts articles model A put in bucket a and model B in b.
	Matrix [3][3]int
	// MeanAbsDelta is the mean absolute difference in threat (0-10 scale).
	MeanAbsDelta float64
	// Disagreements lists articles the models bucketed differently, widest
	// threat gap first, up to the limit asked for.
	Disagreements []ScreenDisagreement
}

// Agree is how many articles both models put in the same bucket.
func (c ScreenComparison) Agree() int {
	return c.Matrix[0][0] + c.Matrix[1][1] + c.Matrix[2][2]
}

// ScreenDisagreement is one article two models bucketed differently. Title is
// feed content: untrusted, and to be escaped by whatever prints it.
type ScreenDisagreement struct {
	ArticleID            int64
	Title                string
	ThreatA, ThreatB     float64
	CategoryA, CategoryB string
}

// SummaryComparison is two models' summaries of the same articles.
type SummaryComparison struct {
	ModelA, ModelB string
	// Compared is how many articles have a row (text or skip) from both.
	Compared int
	// How those split: both produced text, only one did, or neither.
	BothText, OnlyAText, OnlyBText, NeitherText int
	// Average summary length in characters, over articles both summarized.
	AvgCharsA, AvgCharsB float64
	// Average model-call time for each model's successful summaries.
	AvgDurationA, AvgDurationB time.Duration
	// Samples is a fixed pseudo-random selection of articles both summarized.
	Samples []SummaryPair
}

// SummaryPair is one article's summary from each model. All three strings are
// untrusted: a feed's title and model output over attacker-controlled text.
type SummaryPair struct {
	ArticleID          int64
	Title              string
	SummaryA, SummaryB string
}

// ModelRows is how many history rows a model has.
type ModelRows struct {
	Model string // "" for rows written before models were recorded
	Rows  int
}

func (s *PostgresStore) CompareScreens(modelA, modelB string, pass, border float64, maxDisagreements int) (*ScreenComparison, error) {
	ctx := context.Background()
	out := &ScreenComparison{ModelA: modelA, ModelB: modelB}
	buckets, err := s.q.CompareScreenBuckets(ctx, db.CompareScreenBucketsParams{Pass: pass, Border: border, ModelA: modelA, ModelB: modelB})
	if err != nil {
		return nil, fmt.Errorf("compare screens: %w", err)
	}
	var absDelta float64
	for _, b := range buckets {
		if b.BucketA < 0 || b.BucketA > 2 || b.BucketB < 0 || b.BucketB > 2 {
			return nil, fmt.Errorf("compare screens: bucket (%d, %d) out of range", b.BucketA, b.BucketB)
		}
		out.Matrix[b.BucketA][b.BucketB] = b.N
		out.Compared += b.N
		absDelta += b.AbsDelta
	}
	if out.Compared > 0 {
		out.MeanAbsDelta = absDelta / float64(out.Compared)
	}
	if maxDisagreements > 0 {
		rows, err := s.q.ListScreenDisagreements(ctx, db.ListScreenDisagreementsParams{
			Pass: pass, Border: border, Lim: int32(maxDisagreements), ModelA: modelA, ModelB: modelB,
		})
		if err != nil {
			return nil, fmt.Errorf("compare screens: disagreements: %w", err)
		}
		for _, r := range rows {
			out.Disagreements = append(out.Disagreements, ScreenDisagreement{
				ArticleID: r.ID, Title: r.Title, ThreatA: r.ThreatA, ThreatB: r.ThreatB,
				CategoryA: r.CategoryA, CategoryB: r.CategoryB,
			})
		}
	}
	return out, nil
}

func (s *PostgresStore) CompareSummaries(modelA, modelB string, samples int) (*SummaryComparison, error) {
	ctx := context.Background()
	c, err := s.q.CompareSummaryCounts(ctx, db.CompareSummaryCountsParams{ModelA: modelA, ModelB: modelB})
	if err != nil {
		return nil, fmt.Errorf("compare summaries: %w", err)
	}
	out := &SummaryComparison{
		ModelA: modelA, ModelB: modelB,
		Compared: c.BothRows, BothText: c.BothText, OnlyAText: c.OnlyAText, OnlyBText: c.OnlyBText, NeitherText: c.NeitherText,
		AvgCharsA: c.AvgCharsA, AvgCharsB: c.AvgCharsB,
		AvgDurationA: time.Duration(c.AvgMsA * float64(time.Millisecond)),
		AvgDurationB: time.Duration(c.AvgMsB * float64(time.Millisecond)),
	}
	if samples > 0 {
		rows, err := s.q.SampleSummaryPairs(ctx, db.SampleSummaryPairsParams{Lim: int32(samples), ModelA: modelA, ModelB: modelB})
		if err != nil {
			return nil, fmt.Errorf("compare summaries: samples: %w", err)
		}
		for _, r := range rows {
			out.Samples = append(out.Samples, SummaryPair{ArticleID: r.ID, Title: r.Title, SummaryA: r.SummaryA, SummaryB: r.SummaryB})
		}
	}
	return out, nil
}

func (s *PostgresStore) ListSummaryModels() ([]ModelRows, error) {
	rows, err := s.q.ListSummaryModels(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list summary models: %w", err)
	}
	out := make([]ModelRows, 0, len(rows))
	for _, r := range rows {
		out = append(out, ModelRows{Model: r.Model, Rows: r.N})
	}
	return out, nil
}

func (s *PostgresStore) ListScreenModels() ([]ModelRows, error) {
	rows, err := s.q.ListScreenModels(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list screen models: %w", err)
	}
	out := make([]ModelRows, 0, len(rows))
	for _, r := range rows {
		out = append(out, ModelRows{Model: r.Model, Rows: r.N})
	}
	return out, nil
}
