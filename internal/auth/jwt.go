package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims holds the JWT claims Herald uses for identity and provisioning.
type Claims struct {
	Sub   string
	Email string
	Name  string
}

// ValidatorConfig configures JWT validation.
type ValidatorConfig struct {
	// Issuer is the expected iss claim value. Empty means skip issuer check.
	Issuer string
	// CookieName is the name of the cookie containing the JWT.
	CookieName string
	// WebauthURL is the base URL of the webauth server (e.g. https://auth.infodancer.net).
	WebauthURL string
	// JWKSEndpoint is the JWKS discovery URL. Takes precedence over PEMKeyPath.
	JWKSEndpoint string
	// PEMKeyPath is the path to an RSA public key PEM file, used when JWKS is not yet live.
	PEMKeyPath string
	// TenantID is the webauth tenant ID used for the OIDC authorize and token endpoints.
	TenantID string
	// ClientID is Herald's registered OIDC client ID.
	ClientID string
	// CallbackURL is Herald's registered OIDC redirect URI.
	CallbackURL string
	// HTTPClient overrides the HTTP client used for token exchange and JWKS fetches.
	// If nil a default client with a 10s timeout is used.
	HTTPClient *http.Client
}

// Validator validates RS256 JWTs issued by the webauth server.
// Keys are cached in memory and refreshed from JWKS on kid-miss or TTL expiry.
type Validator struct {
	cfg           ValidatorConfig
	httpClient    *http.Client
	mu            sync.RWMutex
	keys          map[string]*rsa.PublicKey // kid → key ("" for PEM-loaded key without kid)
	keysFetchedAt time.Time
	keysTTL       time.Duration
}

// NewValidator creates a Validator and eagerly loads public keys.
// Returns an error if the key source is misconfigured or unreachable.
func NewValidator(cfg ValidatorConfig) (*Validator, error) {
	if cfg.JWKSEndpoint == "" && cfg.PEMKeyPath == "" {
		return nil, fmt.Errorf("auth: one of JWKSEndpoint or PEMKeyPath must be set")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	v := &Validator{
		cfg:        cfg,
		httpClient: client,
		keys:       make(map[string]*rsa.PublicKey),
		keysTTL:    time.Hour,
	}
	if err := v.loadKeys(); err != nil {
		return nil, err
	}
	return v, nil
}

// CookieName returns the name of the JWT session cookie.
func (v *Validator) CookieName() string { return v.cfg.CookieName }

// OIDCConfigured reports whether the OIDC callback flow is configured.
func (v *Validator) OIDCConfigured() bool {
	return v.cfg.TenantID != "" && v.cfg.ClientID != "" && v.cfg.CallbackURL != ""
}

// AuthorizeURL builds the OIDC authorization URL with PKCE.
// state is an opaque nonce; challenge is the base64url-encoded SHA-256 of the PKCE verifier.
func (v *Validator) AuthorizeURL(state, challenge string) string {
	base := fmt.Sprintf("%s/t/%s/authorize", v.cfg.WebauthURL, v.cfg.TenantID)
	u, _ := url.Parse(base)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", v.cfg.ClientID)
	q.Set("redirect_uri", v.cfg.CallbackURL)
	q.Set("scope", "openid email profile")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String()
}

// ExchangeCode exchanges an authorization code for an access token.
// verifier is the PKCE code_verifier that was used to derive the challenge.
// Returns the access_token JWT string from the token endpoint response.
func (v *Validator) ExchangeCode(ctx context.Context, code, verifier string) (string, error) {
	tokenURL := fmt.Sprintf("%s/t/%s/token", v.cfg.WebauthURL, v.cfg.TenantID)

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {v.cfg.CallbackURL},
		"client_id":     {v.cfg.ClientID},
		"code_verifier": {verifier},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}

	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("token response missing access_token")
	}
	return body.AccessToken, nil
}

// ValidateCookie extracts the JWT from the named cookie and validates it.
func (v *Validator) ValidateCookie(r *http.Request) (*Claims, error) {
	cookie, err := r.Cookie(v.cfg.CookieName)
	if err != nil {
		return nil, fmt.Errorf("missing auth cookie")
	}
	return v.Validate(cookie.Value)
}

