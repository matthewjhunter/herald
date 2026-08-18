package storage

import (
	"context"
	"sort"
	"strconv"
	"testing"

	"github.com/pgvector/pgvector-go"

	embedding "github.com/matthewjhunter/go-embedding"
)

// storeEmbedding stores a single-chunk embedding for an article, which is what
// most tests want: they are about the distance queries, not about chunking. The
// byte span is nominal -- nothing reads it back.
func storeEmbedding(s Store, articleID int64, vec []float32, model string) error {
	return s.StoreArticleEmbeddings(articleID, []EmbeddingChunk{{Vector: vec, StartByte: 0, EndByte: 1}}, model)
}

// TestSemanticSearch checks the hybrid-search ANN path (#192): it must return a
// user's subscribed-feed articles within the distance threshold in nearest-first
// order, exclude other users' articles even when those are globally nearer to
// the query, and not under-return when the index's nearest rows all belong to a
// different user (the iterative_scan guard). It cross-checks the result against a
// brute-force Go cosine scan, the path it replaces.
func TestSemanticSearch(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	const model = "test-model"
	maxDist := 1.0 - 0.3 // engine's sim > 0.3 gate, as a distance ceiling

	feedMine, _ := store.AddFeed("https://example.com/mine", "Mine", "")
	feedOther, _ := store.AddFeed("https://example.com/other", "Other", "")
	if err := store.SubscribeUserToFeed(1, feedMine); err != nil {
		t.Fatalf("subscribe user 1: %v", err)
	}
	if err := store.SubscribeUserToFeed(2, feedOther); err != nil {
		t.Fatalf("subscribe user 2: %v", err)
	}

	query := embVec(1, 0)

	// User 1's articles, spanning the threshold.
	mine := []struct {
		guid string
		vec  []float32
		want bool // within maxDist of query
	}{
		{"near", embVec(1, 0.1), true}, // distance ~0.005
		{"mid", embVec(1, 1), true},    // distance ~0.293
		{"far", embVec(0, 1), false},   // distance 1.0, excluded
	}
	mineID := map[string]int64{}
	for _, m := range mine {
		id, err := store.AddArticle(&Article{FeedID: feedMine, GUID: m.guid, Title: m.guid, URL: "https://example.com/" + m.guid})
		if err != nil {
			t.Fatalf("add article %s: %v", m.guid, err)
		}
		mineID[m.guid] = id
		if err := storeEmbedding(store, id, m.vec, model); err != nil {
			t.Fatalf("store embedding %s: %v", m.guid, err)
		}
	}

	// User 2 gets many articles identical to the query -- the globally-nearest
	// rows in the index. Without iterative_scan, a LIMIT scan would return these,
	// the user filter would drop them all, and user 1's hits would be lost.
	for i := 0; i < 25; i++ {
		id, err := store.AddArticle(&Article{FeedID: feedOther, GUID: "other-" + strconv.Itoa(i), Title: "other", URL: "https://example.com/other/" + strconv.Itoa(i)})
		if err != nil {
			t.Fatalf("add other article: %v", err)
		}
		if err := storeEmbedding(store, id, embVec(1, 0), model); err != nil {
			t.Fatalf("store other embedding: %v", err)
		}
	}

	hits, err := store.SemanticSearch(1, model, query, maxDist, 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}

	// Only user 1's in-threshold articles, nearest first.
	gotIDs := make([]int64, len(hits))
	for i, h := range hits {
		gotIDs[i] = h.ArticleID
	}
	wantIDs := []int64{mineID["near"], mineID["mid"]}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("SemanticSearch returned %d hits %v, want %d %v (under-return or leaked another user's rows)", len(gotIDs), gotIDs, len(wantIDs), wantIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("hit[%d] = %d, want %d (order %v, want %v)", i, gotIDs[i], wantIDs[i], gotIDs, wantIDs)
		}
	}

	// Distances are ascending and below the ceiling.
	for i, h := range hits {
		if h.Distance >= maxDist {
			t.Errorf("hit[%d] distance %.4f >= ceiling %.4f", i, h.Distance, maxDist)
		}
		if i > 0 && h.Distance < hits[i-1].Distance {
			t.Errorf("hits not in ascending distance order: [%d]=%.4f < [%d]=%.4f", i, h.Distance, i-1, hits[i-1].Distance)
		}
	}

	// Cross-check against the brute-force Go cosine scan it replaces.
	type scored struct {
		id  int64
		sim float64
	}
	var brute []scored
	for _, m := range mine {
		sim := embedding.CosineSimilarity(query, m.vec)
		if sim > 0.3 {
			brute = append(brute, scored{mineID[m.guid], sim})
		}
	}
	sort.Slice(brute, func(i, j int) bool { return brute[i].sim > brute[j].sim })
	if len(brute) != len(hits) {
		t.Fatalf("brute scan found %d hits, ANN found %d", len(brute), len(hits))
	}
	for i := range brute {
		if brute[i].id != hits[i].ArticleID {
			t.Errorf("rank %d: brute id %d, ANN id %d", i, brute[i].id, hits[i].ArticleID)
		}
	}
}

