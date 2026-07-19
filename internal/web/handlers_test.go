package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/infodancer/oidclient"
	"github.com/infodancer/oidclient/session"
	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/storage"
	"github.com/matthewjhunter/herald/internal/storagetest"
	"github.com/matthewjhunter/herald/internal/urlnorm"
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
	// Pin a deterministic session encryption key so the Manager built inside
	// NewRouter and the helpers that pre-seed session rows (createTestSession)
	// seal and open tokens under the same keyring.
	os.Setenv("HERALD_SESSION_ENC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

// testSessionKeyring builds the keyring from the pinned test key, matching the
// one NewRouter constructs, so pre-seeded session tokens decrypt in the Manager.
func testSessionKeyring(t *testing.T) *session.Keyring {
	t.Helper()
	kr, err := newSessionKeyring(os.Getenv("HERALD_SESSION_ENC_KEY"))
	if err != nil {
		t.Fatalf("newSessionKeyring: %v", err)
	}
	return kr
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
	client, issueToken := newTestValidatorIssuer(t)
	return client, issueToken("test-sub-1", "tester@example.com", "Tester")
}

// newTestValidatorIssuer is like newTestValidator but returns the token
// minting function so tests can authenticate as additional users.
func newTestValidatorIssuer(t *testing.T) (*oidclient.Client, func(sub, email, name string) string) {
	t.Helper()
	srv, issueToken := fakeOIDCProvider(t, nil)

	client, err := oidclient.New(context.Background(), oidclient.Config{
		IssuerURL:  srv.URL,
		CookieName: "test_jwt",
	})
	if err != nil {
		t.Fatalf("oidclient.New: %v", err)
	}
	return client, issueToken
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
	router     http.Handler
	engine     *herald.Engine
	store      storage.Store
	userID     int64
	feedID     int64
	articleID  int64
	jwtToken   string                               // valid access-token JWT for the test user
	sessionID  string                               // opaque session-id cookie for the test user (#173)
	issueToken func(sub, email, name string) string // mints JWTs for additional users
}

// createTestSession persists a server-side session whose access token is the
// given JWT and returns the opaque session id to send as the cookie -- the same
// shape the OIDC callback produces, so requireAuth's validate path runs exactly
// as in production. The session's user_sub is read from the token's sub claim.
func createTestSession(t *testing.T, engine *herald.Engine, accessToken string) string {
	t.Helper()
	var idBytes [32]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		t.Fatalf("session id: %v", err)
	}
	id := hex.EncodeToString(idBytes[:])
	kr := testSessionKeyring(t)
	sealTok := func(tok string) []byte {
		b, err := kr.Seal([]byte(tok), []byte(id))
		if err != nil {
			t.Fatalf("seal token: %v", err)
		}
		return b
	}
	now := time.Now()
	if err := engine.CreateSession(&storage.Session{
		ID:             id,
		UserSub:        tokenSub(t, accessToken),
		AccessToken:    sealTok(accessToken),
		RefreshToken:   sealTok("test-refresh-" + id),
		AccessExpiry:   now.Add(time.Hour),
		AbsoluteExpiry: now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return id
}

// tokenSub extracts the sub claim from a JWT without verifying it -- used only
// to label the test session row, not for any auth decision.
func tokenSub(t *testing.T, token string) string {
	t.Helper()
	parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse test token: %v", err)
	}
	sub, _ := parsed.Claims.(jwt.MapClaims)["sub"].(string)
	return sub
}

func newTestFixtures(t *testing.T) *testFixtures {
	t.Helper()
	return newTestFixturesWith(t, nil)
}

// newTestFixturesWith builds the standard fixtures, letting the caller tweak
// the engine config (e.g. point SummaryBaseURL at a fake cloud gateway).
func newTestFixturesWith(t *testing.T, mutate func(*herald.EngineConfig)) *testFixtures {
	t.Helper()
	dbPath, dropSchema := storagetest.DSN(t)
	t.Cleanup(dropSchema)

	engCfg := herald.EngineConfig{
		DBPath:   dbPath,
		ReadOnly: true,
	}
	if mutate != nil {
		mutate(&engCfg)
	}
	engine, err := herald.NewEngine(engCfg)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	st, err := storage.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
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

	validator, issueToken := newTestValidatorIssuer(t)
	jwtToken := issueToken("test-sub-1", "tester@example.com", "Tester")
	router := NewRouter(engine, validator, "", nil, AnalyticsConfig{})

	t.Cleanup(func() {
		engine.Close()
		st.Close()
	})

	return &testFixtures{
		router:     router,
		engine:     engine,
		store:      st,
		userID:     user.ID,
		feedID:     feedID,
		articleID:  articleID,
		jwtToken:   jwtToken,
		sessionID:  createTestSession(t, engine, jwtToken),
		issueToken: issueToken,
	}
}

// secondTestUser provisions a second OIDC user and returns its ID and a JWT
// minted for it, for cross-user authorization tests.
func secondTestUser(t *testing.T, tf *testFixtures) (int64, string) {
	t.Helper()
	u, err := tf.engine.GetOrProvisionOIDCUser("test-sub-2", "Other", "other@example.com")
	if err != nil {
		t.Fatalf("GetOrProvisionOIDCUser other: %v", err)
	}
	return u.ID, tf.issueToken("test-sub-2", "other@example.com", "Other")
}

// authedRequestAs makes a test HTTP request authenticated as the user the given
// JWT identifies. It stands up a server-side session for that token so the
// request carries the opaque session cookie requireAuth now expects.
func authedRequestAs(t *testing.T, tf *testFixtures, token, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: createTestSession(t, tf.engine, token)})
	rr := httptest.NewRecorder()
	tf.router.ServeHTTP(rr, req)
	return rr
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
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: tf.sessionID})
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
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: tf.sessionID})
	rr := httptest.NewRecorder()
	tf.router.ServeHTTP(rr, req)
	return rr
}

