package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/infodancer/oidclient"
	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
)

// testKey is generated once per test binary run.
var testKey *rsa.PrivateKey

const testKID = "herald-test-kid"

func init() {
	var err error
	testKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("failed to generate test RSA key: " + err.Error())
	}
}

// fakeOIDCProvider starts an httptest.Server that serves OIDC discovery, JWKS,
// and optionally a token endpoint. Returns the server and an issueToken function.
// If tokenHandler is non-nil it is registered at /token; otherwise no token
// endpoint is served (sufficient for validation-only tests).
func fakeOIDCProvider(t *testing.T, tokenHandler http.HandlerFunc) (srv *httptest.Server, issueToken func(sub, email, name string) string) {
	t.Helper()
	pub := &testKey.PublicKey

	var baseURL string

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"issuer":                                baseURL,
			"authorization_endpoint":                baseURL + "/authorize",
			"token_endpoint":                        baseURL + "/token",
			"jwks_uri":                              baseURL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc) //nolint:errcheck
	})

	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": testKID,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
				"alg": "RS256",
				"use": "sig",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc) //nolint:errcheck
	})

	if tokenHandler != nil {
		mux.HandleFunc("/token", tokenHandler)
	}

	srv = httptest.NewServer(mux)
	baseURL = srv.URL
	t.Cleanup(srv.Close)

	issueToken = func(sub, email, name string) string {
		now := time.Now()
		claims := jwt.MapClaims{
			"iss":   baseURL,
			"sub":   sub,
			"email": email,
			"name":  name,
			"iat":   now.Unix(),
			"exp":   now.Add(time.Hour).Unix(),
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = testKID
		signed, err := tok.SignedString(testKey)
		if err != nil {
			t.Fatalf("sign test token: %v", err)
		}
		return signed
	}

	return srv, issueToken
}

// defaultTokenHandler returns a token endpoint handler that issues valid access
// and ID tokens signed with testKey. The issuerURL pointer must point to a string
// that is populated with the server URL before any requests are served.
func defaultTokenHandler(issuerURL *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()

		accessClaims := jwt.MapClaims{
			"iss": *issuerURL, "sub": "test-sub-1",
			"email": "tester@example.com", "name": "Tester",
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		}
		accessTok := jwt.NewWithClaims(jwt.SigningMethodRS256, accessClaims)
		accessTok.Header["kid"] = testKID
		accessSigned, _ := accessTok.SignedString(testKey)

		idClaims := jwt.MapClaims{
			"iss": *issuerURL, "sub": "test-sub-1", "aud": "test-client",
			"email": "tester@example.com", "name": "Tester",
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		}
		idTok := jwt.NewWithClaims(jwt.SigningMethodRS256, idClaims)
		idTok.Header["kid"] = testKID
		idSigned, _ := idTok.SignedString(testKey)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"access_token": accessSigned,
			"id_token":     idSigned,
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}
}

// newTestValidator creates an oidclient.Client backed by a fake OIDC provider
// (no token endpoint). Returns the client and a valid JWT for the test user.
func newTestValidator(t *testing.T) (*oidclient.Client, string) {
	t.Helper()
	srv, issueToken := fakeOIDCProvider(t, nil)

	client, err := oidclient.New(context.Background(), oidclient.Config{
		IssuerURL:  srv.URL,
		CookieName: "test_jwt",
	})
	if err != nil {
		t.Fatalf("oidclient.New: %v", err)
	}

	token := issueToken("test-sub-1", "tester@example.com", "Tester")
	return client, token
}

// newTestValidatorWithOIDC creates an oidclient.Client with OIDC flow configured
// and a custom token endpoint handler. If tokenHandler is nil, a default handler
// that issues valid tokens is used.
func newTestValidatorWithOIDC(t *testing.T, tokenHandler http.HandlerFunc) *oidclient.Client {
	t.Helper()

	// The token handler needs the issuer URL, which is only known after the
	// server starts. Use a pointer that fakeOIDCProvider populates.
	var issuerURL string
	handler := tokenHandler
	if handler == nil {
		handler = defaultTokenHandler(&issuerURL)
	}

	srv, _ := fakeOIDCProvider(t, handler)
	issuerURL = srv.URL

	client, err := oidclient.New(context.Background(), oidclient.Config{
		IssuerURL:   srv.URL,
		CookieName:  "test_jwt",
		ClientID:    "test-client",
		CallbackURL: "https://herald.example.com/auth/callback",
	})
	if err != nil {
		t.Fatalf("oidclient.New: %v", err)
	}
	return client
}

