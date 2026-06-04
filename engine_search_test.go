package herald

import (
	"context"
	"fmt"
	"testing"

	embedding "github.com/matthewjhunter/go-embedding"
)

type fakeReranker struct {
	fn       func(req embedding.RerankRequest) ([]embedding.RerankResult, error)
	gotDocs  []string
	gotQuery string
}

func (f *fakeReranker) Rerank(_ context.Context, req embedding.RerankRequest) ([]embedding.RerankResult, error) {
	f.gotDocs = req.Documents
	f.gotQuery = req.Query
	return f.fn(req)
}
func (f *fakeReranker) Model() string { return "fake-reranker" }

func titles(rs []SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Title
	}
	return out
}

func TestRerankSearchResults(t *testing.T) {
	mk := func() []SearchResult {
		return []SearchResult{
			{Article: Article{Title: "A", Summary: "alpha"}, MatchType: "fts", Score: 0.9},
			{Article: Article{Title: "B", AISummary: "bravo"}, MatchType: "semantic", Score: 0.8},
			{Article: Article{Title: "C", Summary: "charlie"}, MatchType: "both", Score: 0.7},
		}
	}

	t.Run("reorders by reranker output and feeds title+summary docs", func(t *testing.T) {
		fake := &fakeReranker{fn: func(req embedding.RerankRequest) ([]embedding.RerankResult, error) {
			// Rank C, then A, then B.
			return []embedding.RerankResult{{Index: 2, Score: 5}, {Index: 0, Score: 3}, {Index: 1, Score: 1}}, nil
		}}
		e := &Engine{reranker: fake}
		got := e.rerankSearchResults(context.Background(), "q", mk())
		if g := titles(got); len(g) != 3 || g[0] != "C" || g[1] != "A" || g[2] != "B" {
			t.Fatalf("rerank order = %v, want [C A B]", g)
		}
		// Documents are title + best summary; query passed through.
		if fake.gotQuery != "q" {
			t.Errorf("query = %q", fake.gotQuery)
		}
		if fake.gotDocs[1] != "B\nbravo" { // AISummary preferred over Summary
			t.Errorf("doc[1] = %q, want %q", fake.gotDocs[1], "B\nbravo")
		}
	})

	t.Run("keeps first-stage order when backend unavailable", func(t *testing.T) {
		fake := &fakeReranker{fn: func(embedding.RerankRequest) ([]embedding.RerankResult, error) {
			return nil, fmt.Errorf("dial: %w", embedding.ErrRerankUnavailable)
		}}
		e := &Engine{reranker: fake}
		got := e.rerankSearchResults(context.Background(), "q", mk())
		if g := titles(got); len(g) != 3 || g[0] != "A" || g[1] != "B" || g[2] != "C" {
			t.Fatalf("degraded order = %v, want first-stage [A B C]", g)
		}
	})

	t.Run("no reranker configured is a no-op", func(t *testing.T) {
		e := &Engine{} // reranker nil
		got := e.rerankSearchResults(context.Background(), "q", mk())
		if g := titles(got); g[0] != "A" || g[2] != "C" {
			t.Fatalf("nil reranker should not reorder, got %v", g)
		}
	})
}