// --- Auth tests ---

func TestHandleRoot_UnauthenticatedServesLanding(t *testing.T) {
	tf := newTestFixtures(t)

	// No JWT cookie → the public landing page, served in place (not a redirect to
	// the IdP). This is the "accessible without logging in" guarantee.
	rr := request(t, tf.router, "GET", "/", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d (landing must not redirect anonymous visitors)", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Read the news without reading the attacks") {
		t.Error("landing page should contain the hero headline")
	}
	if !strings.Contains(body, `href="/login"`) {
		t.Error("landing page should link to the /login sign-in CTA")
	}
	// The public layout must not carry app chrome (the reader's search box posts
	// to an authenticated route); its absence proves base_public.html was used.
	if strings.Contains(body, "nav-search") {
		t.Error("landing page should not render the app search box")
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
	// The shared nav must expose a way to end the session (#173 made logout
	// revoke the server-side session; without this link it is unreachable).
	if !strings.Contains(body, `href="/auth/logout"`) {
		t.Error("home page nav should contain a log out link")
	}
}

func TestHandleLogin_RedirectsToIdP(t *testing.T) {
	tf := newTestFixtures(t)

	// The landing-page CTA initiates the OIDC flow: /login always redirects to
	// the IdP (the redirect that used to live on an unauthenticated "/").
	rr := request(t, tf.router, "GET", "/login", nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusFound)
	}
	if loc := rr.Header().Get("Location"); loc == "" {
		t.Error("expected a Location header pointing at the IdP login URL")
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

func TestHandleArticleList_InlineAISummary(t *testing.T) {
	tf := newTestFixtures(t)

	const summary = "Batch-fetched inline summary for the list."
	if err := tf.store.UpdateArticleAISummary(tf.articleID, summary); err != nil {
		t.Fatalf("UpdateArticleAISummary: %v", err)
	}

	rr := authedRequest(t, tf, "GET", "/articles", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, summary) {
		t.Error("article list should render the inline AI summary text")
	}
	if !strings.Contains(body, "ai-summary-inline") {
		t.Error("inline summary should use the .ai-summary-inline class")
	}
}

func TestHandleArticleList_NoSummaryNoInlineBlock(t *testing.T) {
	tf := newTestFixtures(t)

	// The seeded article has no AI summary, so the inline block must be absent.
	rr := authedRequest(t, tf, "GET", "/articles", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if strings.Contains(rr.Body.String(), "ai-summary-inline") {
		t.Error("article without a summary should not render an inline summary block")
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

func TestHandleSearch_PastedURLFindsLinkers(t *testing.T) {
	tf := newTestFixtures(t)

	// A link-blog post linking to an external URL that is NOT itself an article
	// in Herald -- the user pastes that URL into search to find who linked it.
	const target = "https://hollymathnerd.substack.com/p/the-government-sets-the-trap"
	linkFeed, err := tf.store.AddFeed("https://instapundit.example/feed", "Instapundit", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := tf.store.SubscribeUserToFeed(tf.userID, linkFeed); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}
	postID, err := tf.store.AddArticle(&storage.Article{
		FeedID: linkFeed, GUID: "ip1", Title: "Heh. Indeed.", URL: "https://instapundit.example/p/1",
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}
	// The stored outbound link carries Substack's session params; the search
	// target is clean -- both normalize to the same key.
	if err := tf.store.StoreArticleLinks(postID, []string{urlnorm.Normalize(target + "?r=wm1qp&triedRedirect=true")}); err != nil {
		t.Fatalf("StoreArticleLinks: %v", err)
	}

	rr := authedRequest(t, tf, "GET", "/search?q="+url.QueryEscape(target), map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Feeds that linked to") {
		t.Error("pasting a URL should switch search to linked-by mode")
	}
	if !strings.Contains(body, "Instapundit") {
		t.Error("linked-by results should include the linking feed (normalized past tracking params)")
	}
	if !strings.Contains(body, "/articles/"+itoa(postID)) {
		t.Error("result should link to the linking post")
	}
}

func TestHandleSearch_BareDomainFindsAllLinks(t *testing.T) {
	tf := newTestFixtures(t)

	linkFeed, err := tf.store.AddFeed("https://instapundit.example/feed", "Instapundit", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := tf.store.SubscribeUserToFeed(tf.userID, linkFeed); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}
	// Two posts linking different paths on the same domain.
	for i, path := range []string{"p/one", "p/two"} {
		id, err := tf.store.AddArticle(&storage.Article{
			FeedID: linkFeed, GUID: "ip" + itoa(int64(i)), Title: "post",
			URL: "https://instapundit.example/x" + itoa(int64(i)),
		})
		if err != nil {
			t.Fatalf("AddArticle: %v", err)
		}
		if err := tf.store.StoreArticleLinks(id, []string{urlnorm.Normalize("https://hollymathnerd.substack.com/" + path)}); err != nil {
			t.Fatalf("StoreArticleLinks: %v", err)
		}
	}

	// Typing just the domain (no scheme) finds all links under it.
	rr := authedRequest(t, tf, "GET", "/search?q="+url.QueryEscape("hollymathnerd.substack.com"), map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Feeds that linked to") {
		t.Error("a bare domain should trigger linked-by mode")
	}
	// Both linking posts should appear.
	if strings.Count(body, "/articles/") < 2 {
		t.Errorf("expected both domain links in results, body had %d /articles/ refs", strings.Count(body, "/articles/"))
	}
}

func TestHandleArticleView_LinkedBy(t *testing.T) {
	tf := newTestFixtures(t)

	// A link-blog post in another subscribed feed that links to the fixture
	// article (URL https://example.com/article/1).
	linkFeed, err := tf.store.AddFeed("https://linkblog.example/feed", "Link Blog", "")
	if err != nil {
		t.Fatalf("AddFeed: %v", err)
	}
	if err := tf.store.SubscribeUserToFeed(tf.userID, linkFeed); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}
	postID, err := tf.store.AddArticle(&storage.Article{
		FeedID: linkFeed, GUID: "lb1", Title: "Worth a read",
		URL: "https://linkblog.example/p/1",
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}
	if err := tf.store.StoreArticleLinks(postID, []string{urlnorm.Normalize("https://example.com/article/1")}); err != nil {
		t.Fatalf("StoreArticleLinks: %v", err)
	}

	rr := authedRequest(t, tf, "GET", "/articles/"+itoa(tf.articleID), map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Linked by") {
		t.Error("article view should show the Linked by section")
	}
	if !strings.Contains(body, "Link Blog") {
		t.Error("Linked by section should name the linking feed")
	}
	// Clicking a backlink opens that post in-app.
	if !strings.Contains(body, "/articles/"+itoa(postID)) {
		t.Error("Linked by entry should link to the linking post's article view")
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

func TestHandleReadToggle(t *testing.T) {
	tf := newTestFixtures(t)

	path := "/articles/" + itoa(tf.articleID) + "/read"

	// Mark unread: button should now offer to mark read again, and the
	// article should reappear in the user's unread list.
	rr := authedRequestForm(t, tf, "POST", path, url.Values{"read": {"false"}})
	if rr.Code != http.StatusOK {
		t.Errorf("mark-unread status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "Mark read") {
		t.Errorf("response should offer to mark read, got %q", rr.Body.String())
	}
	unread, err := tf.engine.GetUnreadArticles(tf.userID, 10, 0, false)
	if err != nil {
		t.Fatalf("GetUnreadArticles: %v", err)
	}
	if len(unread) != 1 || unread[0].ID != tf.articleID {
		t.Errorf("expected article %d back in unread list, got %d articles", tf.articleID, len(unread))
	}

	// Mark read again: button offers to mark unread, article leaves the list.
	rr = authedRequestForm(t, tf, "POST", path, url.Values{"read": {"true"}})
	if rr.Code != http.StatusOK {
		t.Errorf("mark-read status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "Mark unread") {
		t.Errorf("response should offer to mark unread, got %q", rr.Body.String())
	}
	unread, _ = tf.engine.GetUnreadArticles(tf.userID, 10, 0, false)
	if len(unread) != 0 {
		t.Errorf("expected no unread articles after marking read, got %d", len(unread))
	}
	// But it is returned when read articles are included, flagged read.
	all, _ := tf.engine.GetUnreadArticles(tf.userID, 10, 0, true)
	if len(all) != 1 || !all[0].Read {
		t.Errorf("expected 1 read article with includeRead, got %d", len(all))
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

// TestHandleSidebar_ReaderGauge guards against the gauge disappearing when the
// sidebar is refetched via the feeds-changed htmx trigger (e.g. after opening
// an article), which hits this endpoint directly rather than riding along on
// an OOB swap from another handler.
func TestHandleSidebar_ReaderGauge(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "GET", "/sidebar", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "reader-gauge") {
		t.Error("sidebar refresh should include the reader status gauge")
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
	// The column reflects what it actually measures (no summary), not "Unscored".
	if !strings.Contains(body, "Unsummarized") {
		t.Error("feeds page should label the column Unsummarized")
	}
	if strings.Contains(body, ">Unscored<") {
		t.Error("feeds page should no longer use the misleading Unscored header")
	}
	// type="url" triggers browser validation that rejects scheme-less input
	// like "example.com/feed.xml" before the server can add the prefix.
	if strings.Contains(body, `type="url"`) {
		t.Error(`subscribe form must not use type="url"; the server handles scheme-less URLs`)
	}
	if !strings.Contains(body, `inputmode="url"`) {
		t.Error(`subscribe URL input should keep inputmode="url" for mobile keyboards`)
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

func TestBestDate(t *testing.T) {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	fetched := now.Add(-10 * time.Minute)
	withTime := time.Date(now.Year(), now.Month(), now.Day(), 14, 30, 0, 0, time.UTC)

	tests := []struct {
		name      string
		published *time.Time
		fetched   *time.Time
		want      *time.Time
	}{
		{"both nil", nil, nil, nil},
		{"published only", &withTime, nil, &withTime},
		{"fetched only", nil, &fetched, &fetched},
		{"midnight published uses fetched", &midnight, &fetched, &fetched},
		{"non-midnight published stays", &withTime, &fetched, &withTime},
		{"midnight but fetched before published", &midnight, &midnight, &midnight},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bestDate(tt.published, tt.fetched)
			if got != tt.want {
				t.Errorf("bestDate: got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseIntParam(t *testing.T) {
	req := httptest.NewRequest("GET", "/?limit=25&bad=abc&neg=-5&huge=9999", nil)

	// Basic cases (no cap)
	if v := parseIntParam(req, "limit", 10, 0); v != 25 {
		t.Errorf("limit: got %d, want 25", v)
	}
	if v := parseIntParam(req, "missing", 10, 0); v != 10 {
		t.Errorf("missing: got %d, want 10", v)
	}
	if v := parseIntParam(req, "bad", 10, 0); v != 10 {
		t.Errorf("bad: got %d, want 10", v)
	}
	if v := parseIntParam(req, "neg", 10, 0); v != 10 {
		t.Errorf("neg: got %d, want 10", v)
	}

	// Cap cases
	if v := parseIntParam(req, "huge", 10, 100); v != 100 {
		t.Errorf("over-max: got %d, want 100 (capped)", v)
	}
	if v := parseIntParam(req, "limit", 10, 50); v != 25 {
		t.Errorf("under-max: got %d, want 25", v)
	}
	if v := parseIntParam(req, "missing", 10, 5); v != 10 {
		t.Errorf("missing with cap: got %d, want 10 (default)", v)
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

func TestHandleCallback_SetsSessionCookie(t *testing.T) {
	tf := newTestFixtures(t)

	validator := newTestValidatorWithOIDC(t, nil)
	router := NewRouter(tf.engine, validator, "", nil, AnalyticsConfig{})

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
	router := NewRouter(tf.engine, validator, "", nil, AnalyticsConfig{})

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
	router := NewRouter(tf.engine, validator, "", nil, AnalyticsConfig{})

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
	router := NewRouter(tf.engine, validator, "", nil, AnalyticsConfig{})

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
	router := NewRouter(tf.engine, validator, "", nil, AnalyticsConfig{})

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
	router := NewRouter(tf.engine, validator, "", nil, AnalyticsConfig{})

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

func TestHandleProcessingStatus(t *testing.T) {
	tf := newTestFixtures(t)

	rr := authedRequest(t, tf, "GET", "/status", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	for _, want := range []string{"Processing Status", "Pending backlog", "Pipeline", "Summarized"} {
		if !strings.Contains(body, want) {
			t.Errorf("status page missing %q", want)
		}
	}
}

// --- Cross-user ownership tests ---

func TestHandleFilterDelete_CrossUser(t *testing.T) {
	tf := newTestFixtures(t)

	ruleID, err := tf.store.AddFilterRule(&storage.FilterRule{
		UserID: tf.userID, Axis: "author", Value: "Alice", Score: 5,
	})
	if err != nil {
		t.Fatalf("AddFilterRule: %v", err)
	}
	_, otherToken := secondTestUser(t, tf)

	// Another user cannot delete the rule.
	rr := authedRequestAs(t, tf, otherToken, "DELETE", "/filters/"+itoa(ruleID))
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-user delete: got %d, want %d", rr.Code, http.StatusNotFound)
	}
	rules, err := tf.engine.GetFilterRules(tf.userID, nil)
	if err != nil {
		t.Fatalf("GetFilterRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rule should survive cross-user delete, got %d rules", len(rules))
	}

	// The owner can.
	rr = authedRequest(t, tf, "DELETE", "/filters/"+itoa(ruleID), nil)
	if rr.Code != http.StatusOK {
		t.Errorf("owner delete: got %d, want %d", rr.Code, http.StatusOK)
	}
	rules, _ = tf.engine.GetFilterRules(tf.userID, nil)
	if len(rules) != 0 {
		t.Errorf("expected 0 rules after owner delete, got %d", len(rules))
	}
}

func TestHandleNewsletterGenerate_CrossUser(t *testing.T) {
	tf := newTestFixtures(t)

	nlID, err := tf.store.CreateNewsletter(&storage.Newsletter{
		UserID: tf.userID, Name: "Mine", Schedule: "manual",
		Config: storage.NewsletterConfig{MaxArticles: 10},
	})
	if err != nil {
		t.Fatalf("CreateNewsletter: %v", err)
	}
	otherID, otherToken := secondTestUser(t, tf)

	rr := authedRequestAs(t, tf, otherToken, "POST", "/newsletters/"+itoa(nlID)+"/generate")
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-user generate: got %d, want %d", rr.Code, http.StatusNotFound)
	}

	// The victim's newsletter is untouched and no digest row was created for
	// either user.
	nl, err := tf.store.GetNewsletter(nlID)
	if err != nil {
		t.Fatalf("GetNewsletter: %v", err)
	}
	if nl.LastGeneratedAt != nil {
		t.Errorf("last_generated_at advanced by cross-user generate: %v", nl.LastGeneratedAt)
	}
	for _, uid := range []int64{tf.userID, otherID} {
		if s, _ := tf.store.GetLatestAISummary(uid); s != nil {
			t.Errorf("unexpected ai_summaries row for user %d: %+v", uid, s)
		}
	}
}

func TestHandleNewsletterGenerate_Owner(t *testing.T) {
	// Fake cloud gateway so AISummaryEnabled() is true and the background
	// FinishAISummary finishes quickly against a live endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		delta, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"delta": map[string]any{
				"content": `{"headline":"H","body":"<p>b</p>"}`,
			}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", delta)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	tf := newTestFixturesWith(t, func(cfg *herald.EngineConfig) {
		cfg.SummaryBaseURL = srv.URL
	})

	nlID, err := tf.store.CreateNewsletter(&storage.Newsletter{
		UserID: tf.userID, Name: "Mine", Schedule: "manual",
		Config: storage.NewsletterConfig{MaxArticles: 10},
	})
	if err != nil {
		t.Fatalf("CreateNewsletter: %v", err)
	}

	rr := authedRequest(t, tf, "POST", "/newsletters/"+itoa(nlID)+"/generate", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner generate: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "Generating") {
		t.Errorf("expected Generating fragment, got %q", rr.Body.String())
	}

	// Wait for the background FinishAISummary goroutine to settle before the
	// fixtures tear down (avoids racing engine.Close).
	deadline := time.Now().Add(5 * time.Second)
	for {
		inprog, err := tf.store.GetInProgressAISummary(tf.userID)
		if err != nil {
			t.Fatalf("GetInProgressAISummary: %v", err)
		}
		if inprog == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("summary still in progress after 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- Subscription gating tests (#162) ---

func TestHandleArticleView_SubscriptionGated(t *testing.T) {
	tf := newTestFixtures(t)
	_, otherToken := secondTestUser(t, tf)

	path := "/articles/" + itoa(tf.articleID)

	// A non-subscriber cannot read the article.
	rr := authedRequestAs(t, tf, otherToken, "GET", path)
	if rr.Code != http.StatusNotFound {
		t.Errorf("non-subscriber view: got %d, want %d", rr.Code, http.StatusNotFound)
	}

	// The subscriber can.
	rr = authedRequest(t, tf, "GET", path, map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Errorf("subscriber view: got %d, want %d", rr.Code, http.StatusOK)
	}

	// Unknown IDs 404 for everyone.
	rr = authedRequest(t, tf, "GET", "/articles/999999", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown article: got %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestHandleArticleImage_SubscriptionGated(t *testing.T) {
	tf := newTestFixtures(t)
	_, otherToken := secondTestUser(t, tf)

	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	imageID, err := tf.store.StoreArticleImage(tf.articleID, "https://example.com/img.png", png, "image/png", 1, 1)
	if err != nil {
		t.Fatalf("StoreArticleImage: %v", err)
	}
	path := "/images/" + itoa(imageID)

	// A non-subscriber cannot fetch the image.
	rr := authedRequestAs(t, tf, otherToken, "GET", path)
	if rr.Code != http.StatusNotFound {
		t.Errorf("non-subscriber image: got %d, want %d", rr.Code, http.StatusNotFound)
	}

	// The subscriber gets the bytes with the stored MIME type.
	rr = authedRequest(t, tf, "GET", path, nil)
	if rr.Code != http.StatusOK {
		t.Errorf("subscriber image: got %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}

	// Unknown IDs 404 for everyone.
	rr = authedRequest(t, tf, "GET", "/images/999999", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown image: got %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestHandleStarToggle_SubscriptionGated(t *testing.T) {
	tf := newTestFixtures(t)
	otherID, otherToken := secondTestUser(t, tf)

	path := "/articles/" + itoa(tf.articleID) + "/star"

	// A non-subscriber cannot star the article (no form body: the handler
	// defaults to starring, and the engine rejects before any write).
	rr := authedRequestAs(t, tf, otherToken, "POST", path)
	if rr.Code == http.StatusOK {
		t.Errorf("non-subscriber star: got %d, want an error status", rr.Code)
	}

	// Prove no starred row was written: subscribe B afterwards (which would
	// make any starred row visible) and check the starred list is empty.
	if err := tf.store.SubscribeUserToFeed(otherID, tf.feedID); err != nil {
		t.Fatalf("SubscribeUserToFeed: %v", err)
	}
	starred, err := tf.store.GetStarredArticles(otherID, 10, 0, nil)
	if err != nil {
		t.Fatalf("GetStarredArticles: %v", err)
	}
	if len(starred) != 0 {
		t.Errorf("rejected star must not write a row, got %d starred", len(starred))
	}

	// The subscriber can star.
	rr = authedRequestForm(t, tf, "POST", path, url.Values{"starred": {"true"}})
	if rr.Code != http.StatusOK {
		t.Errorf("subscriber star: got %d, want %d", rr.Code, http.StatusOK)
	}
	starred, err = tf.store.GetStarredArticles(tf.userID, 10, 0, nil)
	if err != nil {
		t.Fatalf("GetStarredArticles A: %v", err)
	}
	if len(starred) != 1 || starred[0].ID != tf.articleID {
		t.Errorf("expected article %d starred for the subscriber, got %v", tf.articleID, starred)
	}
}

// --- Plan 007: input validation and security headers ---

// TestPromptSaveLengthCap asserts that a prompt template exceeding maxPromptLen
// gets a 400 and is not persisted.
func TestPromptSaveLengthCap(t *testing.T) {
	tf := newTestFixtures(t)

	// "curation" is a valid user-settable prompt type.
	overlong := strings.Repeat("x", maxPromptLen+1)
	rr := authedRequestForm(t, tf, "POST", "/settings/prompts/curation", url.Values{
		"template": {overlong},
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("overlong prompt: got %d, want %d", rr.Code, http.StatusBadRequest)
	}

	// A prompt at exactly the limit must be accepted.
	atLimit := strings.Repeat("x", maxPromptLen)
	rr = authedRequestForm(t, tf, "POST", "/settings/prompts/curation", url.Values{
		"template": {atLimit},
	})
	if rr.Code != http.StatusOK {
		t.Errorf("at-limit prompt: got %d, want %d", rr.Code, http.StatusOK)
	}
}

// TestSubscribeGenericError asserts that a feed-subscribe failure returns 400
// and does not expose raw error detail (dial strings, DNS, etc.) to the client.
func TestSubscribeGenericError(t *testing.T) {
	tf := newTestFixtures(t)

	// 127.0.0.1:1 is an unreachable loopback address that the SSRF dial guard
	// or TCP stack will reject immediately. We don't care which layer rejects
	// it -- we just want the error to be generic in the response body.
	// Route is POST /feeds (not /feeds/subscribe).
	rr := authedRequestForm(t, tf, "POST", "/feeds", url.Values{
		"url": {"http://127.0.0.1:1/feed.xml"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("failed subscribe: got %d, want 400", rr.Code)
	}
	body := rr.Body.String()
	for _, leak := range []string{"dial", "lookup", "connection refused", "connect:", "127.0.0.1"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("response body leaks internal detail %q: %s", leak, body)
		}
	}
}

// TestSecurityHeaders asserts that every authenticated response carries the
// required security headers when the securityHeaders middleware is applied.
func TestSecurityHeaders(t *testing.T) {
	tf := newTestFixtures(t)

	// SecurityHeaders is wired in serve but not in NewRouter. Wrap manually
	// here to test the middleware in isolation.
	wrapped := SecurityHeaders(tf.router)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: tf.sessionID})
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /: got %d, want 200", rr.Code)
	}

	want := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'",
	}
	for header, wantVal := range want {
		if got := rr.Header().Get(header); got != wantVal {
			t.Errorf("%s: got %q, want %q", header, got, wantVal)
		}
	}
}

// newAdminFixtures builds test fixtures where the test user has admin access.
func newAdminFixtures(t *testing.T) *testFixtures {
	t.Helper()
	dbPath, dropSchema := storagetest.DSN(t)
	t.Cleanup(dropSchema)

	engine, err := herald.NewEngine(herald.EngineConfig{DBPath: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	st, err := storage.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	user, err := engine.GetOrProvisionOIDCUser("test-sub-1", "Tester", "tester@example.com")
	if err != nil {
		t.Fatalf("GetOrProvisionOIDCUser: %v", err)
	}

	validator, issueToken := newTestValidatorIssuer(t)
	jwtToken := issueToken("test-sub-1", "tester@example.com", "Tester")
	// Grant admin by listing the test user's email in adminUsers.
	router := NewRouter(engine, validator, "", []string{"tester@example.com"}, AnalyticsConfig{})

	t.Cleanup(func() {
		engine.Close()
		st.Close()
	})

	return &testFixtures{
		router:     router,
		engine:     engine,
		store:      st,
		userID:     user.ID,
		jwtToken:   jwtToken,
		sessionID:  createTestSession(t, engine, jwtToken),
		issueToken: issueToken,
	}
}

// TestHandleAdminUsers_NonAdminForbidden confirms that a non-admin request to
// GET /admin/users gets 403.
func TestHandleAdminUsers_NonAdminForbidden(t *testing.T) {
	tf := newTestFixtures(t) // no admin email in router
	rr := authedRequest(t, tf, "GET", "/admin/users", nil)
	if rr.Code != http.StatusForbidden {
		t.Errorf("GET /admin/users non-admin: got %d, want 403", rr.Code)
	}
}

// TestHandleAdminUserDelete_NonAdminForbidden confirms that a non-admin DELETE
// is rejected with 403 and the user row is not removed.
func TestHandleAdminUserDelete_NonAdminForbidden(t *testing.T) {
	tf := newTestFixtures(t)

	// Provision a second user to try to delete.
	target, err := tf.engine.GetOrProvisionOIDCUser("target-sub", "Target", "target@example.com")
	if err != nil {
		t.Fatalf("GetOrProvisionOIDCUser target: %v", err)
	}

	rr := authedRequest(t, tf, "DELETE", "/admin/users/"+itoa(target.ID), nil)
	if rr.Code != http.StatusForbidden {
		t.Errorf("DELETE /admin/users/{id} non-admin: got %d, want 403", rr.Code)
	}

	// Confirm the user still exists.
	users, err := tf.engine.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	found := false
	for _, u := range users {
		if u.ID == target.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("target user should still exist after rejected non-admin delete")
	}
}

// TestHandleAdminUserDelete_AdminSuccess confirms that an admin can delete a
// non-reserved user and that the user is gone afterwards.
func TestHandleAdminUserDelete_AdminSuccess(t *testing.T) {
	tf := newAdminFixtures(t)

	// Provision a target user (will get id > 1 because tf.userID is 1).
	target, err := tf.engine.GetOrProvisionOIDCUser("target-sub", "Target", "target@example.com")
	if err != nil {
		t.Fatalf("GetOrProvisionOIDCUser target: %v", err)
	}
	if target.ID == tf.userID {
		t.Fatalf("test assumption broken: target and admin are the same user")
	}

	rr := authedRequest(t, tf, "DELETE", "/admin/users/"+itoa(target.ID), nil)
	// Expect a redirect (303) or 200 -- the handler sets HX-Redirect then 303.
	if rr.Code != http.StatusSeeOther && rr.Code != http.StatusOK {
		t.Errorf("DELETE /admin/users/{id} admin: got %d, want 303 or 200", rr.Code)
	}

	// Confirm the target user is gone.
	users, err := tf.engine.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	for _, u := range users {
		if u.ID == target.ID {
			t.Errorf("target user %d still exists after admin delete", target.ID)
		}
	}
}

// TestHandleAdminUserDelete_ReservedUserRejected confirms that deleting the
// default/reserved user returns 400.
func TestHandleAdminUserDelete_ReservedUserRejected(t *testing.T) {
	tf := newAdminFixtures(t)

	// tf.userID is user 1, which is the DefaultUserID (reserved).
	rr := authedRequest(t, tf, "DELETE", "/admin/users/"+itoa(tf.userID), nil)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("DELETE reserved user: got %d, want 400", rr.Code)
	}
}

// TestHandleHome_ReaderGauge checks the reader status gauge (#232) renders in the
// sidebar on the full page load.
func TestHandleHome_ReaderGauge(t *testing.T) {
	tf := newTestFixtures(t)
	rr := authedRequest(t, tf, "GET", "/", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "reader-gauge") {
		t.Error("home sidebar should render the reader status gauge")
	}
}

// TestHandleArticleList_ReaderGaugeOOB checks the gauge rides along on the OOB
// sidebar refresh so it stays in sync when the active view changes.
func TestHandleArticleList_ReaderGaugeOOB(t *testing.T) {
	tf := newTestFixtures(t)
	rr := authedRequest(t, tf, "GET", "/articles", map[string]string{"HX-Request": "true"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "reader-gauge") {
		t.Error("article-list OOB sidebar should include the reader gauge")
	}
}
