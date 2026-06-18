package web

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/infodancer/smoke"
	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/matthewjhunter/herald/internal/storagetest"
)

// smokePNG is an arbitrary non-empty image blob. The image/favicon handlers
// write the bytes verbatim with the stored MIME type; they don't decode it, so
// the PNG signature is enough to exercise a 200 response.
var smokePNG = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

// TestSmokeRoutesAuthenticated is the phase-2a authed smoke harness: it boots
// the real wired router against a real (SQLite) store, seeds a deterministic
// fixture, mints a valid session cookie via the fake OIDC provider, and runs
// the smoke library over every route in the manifest with that cookie. Unlike
// the per-handler tests (which exercise one handler each), this drives the
// fully assembled server end-to-end and fails on any 5xx/4xx -- the class of
// wiring/DI/route-registration regression that unit tests against fakes miss.
//
// It is in-process (no preview container yet): it catches handler/route/DB-
// wiring 502s, not deployment/migration/image regressions. Those are phase 2b.
//
// The fixture pins ids to 1 (each entity is the first row in a fresh DB), which
// the smoke.Example() annotations in routes.go reference. The smoke user is made
// admin (via the adminUsers email fallback) so /admin/* routes return 200 rather
// than 403.
func TestSmokeRoutesAuthenticated(t *testing.T) {
	dbPath, dropSchema := storagetest.DSN(t)
	t.Cleanup(dropSchema)

	// herald-web runs read-only in production; mirror that. Incidental writes
	// (e.g. auto-mark-read) are best-effort and ignored by the handlers.
	engine, err := herald.NewEngine(herald.EngineConfig{DBPath: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	st, err := storage.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Seed the fixture. newTestValidator issues a token for sub "test-sub-1" /
	// "tester@example.com"; provision the matching user so request-time
	// GetOrProvisionOIDCUser resolves to it and user-scoped routes (summary)
	// find their rows.
	user, err := engine.GetOrProvisionOIDCUser("test-sub-1", "Tester", "tester@example.com")
	if err != nil {
		t.Fatalf("GetOrProvisionOIDCUser: %v", err)
	}
	feedID, err := st.AddFeed("https://example.com/feed", "Test Feed", "A test feed")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := st.SubscribeUserToFeed(user.ID, feedID); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}
	pub := time.Now().Add(-time.Hour)
	articleID, err := st.AddArticle(&storage.Article{
		FeedID:        feedID,
		GUID:          "smoke-guid-1",
		Title:         "Smoke Article",
		URL:           "https://example.com/article/1",
		Content:       "<p>Hello, world!</p>",
		Summary:       "A test summary",
		Author:        "Test Author",
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}
	if _, err := st.StoreArticleImage(articleID, "https://example.com/img.png", smokePNG, "image/png", 1, 1); err != nil {
		t.Fatalf("StoreArticleImage: %v", err)
	}
	if err := st.StoreFeedFavicon(feedID, smokePNG, "image/png"); err != nil {
		t.Fatalf("StoreFeedFavicon: %v", err)
	}
	sumID, err := st.CreateAISummary(&storage.AISummary{UserID: user.ID, Model: "test-model", Prompt: "test prompt"})
	if err != nil {
		t.Fatalf("CreateAISummary: %v", err)
	}
	if err := st.UpdateAISummaryDone(sumID, "Smoke Headline", "<p>Summary body</p>", []int64{articleID}, 1, 1); err != nil {
		t.Fatalf("UpdateAISummaryDone: %v", err)
	}

	// Fixtures for the write probes. Destructive writes target a dedicated
	// second row so they don't delete what the read probes need.
	//   feed 1: tag "smoke-seed" (DELETE /feeds/1/tags removes it)
	//   feed 2: DELETE /feeds/2 unsubscribes it
	if err := st.AddFeedTag(user.ID, feedID, "smoke-seed"); err != nil {
		t.Fatalf("AddFeedTag: %v", err)
	}
	feed2, err := st.AddFeed("https://example.com/feed2", "Feed Two", "second feed")
	if err != nil {
		t.Fatalf("AddFeed feed2: %v", err)
	}
	if err := st.SubscribeUserToFeed(user.ID, feed2); err != nil {
		t.Fatalf("SubscribeUserToFeed feed2: %v", err)
	}
	ruleID, err := st.AddFilterRule(&storage.FilterRule{UserID: user.ID, Axis: "author", Value: "seed", Score: 50})
	if err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	group1, err := st.CreateArticleGroup(user.ID, "Smoke Group One")
	if err != nil {
		t.Fatalf("CreateArticleGroup g1: %v", err)
	}
	if err := st.AddArticleToGroup(group1, articleID); err != nil {
		t.Fatalf("AddArticleToGroup: %v", err)
	}
	group2, err := st.CreateArticleGroup(user.ID, "Smoke Group Two")
	if err != nil {
		t.Fatalf("CreateArticleGroup g2: %v", err)
	}
	nl1, err := st.CreateNewsletter(&storage.Newsletter{UserID: user.ID, Name: "NL One", Schedule: "daily", Config: storage.NewsletterConfig{MaxArticles: 10}})
	if err != nil {
		t.Fatalf("CreateNewsletter nl1: %v", err)
	}
	nl2, err := st.CreateNewsletter(&storage.Newsletter{UserID: user.ID, Name: "NL Two", Schedule: "daily", Config: storage.NewsletterConfig{MaxArticles: 10}})
	if err != nil {
		t.Fatalf("CreateNewsletter nl2: %v", err)
	}
	if err := st.SetFeverCredential(user.ID, "smoke-api-key"); err != nil {
		t.Fatalf("SetFeverCredential: %v", err)
	}
	// "curation" prompts for the reset probes: user-scoped and global (id 0).
	if err := st.SetUserPrompt(user.ID, "curation", "seed user prompt", nil, nil); err != nil {
		t.Fatalf("SetUserPrompt user: %v", err)
	}
	if err := st.SetUserPrompt(0, "curation", "seed global prompt", nil, nil); err != nil {
		t.Fatalf("SetUserPrompt global: %v", err)
	}

	// Confirm the deterministic ids the manifest examples assume.
	if user.ID != 1 || feedID != 1 || articleID != 1 || sumID != 1 ||
		feed2 != 2 || ruleID != 1 || group1 != 1 || group2 != 2 || nl1 != 1 || nl2 != 2 {
		t.Fatalf("fixture ids not deterministic: user=%d feed=%d feed2=%d article=%d summary=%d rule=%d group1=%d group2=%d nl1=%d nl2=%d",
			user.ID, feedID, feed2, articleID, sumID, ruleID, group1, group2, nl1, nl2)
	}

	validator, token := newTestValidator(t)
	// adminRole "admin" with no role claim falls back to the adminUsers email
	// list, granting the smoke user admin so /admin/* returns 200.
	mux := NewRouter(engine, validator, "admin", []string{"tester@example.com"})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	manifest := mux.Registry().Manifest()
	// requireAuth resolves the cookie to a server-side session, so the probe
	// must carry an opaque session id (not the raw JWT) keyed to a stored
	// session for the smoke user (#173).
	cookie := "test_jwt=" + createTestSession(t, engine, token)

	// Pass 1: reads, concurrently. Concurrency is the point -- it re-exercises
	// the path that surfaced the template-init race -- so leave it at the
	// default and don't probe writes here (mutating routes are skipped).
	reads, err := smoke.Run(ctx, manifest, smoke.RunOptions{
		BaseURL: srv.URL, Target: smoke.Preview, Cookie: cookie, IncludeWrites: false,
	})
	if err != nil {
		t.Fatalf("smoke.Run reads: %v", err)
	}

	// Pass 2: writes, serially. Restrict the manifest to mutating routes and run
	// at concurrency 1 so destructive probes can't race each other or a
	// dependent probe. The manifest is sorted (pattern, then method), so for a
	// shared entity a DELETE sorts before its POST -- e.g. DELETE then POST
	// /settings/fever both succeed.
	writeManifest := smoke.Manifest{}
	for _, r := range manifest.Routes {
		if r.Effect == "mutating" {
			writeManifest.Routes = append(writeManifest.Routes, r)
		}
	}
	writes, err := smoke.Run(ctx, writeManifest, smoke.RunOptions{
		BaseURL: srv.URL, Target: smoke.Preview, Cookie: cookie, IncludeWrites: true, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("smoke.Run writes: %v", err)
	}

	rp, rf, rs := reads.Counts()
	wp, wf, ws := writes.Counts()
	t.Logf("smoke authed: reads %d pass / %d fail / %d skip; writes %d pass / %d fail / %d skip", rp, rf, rs, wp, wf, ws)
	if rp == 0 || wp == 0 {
		t.Fatalf("no routes passed in a pass (reads pass=%d, writes pass=%d) -- harness likely not reaching the server", rp, wp)
	}
	for _, res := range append(reads.Failed(), writes.Failed()...) {
		t.Errorf("FAIL %s %s -> status %d: %s", res.Method, res.Pattern, res.Status, res.Reason)
	}
}