// TestSemanticSearchScopedAtScale guards the per-user scoping when another,
// unsubscribed user owns many articles that are nearer to the query than the
// searcher's own. An ANN index here would under-return (its globally-nearest
// rows are the other user's, dropped by the JOIN); the exact scan SemanticSearch
// uses cannot -- it must still return the searcher's two in-threshold articles.
//
// White-box: it reaches the pool to bulk-seed the decoy rows, far cheaper than a
// thousand API round-trips.
func TestSemanticSearchScopedAtScale(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	ps, ok := store.(*PostgresStore)
	if !ok {
		t.Skip("not a PostgresStore")
	}
	ctx := context.Background()

	const model = "test-model"
	maxDist := 1.0 - 0.3

	feedMine, _ := store.AddFeed("https://example.com/mine", "Mine", "")
	feedOther, _ := store.AddFeed("https://example.com/other", "Other", "")
	if err := store.SubscribeUserToFeed(1, feedMine); err != nil {
		t.Fatalf("subscribe user 1: %v", err)
	}
	if err := store.SubscribeUserToFeed(2, feedOther); err != nil {
		t.Fatalf("subscribe user 2: %v", err)
	}

	query := embVec(1, 0)

	// Two in-threshold articles for the subscribed user.
	wantIDs := make([]int64, 0, 2)
	for _, m := range []struct {
		guid string
		vec  []float32
	}{
		{"near", embVec(1, 0.1)}, // distance ~0.005
		{"mid", embVec(1, 1)},    // distance ~0.293
	} {
		id, err := store.AddArticle(&Article{FeedID: feedMine, GUID: m.guid, Title: m.guid, URL: "https://example.com/" + m.guid})
		if err != nil {
			t.Fatalf("add article %s: %v", m.guid, err)
		}
		if err := storeEmbedding(store, id, m.vec, model); err != nil {
			t.Fatalf("store embedding %s: %v", m.guid, err)
		}
		wantIDs = append(wantIDs, id)
	}

	// 1500 decoy articles for the OTHER (unsubscribed) user, every one identical
	// to the query so they would crowd out the searcher's hits under any nearest-
	// first index. Bulk-inserted on the pool: one statement for the articles, one
	// for their embeddings.
	const decoys = 1500
	if _, err := ps.pool.Exec(ctx, `
		INSERT INTO articles (feed_id, guid, title, url)
		SELECT $1, 'decoy-' || g, 'decoy', 'https://example.com/decoy/' || g
		FROM generate_series(1, $2) g`, feedOther, decoys); err != nil {
		t.Fatalf("seed decoy articles: %v", err)
	}
	if _, err := ps.pool.Exec(ctx, `
		INSERT INTO article_embedding_chunks (article_id, embedding_model, ordinal, embedding, start_byte, end_byte)
		SELECT a.id, $1, 0, $2, 0, 1 FROM articles a
		WHERE a.feed_id = $3 AND a.guid LIKE 'decoy-%'`,
		model, pgvector.NewVector(query), feedOther); err != nil {
		t.Fatalf("seed decoy embeddings: %v", err)
	}

	hits, err := store.SemanticSearch(1, model, query, maxDist, 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	gotIDs := make([]int64, len(hits))
	for i, h := range hits {
		gotIDs[i] = h.ArticleID
	}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("got %d hits %v, want %d %v -- the searcher's articles were crowded out by another user's nearer rows", len(gotIDs), gotIDs, len(wantIDs), wantIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("hit[%d] = %d, want %d (order %v, want %v)", i, gotIDs[i], wantIDs[i], gotIDs, wantIDs)
		}
	}
}
