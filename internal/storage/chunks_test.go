package storage

import (
	"testing"
)

// The multi-chunk semantics of #286: an article is a set of vectors, and every
// article-level comparison has to say what it means over that set.

// addChunked adds an article to feedID and stores one chunk per vector.
func addChunked(t *testing.T, s Store, feedID int64, guid, model string, vecs ...[]float32) int64 {
	t.Helper()
	id, err := s.AddArticle(&Article{FeedID: feedID, GUID: guid, Title: guid, URL: "https://example.com/" + guid})
	if err != nil {
		t.Fatalf("add article %s: %v", guid, err)
	}
	chunks := make([]EmbeddingChunk, len(vecs))
	for i, v := range vecs {
		chunks[i] = EmbeddingChunk{Vector: v, StartByte: i * 100, EndByte: (i + 1) * 100}
	}
	if err := s.StoreArticleEmbeddings(id, chunks, model); err != nil {
		t.Fatalf("store embeddings %s: %v", guid, err)
	}
	return id
}

func chunkedFeed(t *testing.T, s Store) int64 {
	t.Helper()
	feedID, err := s.AddFeed("https://example.com/feed", "Feed", "")
	if err != nil {
		t.Fatalf("add feed: %v", err)
	}
	if err := s.SubscribeUserToFeed(1, feedID); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return feedID
}

func TestStoreArticleEmbeddings_RoundTripsEveryChunk(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	const model = "test-model"
	feedID := chunkedFeed(t, store)

	id := addChunked(t, store, feedID, "multi", model, embVec(1, 0), embVec(0, 1), embVec(1, 1))

	rows, err := store.GetArticleEmbeddings(1, model)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows for a 3-chunk article, want 3", len(rows))
	}
	for i, r := range rows {
		if r.ArticleID != id {
			t.Errorf("row %d: article %d, want %d", i, r.ArticleID, id)
		}
		if r.Ordinal != i {
			t.Errorf("row %d: ordinal %d -- rows must come back in ordinal order", i, r.Ordinal)
		}
	}
	if !vecEqual(rows[1].Embedding, embVec(0, 1)) {
		t.Error("chunk 1 did not round-trip")
	}
}

// A re-embed that produces fewer chunks must not leave the old high-ordinal
// vectors behind, or they would keep answering searches for text the article no
// longer has.
func TestStoreArticleEmbeddings_ReplacesRatherThanMerges(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	const model = "test-model"
	feedID := chunkedFeed(t, store)

	id := addChunked(t, store, feedID, "shrinks", model, embVec(1, 0), embVec(0, 1), embVec(1, 1))
	if err := store.StoreArticleEmbeddings(id, []EmbeddingChunk{{Vector: embVec(1, 0), EndByte: 10}}, model); err != nil {
		t.Fatal(err)
	}

	rows, _ := store.GetArticleEmbeddings(1, model)
	if len(rows) != 1 {
		t.Fatalf("got %d rows after re-embedding to one chunk, want 1", len(rows))
	}
}

// A subsequent failure clears the vectors: a stale chunk must not outlive the
// article's ability to embed.
func TestMarkArticleEmbeddingFailed_DropsChunks(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	const model = "test-model"
	feedID := chunkedFeed(t, store)

	id := addChunked(t, store, feedID, "fails", model, embVec(1, 0), embVec(0, 1))
	if err := store.MarkArticleEmbeddingFailed(id, model, "boom"); err != nil {
		t.Fatal(err)
	}

	if rows, _ := store.GetArticleEmbeddings(1, model); len(rows) != 0 {
		t.Errorf("%d chunk rows survived a failure", len(rows))
	}
}

func TestMarkArticleEmbeddingSkipped_DropsChunks(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	const model = "test-model"
	feedID := chunkedFeed(t, store)

	id := addChunked(t, store, feedID, "shortens", model, embVec(1, 0), embVec(0, 1))
	if err := store.MarkArticleEmbeddingSkipped(id, model); err != nil {
		t.Fatal(err)
	}

	if rows, _ := store.GetArticleEmbeddings(1, model); len(rows) != 0 {
		t.Errorf("%d chunk rows survived a skip", len(rows))
	}
}

func TestResetAllArticleEmbeddings_ClearsChunks(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	const model = "test-model"
	feedID := chunkedFeed(t, store)

	addChunked(t, store, feedID, "a", model, embVec(1, 0), embVec(0, 1))
	if _, err := store.ResetAllArticleEmbeddings(); err != nil {
		t.Fatal(err)
	}
	if rows, _ := store.GetArticleEmbeddings(1, model); len(rows) != 0 {
		t.Errorf("%d chunk rows survived the reset", len(rows))
	}
}

// A multi-chunk article must appear once in the results, ranked by its nearest
// chunk. Returning it once per matching chunk would let one long article fill a
// page of results.
func TestSemanticSearch_CollapsesChunksToOneHitPerArticle(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	const model = "test-model"
	feedID := chunkedFeed(t, store)

	query := embVec(1, 0)
	// Three chunks, two of which are close to the query.
	long := addChunked(t, store, feedID, "long", model, embVec(1, 0.5), embVec(1, 0.05), embVec(0, 1))
	// One chunk, between the two matching chunks of the long article in
	// distance, so the ranking distinguishes best-chunk from first-chunk.
	short := addChunked(t, store, feedID, "short", model, embVec(1, 0.2))

	hits, err := store.SemanticSearch(1, model, query, 0.7, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits %v, want one per article", len(hits), hits)
	}
	if hits[0].ArticleID != long || hits[1].ArticleID != short {
		t.Errorf("ranked %d then %d, want %d (best chunk 0.05) then %d (0.2)",
			hits[0].ArticleID, hits[1].ArticleID, long, short)
	}
}