// testFixtures holds all resources for a handler integration test.
type testFixtures struct {
	router    http.Handler
	engine    *herald.Engine
	store     *storage.SQLiteStore
	userID    int64
	feedID    int64
	articleID int64
	jwtToken  string // valid JWT for the test user
}

func newTestFixtures(t *testing.T) *testFixtures {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	engine, err := herald.NewEngine(herald.EngineConfig{
		DBPath:   dbPath,
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	st, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}

	// Provision the OIDC user that matches the test JWT sub claim.
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
		GUID:          "guid-1",
		Title:         "Test Article",
		URL:           "https://example.com/article/1",
		Content:       "<p>Hello, world!</p>",
		Summary:       "A test summary",
		Author:        "Test Author",
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	validator, jwtToken := newTestValidator(t)
	router := newRouter(engine, validator, "", nil)

	t.Cleanup(func() {
		engine.Close()
		st.Close()
	})

	return &testFixtures{
		router:    router,
		engine:    engine,
		store:     st,
		userID:    user.ID,
		feedID:    feedID,
		articleID: articleID,
		jwtToken:  jwtToken,
	}
}

// request makes a test HTTP request.
func request(t *testing.T, handler http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// authedRequest makes a test HTTP request with the test JWT cookie.
func authedRequest(t *testing.T, tf *testFixtures, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: tf.jwtToken})
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	tf.router.ServeHTTP(rr, req)
	return rr
}

func authedRequestForm(t *testing.T, tf *testFixtures, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	body := form.Encode()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: tf.jwtToken})
	rr := httptest.NewRecorder()
	tf.router.ServeHTTP(rr, req)
	return rr
}

// --- Auth tests ---

func TestHandleRoot_UnauthenticatedRedirectsToWebauth(t *testing.T) {
	tf := newTestFixtures(t)

	// No JWT cookie → should redirect to webauth login.
	rr := request(t, tf.router, "GET", "/", nil)
	if rr.Code != http.StatusFound {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusFound)
	}
	loc := rr.Header().Get("Location")
	if loc == "" {
		t.Error("expected Location header for unauthenticated redirect")
	}
}

func TestRequireAuth_HTMXUnauthenticatedUsesHXRedirect(t *testing.T) {
	tf := newTestFixtures(t)

	// HTMX partial request without auth → HX-Redirect header + 401, not a 302.
	rr := request(t, tf.router, "GET", "/articles", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	hxRedirect := rr.Header().Get("HX-Redirect")
	if hxRedirect == "" {
		t.Error("expected HX-Redirect header for HTMX unauthenticated request")
	}
	if loc := rr.Header().Get("Location"); loc != "" {
		t.Errorf("Location header should be empty for HTMX requests, got %q", loc)
	}
}

func TestHandleRoot_AuthenticatedServesHome(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "GET", "/", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "Test Feed") {
		t.Error("home page should contain feed title")
	}
}

func TestHandleLogout(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "GET", "/auth/logout", nil)
	if rr.Code != http.StatusFound {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusFound)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/logout") {
		t.Errorf("redirect %q should point to logout endpoint", loc)
	}
}

// --- Handler tests ---

func TestHandleHome(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "GET", "/", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test Feed") {
		t.Error("home page should contain feed title")
	}
}

func TestHandleHome_Unauthenticated(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/", nil)
	if rr.Code != http.StatusFound {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusFound)
	}
}

func TestHandleArticleList_Default(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "GET", "/articles", map[string]string{
		"HX-Request": "true",
	})
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "Test Article") {
		t.Error("article list should contain article title")
	}
}

func TestHandleArticleList_ByFeed(t *testing.T) {
	tf := newTestFixtures(t)

	path := "/articles?feed_id=" + itoa(tf.feedID)
	rr := authedRequest(t, tf, "GET", path, map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "Test Article") {
		t.Error("article list should contain article from the specified feed")
	}
}

func TestHandleArticleList_Starred_Empty(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "GET", "/articles?starred=1", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if strings.Contains(rr.Body.String(), "Test Article") {
		t.Error("starred list should be empty when nothing is starred")
	}
}

func TestHandleArticleView(t *testing.T) {
	tf := newTestFixtures(t)

	path := "/articles/" + itoa(tf.articleID)
	rr := authedRequest(t, tf, "GET", path, map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test Article") {
		t.Error("article view should contain title")
	}
	if !strings.Contains(body, "Hello, world!") {
		t.Error("article view should contain sanitized content")
	}
}

