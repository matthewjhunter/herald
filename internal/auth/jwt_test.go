package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testKey is generated once per test run.
var testKey *rsa.PrivateKey

func init() {
	var err error
	testKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("failed to generate test RSA key: " + err.Error())
	}
}

// newTestValidator creates a Validator wired to testKey via a test JWKS server.
func newTestValidator(t *testing.T, issuer string) (*Validator, *httptest.Server) {
	t.Helper()

	// Serve a minimal JWKS containing testKey.
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := testKey.N.Bytes()
		e := big.NewInt(int64(testKey.E)).Bytes()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"keys":[{"kty":"RSA","kid":"test","n":"%s","e":"%s"}]}`,
			base64.RawURLEncoding.EncodeToString(n),
			base64.RawURLEncoding.EncodeToString(e),
		)
	}))

	v, err := NewValidator(ValidatorConfig{
		Issuer:       issuer,
		CookieName:   "test_jwt",
		WebauthURL:   "https://auth.example.com",
		JWKSEndpoint: jwksSrv.URL,
	})
	if err != nil {
		jwksSrv.Close()
		t.Fatalf("NewValidator: %v", err)
	}
	return v, jwksSrv
}

// makeToken creates a signed JWT for testing.
func makeToken(t *testing.T, sub, email, name, issuer string, expiry time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":   sub,
		"email": email,
		"name":  name,
		"iss":   issuer,
		"exp":   expiry.Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "test"
	signed, err := tok.SignedString(testKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func TestValidate_ValidToken(t *testing.T) {
	v, srv := newTestValidator(t, "https://auth.example.com")
	defer srv.Close()

	token := makeToken(t, "user-sub-123", "user@example.com", "Test User",
		"https://auth.example.com", time.Now().Add(time.Hour))

	claims, err := v.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.Sub != "user-sub-123" {
		t.Errorf("Sub = %q, want %q", claims.Sub, "user-sub-123")
	}
	if claims.Email != "user@example.com" {
		t.Errorf("Email = %q, want %q", claims.Email, "user@example.com")
	}
	if claims.Name != "Test User" {
		t.Errorf("Name = %q, want %q", claims.Name, "Test User")
	}
}

func TestValidate_ExpiredToken(t *testing.T) {
	v, srv := newTestValidator(t, "")
	defer srv.Close()

	token := makeToken(t, "user-sub", "", "", "", time.Now().Add(-time.Hour))

	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

func TestValidate_WrongIssuer(t *testing.T) {
	v, srv := newTestValidator(t, "https://expected-issuer.com")
	defer srv.Close()

	token := makeToken(t, "sub", "", "", "https://wrong-issuer.com", time.Now().Add(time.Hour))

	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for wrong issuer, got nil")
	}
}

func TestValidate_TamperedToken(t *testing.T) {
	v, srv := newTestValidator(t, "")
	defer srv.Close()

	token := makeToken(t, "sub", "", "", "", time.Now().Add(time.Hour))
	// Tamper with the signature by modifying the first character of the third
	// dot-separated segment. The first character of any base64url block is always
	// a full 6-bit value (never a padding-only position), so this reliably changes
	// the decoded signature bytes regardless of key size.
	parts := strings.SplitN(token, ".", 3)
	sig := []byte(parts[2])
	if sig[0] != 'A' {
		sig[0] = 'A'
	} else {
		sig[0] = 'B'
	}
	tampered := parts[0] + "." + parts[1] + "." + string(sig)

	_, err := v.Validate(tampered)
	if err == nil {
		t.Fatal("expected error for tampered token, got nil")
	}
}

func TestValidate_MissingSub(t *testing.T) {
	v, srv := newTestValidator(t, "")
	defer srv.Close()

	// Build a token with no sub claim.
	claims := jwt.MapClaims{
		"email": "user@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "test"
	signed, _ := tok.SignedString(testKey)

	_, err := v.Validate(signed)
	if err == nil {
		t.Fatal("expected error for missing sub, got nil")
	}
}

func TestValidateCookie(t *testing.T) {
	v, srv := newTestValidator(t, "")
	defer srv.Close()

	token := makeToken(t, "sub-abc", "a@b.com", "Alice", "", time.Now().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "test_jwt", Value: token})

	claims, err := v.ValidateCookie(req)
	if err != nil {
		t.Fatalf("ValidateCookie: %v", err)
	}
	if claims.Sub != "sub-abc" {
		t.Errorf("Sub = %q, want sub-abc", claims.Sub)
	}
}

func TestValidateCookie_Missing(t *testing.T) {
	v, srv := newTestValidator(t, "")
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := v.ValidateCookie(req)
	if err == nil {
		t.Fatal("expected error when cookie absent, got nil")
	}
}

func TestWebauthLoginURL(t *testing.T) {
	v := &Validator{cfg: ValidatorConfig{WebauthURL: "https://auth.example.com"}}

	u := v.WebauthLoginURL("/u/5/feeds")
	if u == "" {
		t.Fatal("empty login URL")
	}
	// Should contain redirect_uri parameter.
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if parsed.Query().Get("redirect_uri") != "/u/5/feeds" {
		t.Errorf("redirect_uri = %q, want /u/5/feeds", parsed.Query().Get("redirect_uri"))
	}
}
