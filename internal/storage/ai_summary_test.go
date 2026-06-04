package storage

import (
	"testing"
	"time"
)

func seedScored(t *testing.T, store Store, feedID, userID int64, guid string, read bool, security, interest float64) int64 {
	t.Helper()
	now := time.Now()
	id, err := store.AddArticle(&Article{FeedID: feedID, GUID: guid, Title: guid,
		URL: "https://example.com/" + guid, Content: "body of " + guid, PublishedDate: &now})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}
	if err := store.UpdateReadState(userID, id, false, &interest, &security, nil, nil); err != nil {
		t.Fatalf("UpdateReadState scores: %v", err)
	}
	if read {
		if err := store.UpdateReadState(userID, id, true, nil, nil, nil, nil); err != nil {
			t.Fatalf("UpdateReadState read: %v", err)
		}
	}
	return id
}

func TestGetUnreadArticlesForSummary(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	uid, _ := store.CreateUser("u")
	feedID, _ := store.AddFeed("https://example.com/feed", "Feed", "")
	if err := store.SubscribeUserToFeed(uid, feedID); err != nil {
		t.Fatal(err)
	}

	want := seedScored(t, store, feedID, uid, "keep", false, 8, 8) // included
	seedScored(t, store, feedID, uid, "lowinterest", false, 8, 5)  // interest < 7
	seedScored(t, store, feedID, uid, "alreadyread", true, 8, 9)   // read
	seedScored(t, store, feedID, uid, "lowsecurity", false, 5, 9)  // security < 7

	got, err := store.GetUnreadArticlesForSummary(uid, 7, 7, 100)
	if err != nil {
		t.Fatalf("GetUnreadArticlesForSummary: %v", err)
	}
	if len(got) != 1 || got[0].ID != want {
		ids := make([]int64, len(got))
		for i, a := range got {
			ids[i] = a.ID
		}
		t.Fatalf("expected only article %d, got %v", want, ids)
	}
}

func TestAISummaryLifecycle(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	uid, _ := store.CreateUser("u")

	// No summary yet.
	if latest, err := store.GetLatestAISummary(uid); err != nil || latest != nil {
		t.Fatalf("expected no summary, got %v err=%v", latest, err)
	}

	id, err := store.CreateAISummary(&AISummary{UserID: uid, Model: "claude-sonnet-4-6", Prompt: "summarize"})
	if err != nil {
		t.Fatalf("CreateAISummary: %v", err)
	}

	// In progress until completed.
	inprog, err := store.GetInProgressAISummary(uid)
	if err != nil || inprog == nil || inprog.ID != id || inprog.Status != "generating" {
		t.Fatalf("expected in-progress id=%d, got %+v err=%v", id, inprog, err)
	}

	ids := []int64{11, 22, 33}
	if err := store.UpdateAISummaryDone(id, "Top stories", "<p>digest</p>", ids, 150000, 1200); err != nil {
		t.Fatalf("UpdateAISummaryDone: %v", err)
	}

	// No longer in progress.
	if inprog, _ := store.GetInProgressAISummary(uid); inprog != nil {
		t.Fatalf("expected no in-progress summary after done, got %+v", inprog)
	}

	latest, err := store.GetLatestAISummary(uid)
	if err != nil || latest == nil {
		t.Fatalf("GetLatestAISummary: %v err=%v", latest, err)
	}
	if latest.Status != "done" || latest.Headline != "Top stories" || latest.ContentHTML != "<p>digest</p>" {
		t.Fatalf("unexpected summary fields: %+v", latest)
	}
	if latest.ArticleCount != 3 || len(latest.ArticleIDs) != 3 || latest.ArticleIDs[2] != 33 {
		t.Fatalf("article IDs not round-tripped: %+v", latest.ArticleIDs)
	}
	if latest.InputTokens != 150000 || latest.OutputTokens != 1200 {
		t.Fatalf("token usage not stored: in=%d out=%d", latest.InputTokens, latest.OutputTokens)
	}
	if latest.GeneratedAt == nil {
		t.Fatalf("generated_at should be set on done")
	}
}

func TestAISummaryFailed(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	uid, _ := store.CreateUser("u")
	id, _ := store.CreateAISummary(&AISummary{UserID: uid, Model: "m"})

	if err := store.UpdateAISummaryFailed(id, "backend timeout"); err != nil {
		t.Fatalf("UpdateAISummaryFailed: %v", err)
	}
	latest, _ := store.GetLatestAISummary(uid)
	if latest == nil || latest.Status != "failed" || latest.Error != "backend timeout" {
		t.Fatalf("expected failed summary, got %+v", latest)
	}
	if inprog, _ := store.GetInProgressAISummary(uid); inprog != nil {
		t.Fatalf("failed summary should not be in-progress, got %+v", inprog)
	}
}
