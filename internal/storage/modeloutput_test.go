package storage

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelOutput_RecordAndList(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ids := seedUnscreened(t, store, 2)

		// A synchronous local call: trainable by default, keywords travel with it.
		id, err := store.RecordModelOutput(ModelOutput{
			Kind: ModelOutputCurate, ArticleID: ids[0], UserID: ptrI64(1),
			Model: "gemma4:e4b", PromptHash: "h1", ContentHash: ContentHash("body"),
			Keywords: ptrStr("go, security"), Response: `{"interest_score": 7, "reasoning": "x"}`,
			Parsed: map[string]any{"interest_score": 7.0}, Via: ModelOutputViaSync, Trainable: true,
		})
		if err != nil || id == 0 {
			t.Fatalf("record sync: %d %v", id, err)
		}
		// A batch result: the writer says it is not trainable.
		if _, err := store.RecordModelOutput(ModelOutput{
			Kind: ModelOutputScreen, ArticleID: ids[0], ChunkIndex: 1, ChunkCount: 2,
			Model: "claude-sonnet-5-5", PromptHash: "h2", ContentHash: ContentHash("body"),
			Response: `{"threat": 0}`, Via: ModelOutputViaBatch("anthropic"), Trainable: false,
		}); err != nil {
			t.Fatalf("record batch: %v", err)
		}
		if _, err := store.RecordModelOutput(ModelOutput{
			Kind: ModelOutputSummarize, ArticleID: ids[1],
			Model: "m", PromptHash: "h3", ContentHash: ContentHash("other"), Response: "A summary.", Via: ModelOutputViaSync, Trainable: true,
		}); err != nil {
			t.Fatalf("record summary: %v", err)
		}

		rows, err := store.ListModelOutputsByArticle(ids[0])
		if err != nil || len(rows) != 2 {
			t.Fatalf("list: %d rows, %v", len(rows), err)
		}
		cur, scr := rows[0], rows[1]
		if cur.Kind != ModelOutputCurate || cur.UserID == nil || *cur.UserID != 1 || cur.Keywords == nil || *cur.Keywords != "go, security" || !cur.Trainable || cur.Via != "sync" {
			t.Errorf("curate row = %+v", cur)
		}
		if raw, _ := cur.Parsed.(json.RawMessage); !strings.Contains(string(raw), `"interest_score"`) {
			t.Errorf("parsed not stored: %s", cur.Parsed)
		}
		if scr.Kind != ModelOutputScreen || scr.UserID != nil || scr.ChunkIndex != 1 || scr.ChunkCount != 2 || scr.Trainable || scr.Via != "batch:anthropic" || scr.Keywords != nil {
			t.Errorf("screen row = %+v", scr)
		}
		if cur.ContentHash != scr.ContentHash || len(cur.ContentHash) != 64 {
			t.Errorf("content hash = %q / %q", cur.ContentHash, scr.ContentHash)
		}

		// The exporter's page sees only trainable rows of its kind.
		page, err := store.ListTrainableModelOutputs(ModelOutputScreen, 0, 10)
		if err != nil || len(page) != 0 {
			t.Fatalf("trainable screen rows = %d, %v", len(page), err)
		}
		page, err = store.ListTrainableModelOutputs(ModelOutputCurate, 0, 10)
		if err != nil || len(page) != 1 || page[0].ID != id {
			t.Fatalf("trainable curate rows = %+v, %v", page, err)
		}
		if page, _ = store.ListTrainableModelOutputs(ModelOutputCurate, id, 10); len(page) != 0 {
			t.Errorf("cursor did not advance: %d rows", len(page))
		}
	})
}

func TestModelOutput_ResponseCapAndValidation(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ids := seedUnscreened(t, store, 1)
		long := strings.Repeat("x", maxModelOutputResponse+100)
		if _, err := store.RecordModelOutput(ModelOutput{
			Kind: ModelOutputSummarize, ArticleID: ids[0], Model: "m", PromptHash: "h",
			ContentHash: ContentHash(""), Response: long, Via: ModelOutputViaSync, Trainable: true,
		}); err != nil {
			t.Fatalf("record long: %v", err)
		}
		rows, _ := store.ListModelOutputsByArticle(ids[0])
		if len(rows) != 1 || len(rows[0].Response) > maxModelOutputResponse+len(modelOutputTruncated) || !strings.HasSuffix(rows[0].Response, modelOutputTruncated) {
			t.Fatalf("response not capped: len %d", len(rows[0].Response))
		}

		for _, bad := range []ModelOutput{
			{Kind: "digest", ArticleID: ids[0], Model: "m", PromptHash: "h", ContentHash: "c", Response: "r", Via: ModelOutputViaSync},
			{Kind: ModelOutputCurate, ArticleID: ids[0], Model: "", PromptHash: "h", ContentHash: "c", Response: "r", Via: ModelOutputViaSync},
			{Kind: ModelOutputCurate, ArticleID: ids[0], Model: "m", PromptHash: "h", ContentHash: "c", Response: "r", Via: ""},
		} {
			if _, err := store.RecordModelOutput(bad); err == nil {
				t.Errorf("accepted invalid output %+v", bad)
			}
		}
	})
}

func TestContentHash(t *testing.T) {
	a, b := ContentHash("a"), ContentHash("b")
	if a == b || len(a) != 64 || a != ContentHash("a") {
		t.Fatal("ContentHash is not a stable 64-hex digest")
	}
}
