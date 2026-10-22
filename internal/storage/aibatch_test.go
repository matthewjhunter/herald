package storage

import (
	"testing"
	"time"
)

func ptrI64(v int64) *int64     { return &v }
func ptrF64(v float64) *float64 { return &v }
func ptrStr(v string) *string   { return &v }
func ptrBool(v bool) *bool      { return &v }

func TestAIBatch_RecordListResolveFinish(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ids := seedUnscreened(t, store, 2)
		expires := time.Now().Add(24 * time.Hour).Truncate(time.Second)

		b := AIBatch{ID: "msgbatch_1", Provider: "anthropic", Kind: "screen", Model: "m", ExpiresAt: &expires}
		reqs := []AIBatchRequest{
			{CustomID: "s-1-0", Kind: "screen", SubjectKey: "1", ArticleID: ptrI64(ids[0]), ChunkIndex: 0, ChunkCount: 2, Model: "m", PromptHash: "h"},
			{CustomID: "s-1-1", Kind: "screen", SubjectKey: "1", ArticleID: ptrI64(ids[0]), ChunkIndex: 1, ChunkCount: 2, Model: "m", PromptHash: "h"},
			{CustomID: "s-2-0", Kind: "screen", SubjectKey: "2", ArticleID: ptrI64(ids[1]), Model: "m"},
		}
		if err := store.RecordAIBatch(b, reqs); err != nil {
			t.Fatalf("record: %v", err)
		}
		// A second record of the same id is a conflict, never a silent overwrite.
		if err := store.RecordAIBatch(b, reqs); err == nil {
			t.Fatal("duplicate batch id accepted")
		}

		got, err := store.GetAIBatch("msgbatch_1")
		if err != nil || got == nil {
			t.Fatalf("get: %v %v", got, err)
		}
		if got.Status != AIBatchRunning || got.RequestCount != 3 || got.Provider != "anthropic" || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
			t.Fatalf("batch = %+v", got)
		}
		if missing, err := store.GetAIBatch("nope"); err != nil || missing != nil {
			t.Fatalf("unknown id: %v %v", missing, err)
		}

		running, err := store.ListRunningAIBatches()
		if err != nil || len(running) != 1 || running[0].ID != "msgbatch_1" {
			t.Fatalf("running = %+v %v", running, err)
		}

		if err := store.TouchAIBatchPolled("msgbatch_1"); err != nil {
			t.Fatal(err)
		}
		got, _ = store.GetAIBatch("msgbatch_1")
		if got.LastPolledAt == nil {
			t.Fatal("last_polled_at not stamped")
		}

		list, err := store.ListAIBatchRequests("msgbatch_1")
		if err != nil || len(list) != 3 {
			t.Fatalf("requests = %d %v", len(list), err)
		}
		for _, r := range list {
			if r.Outcome != AIBatchPending || r.ResolvedAt != nil {
				t.Fatalf("fresh request not pending: %+v", r)
			}
			if len(r.ArticleIDs) != 1 || r.ArticleIDs[0] != *r.ArticleID {
				t.Fatalf("covered articles not derived from article_id: %+v", r)
			}
		}
		if list[0].ChunkCount != 2 || list[0].PromptHash != "h" || list[2].ChunkCount != 1 || list[2].PromptHash != "" {
			t.Fatalf("chunk/provenance fields: %+v %+v", list[0], list[2])
		}

		if err := store.ResolveAIBatchRequest(AIBatchRequest{
			BatchID: "msgbatch_1", CustomID: "s-1-0", Outcome: AIBatchSucceeded,
			Threat: ptrF64(2), Category: ptrStr("none"), Verified: ptrBool(true),
			Usage: AIBatchUsage{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 1700},
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.ResolveAIBatchRequest(AIBatchRequest{BatchID: "msgbatch_1", CustomID: "s-2-0", Outcome: AIBatchErrored, Error: "max_tokens"}); err != nil {
			t.Fatal(err)
		}
		list, _ = store.ListAIBatchRequests("msgbatch_1")
		byID := map[string]AIBatchRequest{}
		for _, r := range list {
			byID[r.CustomID] = r
		}
		ok := byID["s-1-0"]
		if ok.Outcome != AIBatchSucceeded || ok.ResolvedAt == nil || ok.Threat == nil || *ok.Threat != 2 || *ok.Category != "none" || !*ok.Verified || ok.Usage.CacheReadTokens != 1700 {
			t.Fatalf("resolved = %+v", ok)
		}
		if bad := byID["s-2-0"]; bad.Outcome != AIBatchErrored || bad.Error != "max_tokens" || bad.Threat != nil {
			t.Fatalf("errored = %+v", bad)
		}
		if byID["s-1-1"].Outcome != AIBatchPending {
			t.Fatal("untouched request changed")
		}

		if err := store.FinishAIBatch("msgbatch_1", AIBatchEnded, "", AIBatchUsage{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 1700}); err != nil {
			t.Fatal(err)
		}
		got, _ = store.GetAIBatch("msgbatch_1")
		if got.Status != AIBatchEnded || got.EndedAt == nil || got.Usage.CacheReadTokens != 1700 || got.Error != "" {
			t.Fatalf("finished = %+v", got)
		}
		if running, _ := store.ListRunningAIBatches(); len(running) != 0 {
			t.Fatalf("finished batch still listed as running: %+v", running)
		}

		if err := store.FinishAIBatch("msgbatch_1", AIBatchFailed, "gateway 502", AIBatchUsage{}); err != nil {
			t.Fatal(err)
		}
		got, _ = store.GetAIBatch("msgbatch_1")
		if got.Status != AIBatchFailed || got.Error != "gateway 502" {
			t.Fatalf("failed = %+v", got)
		}
	})
}

