package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/infodancer/oidclient"
	"github.com/matthewjhunter/herald"
	web "github.com/matthewjhunter/herald/internal/web"
	"github.com/spf13/cobra"
)

// serveCmd returns the cobra command for `herald serve` (the unified web UI).
func serveCmd() *cobra.Command {
	var (
		addr string
		// Additional web-specific CLI flags (override config file and env).
		webauthIssuer   string
		webauthURL      string
		jwtCookie       string
		webauthClientID string
		webauthCallback string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the web UI server",
		Long: `Run the Herald web interface (replaces the old standalone herald-web binary).
Requires a valid webauth OIDC configuration. Listens for HTTP and handles
user sessions, article browsing, admin, Fever API, etc.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Merge precedence (env > config > default), matching prior web behavior.
			// (A --db flag can be added if needed; HERALD_DB_DSN covers container overrides.)
			db := os.Getenv("HERALD_DB_DSN")
			if db == "" {
				db = cfg.Database.Path
			}
			if db == "" {
				db = "postgres://localhost:5432/herald?sslmode=disable"
			}
			listenAddr := mergeString(addr, mergeString("", cfg.Web.Addr))
			if listenAddr == "" {
				listenAddr = ":8080"
			}

			issuerURL := mergeString(webauthIssuer, cfg.Web.Webauth.IssuerURL)
			webauthBaseURL := mergeString(webauthURL, cfg.Web.Webauth.WebauthURL)
			cookie := mergeString(jwtCookie, mergeString("", cfg.Web.Webauth.Cookie))
			if cookie == "" {
				cookie = "infodancer_jwt"
			}
			clientID := mergeString(webauthClientID, cfg.Web.Webauth.ClientID)
			callbackURL := mergeString(webauthCallback, cfg.Web.Webauth.CallbackURL)

			if issuerURL == "" {
				return fmt.Errorf("webauth.issuer_url (or --webauth-issuer) is required")
			}

			ctx := context.Background()
			validator, err := oidclient.NewLazy(ctx, oidclient.Config{
				IssuerURL:   issuerURL,
				CookieName:  cookie,
				WebauthURL:  webauthBaseURL,
				ClientID:    clientID,
				ClientName:  "Herald",
				CallbackURL: callbackURL,
				// Request a refresh token so the session can be renewed server-side
				// without an interactive redirect (#173). The token is stored only in
				// the herald DB and never reaches the browser.
				OfflineAccess: true,
				Logf:          log.Printf,
			})
			if err != nil {
				return fmt.Errorf("create OIDC validator: %w", err)
			}

			engine, err := herald.NewEngine(herald.EngineConfig{
				DBPath:   db,
				ReadOnly: true,
				// AI Summary cloud backend — built even though the engine is read-only
				// (it is one streaming HTTPS call, not the Olla pipeline). The key comes
				// from the environment so it is never committed to config.
				SummaryBaseURL:         cfg.Summary.BaseURL,
				SummaryModel:           cfg.Summary.Model,
				SummaryAPIKey:          os.Getenv("HERALD_SUMMARY_API_KEY"),
				SummaryDisableThinking: cfg.Summary.DisableThinking,
				SummaryMaxInputTokens:  cfg.Summary.MaxInputTokens,
			})
			if err != nil {
				return fmt.Errorf("create engine: %w", err)
			}
			defer engine.Close()

			mux := web.NewRouter(engine, validator, cfg.Web.Admin.Role, cfg.Web.Admin.Users, web.AnalyticsConfig{
				UmamiSrc:  cfg.Web.Analytics.UmamiSrc,
				WebsiteID: cfg.Web.Analytics.WebsiteID,
			})

			// Sweep expired sessions hourly for the lifetime of the server (#173).
			sweepCtx, stopSweep := context.WithCancel(context.Background())
			defer stopSweep()
			go web.SweepExpiredSessions(sweepCtx, engine, time.Hour)

			srv := &http.Server{
				Addr:         listenAddr,
				Handler:      web.SecurityHeaders(web.Logging(web.Recovery(mux))),
				ReadTimeout:  15 * time.Second,
				WriteTimeout: 30 * time.Second,
				IdleTimeout:  60 * time.Second,
			}

			// Graceful shutdown on SIGINT/SIGTERM
			done := make(chan os.Signal, 1)
			signal.Notify(done, os.Interrupt, syscall.SIGTERM)

			go func() {
				log.Printf("herald serve: listening on %s", listenAddr)
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Fatalf("herald serve: %v", err)
				}
			}()

			<-done
			log.Println("herald serve: shutting down...")

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if err := srv.Shutdown(shutdownCtx); err != nil {
				log.Fatalf("herald serve: shutdown error: %v", err)
			}
			log.Println("herald serve: stopped")
			return nil
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "", "listen address (default :8080)")
	cmd.Flags().StringVar(&webauthIssuer, "webauth-issuer", "", "OIDC issuer URL (enables autodiscovery)")
	cmd.Flags().StringVar(&webauthURL, "webauth-url", "", "webauth base URL (derived from issuer if empty)")
	cmd.Flags().StringVar(&jwtCookie, "jwt-cookie", "", "JWT session cookie name (default infodancer_jwt)")
	cmd.Flags().StringVar(&webauthClientID, "webauth-client-id", "", "OIDC client ID")
	cmd.Flags().StringVar(&webauthCallback, "webauth-callback-url", "", "OIDC callback URL")

	return cmd
}

// mergeString returns first non-empty of a then b (simple flag/env/config precedence helper).
func mergeString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
