package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/auth"
)

// version is injected at build time via -ldflags "-X main.version=<git-hash>".
var version = "dev"

func main() {
	dbPath := flag.String("db", "./herald.db", "path to SQLite database")
	addr := flag.String("addr", ":8080", "listen address")

	// Auth flags.
	webauthIssuer := flag.String("webauth-issuer", "", "OIDC issuer URL, e.g. https://auth.infodancer.net/t/infodancer (enables autodiscovery)")
	webauthURL := flag.String("webauth-url", "", "base URL of webauth server; derived from -webauth-issuer when omitted")
	jwtCookie := flag.String("jwt-cookie", "infodancer_jwt", "name of the JWT cookie set by webauth")
	jwksURL := flag.String("jwks-url", "", "JWKS endpoint URL; overrides autodiscovery when set")
	pemKeyPath := flag.String("jwt-public-key", "", "path to RSA public key PEM file (dev fallback when JWKS not yet live)")
	jwtIssuer := flag.String("jwt-issuer", "", "expected JWT issuer claim (empty = skip validation)")
	webauthTenant := flag.String("webauth-tenant", "", "webauth tenant ID for manual OIDC endpoint construction (not needed with -webauth-issuer)")
	webauthClientID := flag.String("webauth-client-id", "", "Herald's registered OIDC client ID")
	webauthCallbackURL := flag.String("webauth-callback-url", "", "Herald's registered OIDC callback URL (e.g. https://herald.infodancer.net/auth/callback)")

	flag.Parse()

	if *webauthIssuer == "" && *webauthURL == "" {
		fmt.Fprintln(os.Stderr, "herald-web: -webauth-issuer or -webauth-url is required")
		os.Exit(1)
	}
	if *webauthIssuer == "" && *jwksURL == "" && *pemKeyPath == "" {
		fmt.Fprintln(os.Stderr, "herald-web: one of -jwks-url or -jwt-public-key is required (or use -webauth-issuer for autodiscovery)")
		os.Exit(1)
	}

	validator, err := auth.NewValidator(auth.ValidatorConfig{
		Issuer:       *jwtIssuer,
		CookieName:   *jwtCookie,
		IssuerURL:    *webauthIssuer,
		WebauthURL:   *webauthURL,
		JWKSEndpoint: *jwksURL,
		PEMKeyPath:   *pemKeyPath,
		TenantID:     *webauthTenant,
		ClientID:     *webauthClientID,
		CallbackURL:  *webauthCallbackURL,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "herald-web: %v\n", err)
		os.Exit(1)
	}

	engine, err := herald.NewEngine(herald.EngineConfig{
		DBPath:   *dbPath,
		ReadOnly: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "herald-web: %v\n", err)
		os.Exit(1)
	}
	defer engine.Close()

	mux := newRouter(engine, validator)

	srv := &http.Server{
		Addr:         *addr,
		Handler:      logging(recovery(mux)),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("herald-web: listening on %s", *addr)
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