// Validate parses and validates a raw JWT string, returning the extracted claims.
func (v *Validator) Validate(tokenStr string) (*Claims, error) {
	token, err := jwt.Parse(tokenStr, v.keyFunc, jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	mapClaims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	if v.cfg.Issuer != "" {
		iss, _ := mapClaims["iss"].(string)
		if iss != v.cfg.Issuer {
			return nil, fmt.Errorf("token issuer mismatch: got %q, want %q", iss, v.cfg.Issuer)
		}
	}

	claims := &Claims{
		Sub:   stringClaim(mapClaims, "sub"),
		Email: stringClaim(mapClaims, "email"),
		Name:  stringClaim(mapClaims, "name"),
	}
	if claims.Sub == "" {
		return nil, fmt.Errorf("token missing sub claim")
	}
	return claims, nil
}

// WebauthLoginURL returns the URL to redirect unauthenticated users to.
// redirectPath is the path (with query string) to return to after login.
func (v *Validator) WebauthLoginURL(redirectPath string) string {
	base := v.cfg.WebauthURL + "/login"
	if redirectPath == "" {
		return base
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("redirect_uri", redirectPath)
	u.RawQuery = q.Encode()
	return u.String()
}

// WebauthLogoutURL returns the webauth logout URL.
func (v *Validator) WebauthLogoutURL() string {
	return v.cfg.WebauthURL + "/logout"
}

// keyFunc is the jwt.Keyfunc used during token parsing.
// It enforces RS256 and resolves the signing key from the cache, refreshing
// on kid-miss when a JWKS endpoint is configured.
func (v *Validator) keyFunc(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
		return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
	}
	kid, _ := token.Header["kid"].(string)

	v.mu.RLock()
	key, ok := v.findKey(kid)
	expired := v.cfg.JWKSEndpoint != "" && time.Since(v.keysFetchedAt) > v.keysTTL
	v.mu.RUnlock()

	if ok && !expired {
		return key, nil
	}

	// Cache miss or TTL expired — re-fetch JWKS if configured.
	if v.cfg.JWKSEndpoint != "" {
		v.mu.Lock()
		// Re-check under write lock to avoid thundering herd.
		if key, ok = v.findKey(kid); !ok || expired {
			_ = v.fetchJWKS() // best-effort; log omitted to avoid import cycle
			key, ok = v.findKey(kid)
		}
		v.mu.Unlock()
		if ok {
			return key, nil
		}
	}

	return nil, fmt.Errorf("public key not found for kid %q", kid)
}

// findKey returns the RSA key for the given kid. If kid is empty, the first
// available key is returned (covers PEM-loaded keys and single-key JWKS).
// Caller must hold at least a read lock.
func (v *Validator) findKey(kid string) (*rsa.PublicKey, bool) {
	if kid != "" {
		key, ok := v.keys[kid]
		return key, ok
	}
	for _, key := range v.keys {
		return key, true
	}
	return nil, false
}

// loadKeys loads keys from whichever source is configured.
func (v *Validator) loadKeys() error {
	if v.cfg.JWKSEndpoint != "" {
		return v.fetchJWKS()
	}
	return v.loadPEM()
}

// fetchJWKS fetches RSA public keys from the JWKS endpoint and updates the cache.
// Caller must hold the write lock (except during NewValidator).
func (v *Validator) fetchJWKS() error {
	resp, err := v.httpClient.Get(v.cfg.JWKSEndpoint) //nolint:noctx
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("parse JWKS: %w", err)
	}

	newKeys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		key, err := rsaKeyFromJWK(k.N, k.E)
		if err != nil {
			continue
		}
		newKeys[k.Kid] = key
	}
	if len(newKeys) == 0 {
		return fmt.Errorf("JWKS contained no usable RSA keys")
	}
	v.keys = newKeys
	v.keysFetchedAt = time.Now()
	return nil
}

// loadPEM loads an RSA public key from a PEM file. Stored under kid="" so
// keyFunc finds it for tokens that omit the kid header.
func (v *Validator) loadPEM() error {
	data, err := os.ReadFile(v.cfg.PEMKeyPath)
	if err != nil {
		return fmt.Errorf("read public key file: %w", err)
	}
	key, err := jwt.ParseRSAPublicKeyFromPEM(data)
	if err != nil {
		return fmt.Errorf("parse RSA public key PEM: %w", err)
	}
	v.keys[""] = key
	return nil
}

// rsaKeyFromJWK constructs an *rsa.PublicKey from base64url-encoded n and e.
func rsaKeyFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("decode JWK n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("decode JWK e: %w", err)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(new(big.Int).SetBytes(eBytes).Int64()),
	}, nil
}

func stringClaim(claims jwt.MapClaims, key string) string {
	v, _ := claims[key].(string)
	return v
}