func TestHandleArticleView_SanitizesXSS(t *testing.T) {
	tf := newTestFixtures(t)

	pub := time.Now()
	id, err := tf.store.AddArticle(&storage.Article{
		FeedID:        tf.feedID,
		GUID:          "xss-test",
		Title:         "XSS Test",
		URL:           "https://example.com/xss",
		Content:       `<p>Safe</p><script>alert('xss')</script><img src=x onerror="alert(1)">`,
		PublishedDate: &pub,
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	path := "/articles/" + itoa(id)
	rr := authedRequest(t, tf, "GET", path, map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<script>") {
		t.Error("script tags should be stripped by bluemonday")
	}
	if strings.Contains(body, "onerror") {
		t.Error("event handlers should be stripped by bluemonday")
	}
	if !strings.Contains(body, "Safe") {
		t.Error("safe content should be preserved")
	}
}

func TestHandleArticleView_NotFound(t *testing.T) {
	tf := newTestFixtures(t)

	path := "/articles/99999"
	rr := authedRequest(t, tf, "GET", path, map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestHandleStarToggle(t *testing.T) {
	tf := newTestFixtures(t)

	path := "/articles/" + itoa(tf.articleID) + "/star"

	rr := authedRequestForm(t, tf, "POST", path, url.Values{"starred": {"true"}})
	if rr.Code != http.StatusOK {
		t.Errorf("star status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "Starred") {
		t.Error("response should contain starred state")
	}

	rr = authedRequestForm(t, tf, "POST", path, url.Values{"starred": {"false"}})
	if rr.Code != http.StatusOK {
		t.Errorf("unstar status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "Star") {
		t.Error("response should contain star button")
	}
}

func TestHandleSidebar(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "GET", "/sidebar", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "Test Feed") {
		t.Error("sidebar should contain feed title")
	}
}

func TestHandleFeedsManage(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "GET", "/feeds", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test Feed") {
		t.Error("feeds page should contain feed title")
	}
	if !strings.Contains(body, "example.com/feed") {
		t.Error("feeds page should contain feed URL")
	}
}

func TestHandleSettings(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "GET", "/settings", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
}

func TestHandleSettingsSave(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequestForm(t, tf, "POST", "/settings", url.Values{
		"keywords":           {"go, security, ai"},
		"interest_threshold": {"7.5"},
		"notify_when":        {"always"},
		"notify_min_score":   {"6.0"},
	})
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}

	prefs, err := tf.engine.GetPreferences(tf.userID)
	if err != nil {
		t.Fatalf("GetPreferences: %v", err)
	}
	if prefs.InterestThreshold != 7.5 {
		t.Errorf("interest_threshold: got %f, want 7.5", prefs.InterestThreshold)
	}
	if prefs.NotifyWhen != "always" {
		t.Errorf("notify_when: got %q, want always", prefs.NotifyWhen)
	}
}

func TestHandleOIDCUserProvisioning(t *testing.T) {
	tf := newTestFixtures(t)

	// Second login with same sub but different name/email should succeed
	// and return the same user (not duplicate).
	user2, err := tf.engine.GetOrProvisionOIDCUser("test-sub-1", "Updated Name", "new@example.com")
	if err != nil {
		t.Fatalf("GetOrProvisionOIDCUser: %v", err)
	}
	if user2.ID != tf.userID {
		t.Errorf("second login should return same user ID: got %d, want %d", user2.ID, tf.userID)
	}
}

func TestHandleOIDCUserProvisioning_NewUser(t *testing.T) {
	tf := newTestFixtures(t)

	// A completely new sub should create a new user.
	newUser, err := tf.engine.GetOrProvisionOIDCUser("brand-new-sub", "New Person", "new@example.com")
	if err != nil {
		t.Fatalf("GetOrProvisionOIDCUser: %v", err)
	}
	if newUser.ID == tf.userID {
		t.Error("new sub should create a different user")
	}
	if newUser.Name != "New Person" {
		t.Errorf("Name = %q, want %q", newUser.Name, "New Person")
	}
}

// --- Utility tests ---

func TestFormatDate(t *testing.T) {
	tests := []struct {
		name string
		time *time.Time
		want string
	}{
		{"nil", nil, ""},
		{"minutes ago", timePtr(time.Now().Add(-30 * time.Minute)), "30m ago"},
		{"hours ago", timePtr(time.Now().Add(-5 * time.Hour)), "5h ago"},
		{"days ago", timePtr(time.Now().Add(-3 * 24 * time.Hour)), "3d ago"},
		{"old date", timePtr(time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)), "Jan 15, 2024"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDate(tt.time)
			if got != tt.want {
				t.Errorf("formatDate: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseIntParam(t *testing.T) {
	req := httptest.NewRequest("GET", "/?limit=25&bad=abc&neg=-5", nil)

	if v := parseIntParam(req, "limit", 10); v != 25 {
		t.Errorf("limit: got %d, want 25", v)
	}
	if v := parseIntParam(req, "missing", 10); v != 10 {
		t.Errorf("missing: got %d, want 10", v)
	}
	if v := parseIntParam(req, "bad", 10); v != 10 {
		t.Errorf("bad: got %d, want 10", v)
	}
	if v := parseIntParam(req, "neg", 10); v != 10 {
		t.Errorf("neg: got %d, want 10", v)
	}
}

func TestStaticFilesServed(t *testing.T) {
	tf := newTestFixtures(t)

	rr := request(t, tf.router, "GET", "/static/htmx.min.js", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("htmx.min.js status: got %d, want %d", rr.Code, http.StatusOK)
	}

	rr = request(t, tf.router, "GET", "/static/herald.css", nil)
	if rr.Code != http.StatusOK {
		t.Errorf("herald.css status: got %d, want %d", rr.Code, http.StatusOK)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// itoa converts an int64 to a string path component.
func itoa(n int64) string {
	return url.PathEscape(strings.TrimSpace(strconv.FormatInt(n, 10)))
}

// --- Callback handler tests ---

func TestHandleCallback_SetsJWTCookie(t *testing.T) {
	tf := newTestFixtures(t)

	validator := newTestValidatorWithOIDC(t, nil)
	router := newRouter(tf.engine, validator, "", nil)

	state := "test-state-nonce"
	verifier := "test-pkce-verifier"
	req := httptest.NewRequest("GET", "/auth/callback?code=test-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oidclient.CookieState, Value: state})
	req.AddCookie(&http.Cookie{Name: oidclient.CookieVerifier, Value: verifier})
	req.AddCookie(&http.Cookie{Name: oidclient.CookieRedirect, Value: "/u/1"})

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusFound)
	}
	if loc := rr.Header().Get("Location"); loc != "/u/1" {
		t.Errorf("Location: got %q, want /u/1", loc)
	}

	var jwtCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "test_jwt" {
			jwtCookie = c
		}
	}
	if jwtCookie == nil || jwtCookie.Value == "" {
		t.Error("JWT cookie should be set after successful callback")
	}
	if jwtCookie != nil && !jwtCookie.HttpOnly {
		t.Error("JWT cookie must be HttpOnly")
	}
}

func TestHandleCallback_DefaultRedirect(t *testing.T) {
	tf := newTestFixtures(t)

	validator := newTestValidatorWithOIDC(t, nil)
	router := newRouter(tf.engine, validator, "", nil)

	state := "test-state"
	req := httptest.NewRequest("GET", "/auth/callback?code=test-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oidclient.CookieState, Value: state})
	req.AddCookie(&http.Cookie{Name: oidclient.CookieVerifier, Value: "verifier"})
	// No oauth_redirect cookie — should default to "/".

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusFound)
	}
	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Errorf("Location: got %q, want /", loc)
	}
}

func TestHandleCallback_InvalidState(t *testing.T) {
	tf := newTestFixtures(t)

	validator := newTestValidatorWithOIDC(t, nil)
	router := newRouter(tf.engine, validator, "", nil)

	req := httptest.NewRequest("GET", "/auth/callback?code=test-code&state=WRONG", nil)
	req.AddCookie(&http.Cookie{Name: oidclient.CookieState, Value: "correct-state"})
	req.AddCookie(&http.Cookie{Name: oidclient.CookieVerifier, Value: "verifier"})

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want %d (state mismatch should be 400)", rr.Code, http.StatusBadRequest)
	}
}

func TestHandleCallback_MissingVerifier(t *testing.T) {
	tf := newTestFixtures(t)

	validator := newTestValidatorWithOIDC(t, nil)
	router := newRouter(tf.engine, validator, "", nil)

	state := "test-state"
	req := httptest.NewRequest("GET", "/auth/callback?code=test-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oidclient.CookieState, Value: state})
	// oauth_verifier cookie omitted.

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want %d (missing verifier should be 400)", rr.Code, http.StatusBadRequest)
	}
}

func TestHandleCallback_TokenExchangeError(t *testing.T) {
	tf := newTestFixtures(t)

	// Token endpoint returns 401.
	validator := newTestValidatorWithOIDC(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid_grant", http.StatusUnauthorized)
	})
	router := newRouter(tf.engine, validator, "", nil)

	state := "test-state"
	req := httptest.NewRequest("GET", "/auth/callback?code=bad-code&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oidclient.CookieState, Value: state})
	req.AddCookie(&http.Cookie{Name: oidclient.CookieVerifier, Value: "verifier"})

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status: got %d, want %d (upstream failure should be 502)", rr.Code, http.StatusBadGateway)
	}
}

func TestHandleCallback_UpstreamAuthError(t *testing.T) {
	tf := newTestFixtures(t)

	validator := newTestValidatorWithOIDC(t, nil)
	router := newRouter(tf.engine, validator, "", nil)

	// Webauth redirects with ?error=access_denied when the user denies.
	req := httptest.NewRequest("GET", "/auth/callback?error=access_denied&error_description=User+denied+access", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d (upstream error param should be 401)", rr.Code, http.StatusUnauthorized)
	}
}

func TestHandleArticleList_ByGroup(t *testing.T) {
	tf := newTestFixtures(t)

	// Create a group and add the test article to it
	groupID, err := tf.store.CreateArticleGroup(tf.userID, "Test Group Topic")
	if err != nil {
		t.Fatalf("CreateArticleGroup: %v", err)
	}
	tf.store.UpdateGroupDisplayName(groupID, "Test Group")
	tf.store.AddArticleToGroup(groupID, tf.articleID)

	// Add a second article to the group (need 2 for it to show)
	pub := time.Now().Add(-30 * time.Minute)
	art2, _ := tf.store.AddArticle(&storage.Article{
		FeedID: tf.feedID, GUID: "guid-grp-2", Title: "Group Article 2",
		URL: "https://example.com/grp2", PublishedDate: &pub,
	})
	tf.store.AddArticleToGroup(groupID, art2)

	// Verify group articles are returned
	path := "/articles?group_id=" + itoa(groupID)
	rr := authedRequest(t, tf, "GET", path, map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test Article") {
		t.Error("group article list should contain article title")
	}
	if !strings.Contains(body, "Group Article 2") {
		t.Error("group article list should contain second article")
	}

	// Verify grouped articles are excluded from default article list
	rr = authedRequest(t, tf, "GET", "/articles", map[string]string{"HX-Request": "true"})
	if strings.Contains(rr.Body.String(), "Test Article") {
		t.Error("default article list should not contain grouped articles")
	}
}

func TestHandleGroupMute(t *testing.T) {
	tf := newTestFixtures(t)

	groupID, _ := tf.store.CreateArticleGroup(tf.userID, "Mute Test")
	tf.store.AddArticleToGroup(groupID, tf.articleID)

	// Add a second article
	pub := time.Now().Add(-30 * time.Minute)
	art2, _ := tf.store.AddArticle(&storage.Article{
		FeedID: tf.feedID, GUID: "guid-mute-2", Title: "Mute Article 2",
		URL: "https://example.com/mute2", PublishedDate: &pub,
	})
	tf.store.AddArticleToGroup(groupID, art2)

	path := "/groups/" + itoa(groupID) + "/mute"
	rr := authedRequest(t, tf, "POST", path, nil)
	if rr.Code != http.StatusNoContent {
		t.Errorf("mute status: got %d, want %d", rr.Code, http.StatusNoContent)
	}

	// Verify group is muted
	muted, _ := tf.store.IsGroupMuted(groupID)
	if !muted {
		t.Error("group should be muted after POST /groups/{id}/mute")
	}
}

func TestHandleGroupDisband(t *testing.T) {
	tf := newTestFixtures(t)

	groupID, _ := tf.store.CreateArticleGroup(tf.userID, "Disband Test")
	tf.store.AddArticleToGroup(groupID, tf.articleID)

	path := "/groups/" + itoa(groupID)
	rr := authedRequest(t, tf, "DELETE", path, nil)
	if rr.Code != http.StatusNoContent {
		t.Errorf("disband status: got %d, want %d", rr.Code, http.StatusNoContent)
	}

	// Group should be gone
	group, _ := tf.store.GetGroup(groupID)
	if group != nil {
		t.Error("group should be deleted after DELETE /groups/{id}")
	}
}