func TestAIBatch_MultiArticleRequest(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ids := seedUnscreened(t, store, 3)
		b := AIBatch{ID: "msgbatch_d", Provider: "anthropic", Kind: "digest", Model: "m", Adopted: true}
		req := AIBatchRequest{CustomID: "d-1-20261021", Kind: "digest", UserID: 1, SubjectKey: "1:20261021", ArticleIDs: ids, Model: "m"}
		if err := store.RecordAIBatch(b, []AIBatchRequest{req}); err != nil {
			t.Fatal(err)
		}
		list, err := store.ListAIBatchRequests("msgbatch_d")
		if err != nil || len(list) != 1 {
			t.Fatalf("requests = %v %v", list, err)
		}
		if list[0].ArticleID != nil || len(list[0].ArticleIDs) != 3 {
			t.Fatalf("digest request = %+v", list[0])
		}
		got, _ := store.GetAIBatch("msgbatch_d")
		if !got.Adopted {
			t.Fatal("adopted flag lost")
		}

	})
}

func TestGetUnbatchedCurationArticles_ExcludesPending(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		ids := seedUnscreened(t, store, 2)
		for _, id := range ids {
			if err := store.ScreenArticleSecurity(id, 0, "none", true, false); err != nil {
				t.Fatal(err)
			}
		}
		all, err := store.GetUnbatchedCurationArticles(1, 3.0, 10)
		if err != nil || len(all) != 2 {
			t.Fatalf("before: %d %v", len(all), err)
		}

		b := AIBatch{ID: "msgbatch_c", Provider: "anthropic", Kind: "curate", Model: "m"}
		req := AIBatchRequest{CustomID: "c-x-1", Kind: "curate", UserID: 1, SubjectKey: "x", ArticleID: ptrI64(ids[0]), Model: "m"}
		if err := store.RecordAIBatch(b, []AIBatchRequest{req}); err != nil {
			t.Fatal(err)
		}
		left, err := store.GetUnbatchedCurationArticles(1, 3.0, 10)
		if err != nil || len(left) != 1 || left[0].ID != ids[1] {
			t.Fatalf("pending not excluded: %+v %v", left, err)
		}
		// Another user's pending request does not hide the article from this one.
		if err := store.RecordAIBatch(AIBatch{ID: "msgbatch_c2", Provider: "anthropic", Kind: "curate", Model: "m"},
			[]AIBatchRequest{{CustomID: "c-y-2", Kind: "curate", UserID: 2, SubjectKey: "y", ArticleID: ptrI64(ids[1]), Model: "m"}}); err != nil {
			t.Fatal(err)
		}
		left, _ = store.GetUnbatchedCurationArticles(1, 3.0, 10)
		if len(left) != 1 {
			t.Fatalf("other user's request hid an article: %+v", left)
		}

		// Once resolved (even without a score written), the article is eligible again.
		if err := store.ResolveAIBatchRequest(AIBatchRequest{BatchID: "msgbatch_c", CustomID: "c-x-1", Outcome: AIBatchExpired}); err != nil {
			t.Fatal(err)
		}
		left, _ = store.GetUnbatchedCurationArticles(1, 3.0, 10)
		if len(left) != 2 {
			t.Fatalf("expired request still excludes: %+v", left)
		}
		// The synchronous query is untouched by batch state.
		sync, _ := store.GetUnscoredCurationArticles(1, 3.0, 10)
		if len(sync) != 2 {
			t.Fatalf("sync query = %d", len(sync))
		}
	})
}
