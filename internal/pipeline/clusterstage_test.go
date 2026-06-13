package pipeline

import (
	"context"
	"strings"
	"testing"

	embedding "github.com/matthewjhunter/go-embedding"
	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
)

// clusterHarness builds a Stage wired with a fake embedder (only Model is used
// by the cluster stage) and returns it with the store and feed id.
func clusterHarness(t *testing.T, fake *fakeAI) (*Stage, storage.Store, int64) {
	t.Helper()
	st, store, feedID := newHarness(t, fake)
	st.Embedder = &fakeEmbedder{model: "m"}
	return st, store, feedID
}

// embed stores a vector for an article under model "m".
func embedArt(t *testing.T, store storage.Store, a storage.Article, vec []float32) {
	t.Helper()
	if err := store.StoreArticleEmbedding(a.ID, embedding.EncodeFloat32s(vec), "m"); err != nil {
		t.Fatalf("StoreArticleEmbedding: %v", err)
	}
}

func groupOf(t *testing.T, store storage.Store, articleID int64) *int64 {
	t.Helper()
	gid, err := store.FindArticleGroup(articleID, 1)
	if err != nil {
		t.Fatalf("FindArticleGroup: %v", err)
	}
	return gid
}

func TestClusterFormsNewGroupFromSiblings(t *testing.T) {
	st, store, feedID := clusterHarness(t, &fakeAI{available: true})

	a := seed(t, store, feedID, "a", "body a")
	b := seed(t, store, feedID, "b", "body b")
	c := seed(t, store, feedID, "c", "body c")
	embedArt(t, store, a, []float32{1, 0, 0})
	embedArt(t, store, b, []float32{0.99, 0.1, 0}) // ~1.0 cosine with a
	embedArt(t, store, c, []float32{0, 0, 1})      // unrelated
	// Summaries + scores so the naming step has something to work with.
	for _, art := range []storage.Article{a, b} {
		if err := store.UpdateArticleAISummary(art.ID, "summary of "+art.GUID); err != nil {
			t.Fatal(err)
		}
		if err := store.SetInterestScore(1, art.ID, 8); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.Cluster(context.Background(), []storage.Article{a, b, c}); err != nil {
		t.Fatalf("Cluster: %v", err)
	}

	ga, gb, gc := groupOf(t, store, a.ID), groupOf(t, store, b.ID), groupOf(t, store, c.ID)
	if ga == nil || gb == nil {
		t.Fatalf("expected a and b grouped, got a=%v b=%v", ga, gb)
	}
	if *ga != *gb {
		t.Fatalf("a and b should share a group, got %d and %d", *ga, *gb)
	}
	if gc != nil {
		t.Fatalf("dissimilar article c should stay ungrouped, got group %d", *gc)
	}
	// The new group was named via the LLM.
	if sum, err := store.GetGroupSummary(*ga); err != nil || sum == nil || sum.Headline == "" {
		t.Fatalf("expected the new group to be named, got %+v (err %v)", sum, err)
	}
}

// An empty group summary (degenerate/over-large cluster) must not trigger topic
// refinement — otherwise the model replies conversationally and the chatter is
// stored as the group name (the "Please provide the summary…" prod bug).
func TestClusterSkipsTopicRefineOnEmptySummary(t *testing.T) {
	fake := &fakeAI{available: true}
	fake.groupSummaryFn = func(string, []ai.GroupSummaryInput) (*ai.GroupSummaryResult, error) {
		return &ai.GroupSummaryResult{Headline: "Headline", Summary: ""}, nil // degenerate
	}
	refineCalled := false
	fake.refineTopicFn = func(string) (string, error) {
		refineCalled = true
		return "Please provide the summary of related news articles so I can generate the topic label.", nil
	}
	st, store, feedID := clusterHarness(t, fake)

	a := seed(t, store, feedID, "a", "body a")
	b := seed(t, store, feedID, "b", "body b")
	c := seed(t, store, feedID, "c", "body c")
	embedArt(t, store, a, []float32{1, 0, 0})
	embedArt(t, store, b, []float32{0.99, 0.05, 0})
	embedArt(t, store, c, []float32{0.98, 0.1, 0}) // all three cluster together (>=3 → refine path)
	for _, art := range []storage.Article{a, b, c} {
		if err := store.UpdateArticleAISummary(art.ID, "summary of "+art.GUID); err != nil {
			t.Fatal(err)
		}
		if err := store.SetInterestScore(1, art.ID, 8); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.Cluster(context.Background(), []storage.Article{a, b, c}); err != nil {
		t.Fatalf("Cluster: %v", err)
	}

	gid := groupOf(t, store, a.ID)
	if gid == nil {
		t.Fatal("expected a group to form")
	}
	g, err := store.GetGroup(*gid)
	if err != nil || g == nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if refineCalled {
		t.Error("RefineGroupTopic must not be called when the group summary is empty")
	}
	if strings.Contains(g.Topic, "Please provide") {
		t.Fatalf("conversational reply leaked into group topic: %q", g.Topic)
	}
}

// The grouping toggle (Cfg.Grouping.Enabled) gates the orchestrator's cluster
// pass: enabled groups the recent cohort, disabled leaves it ungrouped.
func TestClusterRecentRespectsToggle(t *testing.T) {
	run := func(enabled bool) (storage.Store, int64) {
		st, store, feedID := clusterHarness(t, &fakeAI{available: true})
		st.Cfg.Grouping.Enabled = enabled
		a := seed(t, store, feedID, "a", "body a")
		b := seed(t, store, feedID, "b", "body b")
		embedArt(t, store, a, []float32{1, 0, 0})
		embedArt(t, store, b, []float32{0.99, 0.05, 0})
		for _, art := range []storage.Article{a, b} {
			if err := store.ScreenArticleSecurity(art.ID, 9, "ok", false); err != nil {
				t.Fatal(err)
			}
			if err := store.SetInterestScore(1, art.ID, 8); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.clusterRecent(context.Background()); err != nil {
			t.Fatalf("clusterRecent: %v", err)
		}
		return store, a.ID
	}

	// Positive control: enabled groups the cohort (also proves the setup is valid).
	if store, id := run(true); groupOf(t, store, id) == nil {
		t.Fatal("grouping enabled: expected the cohort to be grouped")
	}
	// Toggle off: no group forms.
	if store, id := run(false); groupOf(t, store, id) != nil {
		t.Fatal("grouping disabled: expected the cohort to stay ungrouped")
	}
}

func TestClusterJoinsExistingGroup(t *testing.T) {
	st, store, feedID := clusterHarness(t, &fakeAI{available: true})

	// An existing group whose centroid points at [1,0,0].
	gid, _ := store.CreateArticleGroup(1, "ongoing story")
	if err := store.UpdateGroupEmbedding(gid, embedding.EncodeFloat32s([]float32{1, 0, 0}), "m"); err != nil {
		t.Fatal(err)
	}

	x := seed(t, store, feedID, "x", "body x")
	embedArt(t, store, x, []float32{0.98, 0.05, 0}) // close to the centroid

	if err := st.Cluster(context.Background(), []storage.Article{x}); err != nil {
		t.Fatalf("Cluster: %v", err)
	}

	gx := groupOf(t, store, x.ID)
	if gx == nil || *gx != gid {
		t.Fatalf("expected x to join group %d, got %v", gid, gx)
	}
}

func TestClusterJoinMutedGroupMarksRead(t *testing.T) {
	st, store, feedID := clusterHarness(t, &fakeAI{available: true})

	gid, _ := store.CreateArticleGroup(1, "muted story")
	if err := store.UpdateGroupEmbedding(gid, embedding.EncodeFloat32s([]float32{1, 0, 0}), "m"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupMuted(gid, true); err != nil {
		t.Fatal(err)
	}

	x := seed(t, store, feedID, "x", "body x")
	embedArt(t, store, x, []float32{1, 0, 0})

	if err := st.Cluster(context.Background(), []storage.Article{x}); err != nil {
		t.Fatalf("Cluster: %v", err)
	}

	if gx := groupOf(t, store, x.ID); gx == nil || *gx != gid {
		t.Fatalf("expected x to join muted group %d, got %v", gid, gx)
	}
	unread, _ := store.GetUnreadArticleIDsForUser(1)
	for _, id := range unread {
		if id == x.ID {
			t.Fatal("article joining a muted group should be marked read")
		}
	}
}

func TestClusterEmptyCohortIsNoop(t *testing.T) {
	called := false
	fake := &fakeAI{available: true}
	fake.groupSummaryFn = func(string, []ai.GroupSummaryInput) (*ai.GroupSummaryResult, error) {
		called = true
		return &ai.GroupSummaryResult{}, nil
	}
	st, store, _ := clusterHarness(t, fake)

	if err := st.Cluster(context.Background(), nil); err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if called {
		t.Fatal("empty cohort must not call the LLM")
	}
	groups, _ := store.GetUserGroups(1)
	if len(groups) != 0 {
		t.Fatalf("empty cohort must not create groups, got %d", len(groups))
	}
}

func TestClusterSkipsWhenBreakerOpen(t *testing.T) {
	st, store, feedID := clusterHarness(t, &fakeAI{available: false})
	a := seed(t, store, feedID, "a", "body a")
	b := seed(t, store, feedID, "b", "body b")
	embedArt(t, store, a, []float32{1, 0, 0})
	embedArt(t, store, b, []float32{1, 0, 0})

	if err := st.Cluster(context.Background(), []storage.Article{a, b}); err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	groups, _ := store.GetUserGroups(1)
	if len(groups) != 0 {
		t.Fatalf("breaker open must skip clustering, got %d groups", len(groups))
	}
}
