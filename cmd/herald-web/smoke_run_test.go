package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/infodancer/smoke"
	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
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
	dbPath := filepath.Join(t.TempDir(), "smoke.db")

	// herald-web runs read-only in production; mirror that. Incidental writes
	// (e.g. auto-mark-read) are best-effort and ignored by the handlers.
	engine, err := herald.NewEngine(herald.EngineConfig{DBPath: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	st, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
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

	// Confirm the deterministic ids the manifest examples assume.
	if user.ID != 1 || feedID != 1 || articleID != 1 || sumID != 1 {
		t.Fatalf("fixture ids not deterministic: user=%d feed=%d article=%d summary=%d (want all 1)",
			user.ID, feedID, articleID, sumID)
	}

	validator, token := newTestValidator(t)
	// adminRole "admin" with no role claim falls back to the adminUsers email
	// list, granting the smoke user admin so /admin/* returns 200.
	mux := newRouter(engine, validator, "admin", []string{"tester@example.com"})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	report, err := smoke.Run(context.Background(), mux.Registry().Manifest(), smoke.RunOptions{
		BaseURL: srv.URL,
		Target:  smoke.Preview,
		Cookie:  "test_jwt=" + token,
		// Writes are smoke.Write()-skipped (no payloads/CSRF wired yet); this is
		// the authed reads pass. Converting writes to real probes is follow-up.
		IncludeWrites: false,
	})
	if err != nil {
		t.Fatalf("smoke.Run: %v", err)
	}

	pass, fail, skip := report.Counts()
	t.Logf("smoke authed run: %d pass, %d fail, %d skip (of %d routes)", pass, fail, skip, len(mux.Registry().Manifest().Routes))
	if pass == 0 {
		t.Fatalf("no routes passed -- the harness likely isn't reaching the server")
	}
	for _, res := range report.Failed() {
		t.Errorf("FAIL %s %s -> status %d: %s", res.Method, res.Pattern, res.Status, res.Reason)
	}
}
