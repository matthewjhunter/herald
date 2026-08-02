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
	// Security verdict is article-level (#141); interest stays per-user.
	if err := store.ScreenArticleSecurity(id, 10-security, "none", false, false); err != nil {
		t.Fatalf("ScreenArticleSecurity: %v", err)
	}
	if err := store.UpdateReadState(userID, id, false, &interest); err != nil {
		t.Fatalf("UpdateReadState scores: %v", err)
	}
	if read {
		if err := store.UpdateReadState(userID, id, true, nil); err != nil {
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

	got, err := store.GetUnreadArticlesForSummary(uid, 3, 7, 100, false)
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

func TestGetAISummaries(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	uid, _ := store.CreateUser("u")
	other, _ := store.CreateUser("other")

	id1, _ := store.CreateAISummary(&AISummary{UserID: uid, Model: "m"})
	store.UpdateAISummaryDone(id1, "First", "<p>1</p>", []int64{1, 2}, 100, 10) //nolint:errcheck
	id2, _ := store.CreateAISummary(&AISummary{UserID: uid, Model: "m"})        // newest, still generating
	store.CreateAISummary(&AISummary{UserID: other, Model: "m"})                //nolint:errcheck // other user

	list, err := store.GetAISummaries(uid, 50)
	if err != nil {
		t.Fatalf("GetAISummaries: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 summaries for user, got %d", len(list))
	}
	// Newest first.
	if list[0].ID != id2 || list[1].ID != id1 {
		t.Fatalf("wrong order: %d, %d (want %d, %d)", list[0].ID, list[1].ID, id2, id1)
	}

	// GetAISummary is user-scoped and round-trips fields.
	got, err := store.GetAISummary(uid, id1)
	if err != nil || got == nil || got.Headline != "First" || len(got.ArticleIDs) != 2 {
		t.Fatalf("GetAISummary(%d) = %+v err=%v", id1, got, err)
	}
	if other2, _ := store.GetAISummary(other, id1); other2 != nil {
		t.Fatalf("GetAISummary must be user-scoped, leaked id1 to other user")
	}
}

func TestAISummaryNewsletterLink(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()
	uid, _ := store.CreateUser("u")
	nlID, err := store.CreateNewsletter(&Newsletter{UserID: uid, Name: "Cyber", Schedule: "manual"})
	if err != nil {
		t.Fatalf("CreateNewsletter: %v", err)
	}

	// One ad-hoc summary, one linked to the config.
	store.CreateAISummary(&AISummary{UserID: uid, Model: "m"}) //nolint:errcheck
	linkedID, _ := store.CreateAISummary(&AISummary{UserID: uid, NewsletterID: &nlID, Model: "m"})

	// Round-trips the link.
	got, _ := store.GetAISummary(uid, linkedID)
	if got == nil || got.NewsletterID == nil || *got.NewsletterID != nlID {
		t.Fatalf("newsletter_id not round-tripped: %+v", got)
	}
	// List-by-config returns only the linked one.
	forNl, err := store.GetAISummariesForNewsletter(uid, nlID, 50)
	if err != nil {
		t.Fatalf("GetAISummariesForNewsletter: %v", err)
	}
	if len(forNl) != 1 || forNl[0].ID != linkedID {
		t.Fatalf("expected only the linked summary, got %d rows", len(forNl))
	}
	// Full list still has both.
	if all, _ := store.GetAISummaries(uid, 50); len(all) != 2 {
		t.Fatalf("expected 2 summaries total, got %d", len(all))
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
