package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/auth"
)

// version and buildTime are optionally injected at build time via ldflags.
// When not set (or when a full 40-char hash is injected), init() normalises
// them using Go's embedded VCS build info.
var version = "dev"
var buildTime = "unknown"

func init() {
	// Truncate any full commit hash (40 chars) down to the conventional 7.
	if len(version) > 12 {
		version = version[:7]
	}
	// Fall back to Go's embedded VCS metadata when ldflags weren't used.
	if version == "dev" || buildTime == "unknown" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					if version == "dev" && len(s.Value) >= 7 {
						version = s.Value[:7]
					}
				case "vcs.time":
					if buildTime == "unknown" {
						if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
							buildTime = t.UTC().Format("2006-01-02T15:04Z")
						}
					}
				}
			}
		}
	}
}

func main() {
	configPath := flag.String("config", "", "path to TOML config file")

	// CLI flags — all default to \"\" so config file values take effect when flags are omitted.
	dbPath := flag.String("db", "", "path to SQLite database (default ./herald.db)")
	addr := flag.String("addr", "", "listen address (default :8080)")

	// Auth flags.
	webauthIssuer := flag.String("webauth-issuer", "", "OIDC issuer URL, e.g. https://auth.infodancer.net/t/infodancer (enables autodiscovery)")
	webauthURL := flag.String("webauth-url", "", "base URL of webauth server; derived from -webauth-issuer when omitted")
	jwtCookie := flag.String("jwt-cookie", "", "name of the JWT cookie set by webauth (default infodancer_jwt)")
	jwksURL := flag.String("jwks-url", "", "JWKS endpoint URL; overrides autodiscovery when set")
	pemKeyPath := flag.String("jwt-public-key", "", "path to RSA public key PEM file (dev fallback when JWKS not yet live)")
	jwtIssuer := flag.String("jwt-issuer", "", "expected JWT issuer claim (empty = skip validation)")
	webauthTenant := flag.String("webauth-tenant", "", "webauth tenant ID for manual OIDC endpoint construction (not needed with -webauth-issuer)")
	webauthClientID := flag.String("webauth-client-id", "", "Herald's registered OIDC client ID")
	webauthCallbackURL := flag.String("webauth-callback-url", "", "Herald's registered OIDC callback URL (e.g. https://herald.infodancer.net/auth/callback)")

	flag.Parse()

	// Load config file (empty path is a no-op).
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herald-web: %v\n", err)
		os.Exit(1)
	}

	// Merge: CLI flag wins over config file, config file wins over hardcoded default.
	db := mergeString(*dbPath, mergeString(cfg.DB, "./herald.db"))
	listenAddr := mergeString(*addr, mergeString(cfg.Addr, ":8080"))
	issuerURL := mergeString(*webauthIssuer, cfg.Webauth.IssuerURL)
	webauthBaseURL := mergeString(*webauthURL, cfg.Webauth.WebauthURL)
	cookie := mergeString(*jwtCookie, mergeString(cfg.Webauth.Cookie, "infodancer_jwt"))
	jwks := mergeString(*jwksURL, cfg.Webauth.JWKSUrl)
	pem := mergeString(*pemKeyPath, cfg.Webauth.PEMKeyPath)
	jwtIss := mergeString(*jwtIssuer, cfg.Webauth.JWTIssuer)
	tenantID := mergeString(*webauthTenant, cfg.Webauth.TenantID)
	clientID := mergeString(*webauthClientID, cfg.Webauth.ClientID)
	callbackURL := mergeString(*webauthCallbackURL, cfg.Webauth.CallbackURL)

	if issuerURL == "" && webauthBaseURL == "" {
		fmt.Fprintln(os.Stderr, "herald-web: auth.issuer_url (or -webauth-issuer) is required")
		os.Exit(1)
	}
	if issuerURL == "" && jwks == "" && pem == "" {
		fmt.Fprintln(os.Stderr, "herald-web: auth.jwks_url or auth.pem_key_path required when issuer_url is not set")
		os.Exit(1)
	}

	validator, err := auth.NewValidator(auth.ValidatorConfig{
		Issuer:       jwtIss,
		CookieName:   cookie,
		IssuerURL:    issuerURL,
		WebauthURL:   webauthBaseURL,
		JWKSEndpoint: jwks,
		PEMKeyPath:   pem,
		TenantID:     tenantID,
		ClientID:     clientID,
		CallbackURL:  callbackURL,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "herald-web: %v\n", err)
		os.Exit(1)
	}

	engine, err := herald.NewEngine(herald.EngineConfig{
		DBPath:   db,
		ReadOnly: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "herald-web: %v\n", err)
		os.Exit(1)
	}
	defer engine.Close()

	mux := newRouter(engine, validator, cfg.Admin.Role, cfg.Admin.Users)

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      logging(recovery(mux)),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("herald-web: listening on %s", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("herald-web: %v", err)
		}
	}()

	<-done
	log.Println("herald-web: shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("herald-web: shutdown error: %v", err)
	}
	log.Println("herald-web: stopped")
}