// The article-level distance is the minimum over chunk pairs: a match on one
// passage is enough to link two articles.
func TestLeftoverSimilarPairs_LinksOnBestChunkPair(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	const model = "test-model"
	feedID := chunkedFeed(t, store)

	// a and b share a topic only in their second chunks; their first chunks are
	// orthogonal, so a first-chunk-only comparison would miss the link.
	a := addChunked(t, store, feedID, "a", model, embVec(1, 0), embVec(0, 1))
	b := addChunked(t, store, feedID, "b", model, embVec(0, 0, 1), embVec(0, 1))
	c := addChunked(t, store, feedID, "c", model, embVec(0, 0, 0, 1))

	pairs, err := store.LeftoverSimilarPairs(model, []int64{a, b, c}, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 {
		t.Fatalf("got %d pairs %v, want exactly one (a-b), deduplicated", len(pairs), pairs)
	}
	if lo, hi := pairs[0][0], pairs[0][1]; lo != min(a, b) || hi != max(a, b) {
		t.Errorf("pair = (%d,%d), want (%d,%d)", lo, hi, min(a, b), max(a, b))
	}
}

// Several matching chunk pairs between the same two articles must still yield
// one edge, or the caller's union-find sees duplicates.
func TestLeftoverSimilarPairs_DeduplicatesChunkPairs(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	const model = "test-model"
	feedID := chunkedFeed(t, store)

	a := addChunked(t, store, feedID, "a", model, embVec(1, 0), embVec(1, 0), embVec(1, 0))
	b := addChunked(t, store, feedID, "b", model, embVec(1, 0), embVec(1, 0))

	pairs, err := store.LeftoverSimilarPairs(model, []int64{a, b}, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 {
		t.Errorf("got %d pairs for 6 matching chunk pairs, want 1", len(pairs))
	}
}

// An article joins the group nearest to its best chunk, and produces exactly one
// row whether or not it matched.
func TestMatchArticlesToGroups_OneRowPerArticleOnBestChunk(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	const model = "test-model"
	feedID := chunkedFeed(t, store)

	groupID, err := store.CreateArticleGroup(1, "topic")
	if err != nil {
		t.Fatal(err)
	}
	member := addChunked(t, store, feedID, "member", model, embVec(0, 1))
	if err := store.AddArticleToGroup(groupID, member); err != nil {
		t.Fatal(err)
	}
	if err := store.RecomputeGroupCentroid(groupID, model); err != nil {
		t.Fatal(err)
	}

	// Its first chunk is orthogonal to the centroid; its second matches.
	joiner := addChunked(t, store, feedID, "joiner", model, embVec(1, 0), embVec(0, 1))
	// No chunk anywhere near the centroid.
	loner := addChunked(t, store, feedID, "loner", model, embVec(1, 0), embVec(1, 0.1))

	matches, err := store.MatchArticlesToGroups(1, model, []int64{joiner, loner}, 0.1)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]int64{}
	for _, m := range matches {
		if _, dup := got[m.ArticleID]; dup {
			t.Fatalf("article %d matched more than once", m.ArticleID)
		}
		got[m.ArticleID] = m.GroupID
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows %v, want one per article", len(got), got)
	}
	if got[joiner] != groupID {
		t.Errorf("joiner matched group %d, want %d -- its second chunk is on the centroid", got[joiner], groupID)
	}
	if got[loner] != 0 {
		t.Errorf("loner matched group %d, want none", got[loner])
	}
}

// One article, one vote: a member with many chunks must not pull the centroid
// harder than a member with one.
func TestRecomputeGroupCentroid_WeightsPerArticleNotPerChunk(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	const model = "test-model"
	feedID := chunkedFeed(t, store)

	groupID, err := store.CreateArticleGroup(1, "topic")
	if err != nil {
		t.Fatal(err)
	}
	// Nine chunks all at (1,0), against one article's single chunk at (0,1).
	// Per-chunk averaging would land the centroid at (0.9, 0.1); per-article
	// averaging lands it at (0.5, 0.5).
	long := addChunked(t, store, feedID, "long", model,
		embVec(1, 0), embVec(1, 0), embVec(1, 0), embVec(1, 0), embVec(1, 0),
		embVec(1, 0), embVec(1, 0), embVec(1, 0), embVec(1, 0))
	short := addChunked(t, store, feedID, "short", model, embVec(0, 1))
	for _, id := range []int64{long, short} {
		if err := store.AddArticleToGroup(groupID, id); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.RecomputeGroupCentroid(groupID, model); err != nil {
		t.Fatal(err)
	}

	// Probe the centroid through MatchArticlesToGroups: a (1,1) article is at
	// distance 0 from a (0.5,0.5) centroid and far from a (0.9,0.1) one.
	probe := addChunked(t, store, feedID, "probe", model, embVec(1, 1))
	matches, err := store.MatchArticlesToGroups(1, model, []int64{probe}, 0.01)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].GroupID != groupID {
		t.Errorf("probe did not land on the centroid (%v); the long article outvoted the short one", matches)
	}
}
