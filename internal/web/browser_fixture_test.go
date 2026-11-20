package web

import (
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/matthewjhunter/herald/internal/storagetest"
)

// TestServeFixtureForBrowser serves the smoke fixture on a real listener so a
// browser can crawl the assembled app: real router, real headers, a seeded
// database and a signed-in admin session. It exists to check pages against a
// Content-Security-Policy in a real browser, which no in-process test can.
//
// It does nothing unless HERALD_BROWSER_FIXTURE_ADDR is set (host:port to
// listen on). It writes the session cookie value to HERALD_BROWSER_FIXTURE_COOKIE
// (a file path), then serves until HERALD_BROWSER_FIXTURE_STOP (a file path)
// exists or 30 minutes pass. It needs HERALD_TEST_DB_DSN like every storage test.
//
//	HERALD_TEST_DB_DSN=... HERALD_BROWSER_FIXTURE_ADDR=127.0.0.1:18200 \
//	  HERALD_BROWSER_FIXTURE_COOKIE=/tmp/herald.cookie \
//	  HERALD_BROWSER_FIXTURE_STOP=/tmp/herald.stop \
//	  go test ./internal/web -run TestServeFixtureForBrowser -timeout 40m
//
// The cookie name is test_jwt. Serve it with the same wrapping production uses
// (SecurityHeaders around CSRFGuard around the router), so what the browser sees
// is what users get.
func TestServeFixtureForBrowser(t *testing.T) {
	addr := os.Getenv("HERALD_BROWSER_FIXTURE_ADDR")
	if addr == "" {
		t.Skip("HERALD_BROWSER_FIXTURE_ADDR is unset; this test only serves a fixture for a browser")
	}
	cookiePath := os.Getenv("HERALD_BROWSER_FIXTURE_COOKIE")
	stopPath := os.Getenv("HERALD_BROWSER_FIXTURE_STOP")
	if cookiePath == "" || stopPath == "" {
		t.Fatal("set HERALD_BROWSER_FIXTURE_COOKIE and HERALD_BROWSER_FIXTURE_STOP")
	}

	dbPath, dropSchema := storagetest.DSN(t)
	t.Cleanup(dropSchema)
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
	seedSmokeFixture(t, engine, st)

	validator, token := newTestValidator(t)
	mux := NewRouter(engine, validator, "", nil, "admin", []string{"tester@example.com"}, AnalyticsConfig{})
	cookie := createTestSession(t, engine, token)
	if err := os.WriteFile(cookiePath, []byte(cookie), 0o600); err != nil {
		t.Fatalf("write cookie file: %v", err)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	srv := &http.Server{Handler: SecurityHeaders(CSRFGuard(mux)), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Logf("serving the fixture on http://%s", addr)

	deadline := time.After(30 * time.Minute)
	for {
		if _, err := os.Stat(stopPath); err == nil {
			return
		}
		select {
		case <-deadline:
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
