package main

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/infodancer/authz"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for the authz handle
	"github.com/spf13/cobra"

	"github.com/matthewjhunter/herald/internal/storage"
)

// adminCmd groups administrative operations that are run by hand against a
// deployment, not part of the serving path.
func adminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administrative operations",
	}
	cmd.AddCommand(adminGrantCmd())
	return cmd
}

// adminGrantCmd grants a role to a webauth subject in herald's authz store.
//
// This is the run-once step that seeds admin after herald moves off the token
// role claim: the subject is the webauth user's UUID (its `sub`), NOT an email,
// because that is what the token carries and what authz keys on. The issuer is
// the tenant issuer herald validates tokens against. Idempotent -- re-granting
// an existing tuple is a no-op.
func adminGrantCmd() *cobra.Command {
	var issuer, subject, role, dsn string
	c := &cobra.Command{
		Use:   "grant",
		Short: "Grant a role to a webauth subject in the authz store",
		Long: "Grant a role to a webauth subject.\n\n" +
			"--subject is the token 'sub' (the webauth user UUID), not an email: " +
			"the access token carries the UUID and the authz store keys on it, so " +
			"an email would never match at request time.",
		// A role seed must not depend on the full serving config being valid;
		// it takes the DSN directly (flag or HERALD_DB_DSN) and loads no config.
		Annotations: map[string]string{annotationSkipConfigLoad: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if issuer == "" || subject == "" || role == "" {
				return fmt.Errorf("--issuer, --subject and --role are all required")
			}

			if dsn == "" {
				dsn = os.Getenv("HERALD_DB_DSN")
			}
			if dsn == "" {
				return fmt.Errorf("no database DSN: pass --dsn or set HERALD_DB_DSN")
			}

			// Open the store once so migrations run and the authz table is
			// guaranteed to exist, even if this is invoked before the web
			// process has started against a fresh database.
			store, err := storage.NewStore(dsn)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			store.Close()

			db, err := sql.Open("pgx", dsn)
			if err != nil {
				return fmt.Errorf("open authz handle: %w", err)
			}
			defer db.Close()

			if err := authz.NewPostgresStore(db).Grant(cmd.Context(), authz.Grant{
				Issuer:    issuer,
				Subject:   subject,
				Module:    "", // global grant
				Role:      role,
				GrantedBy: "herald admin grant",
			}); err != nil {
				return fmt.Errorf("grant role: %w", err)
			}

			fmt.Printf("granted role %q to subject %s at issuer %s\n", role, subject, issuer)
			return nil
		},
	}
	c.Flags().StringVar(&issuer, "issuer", "", "OIDC issuer URL the tokens are validated against")
	c.Flags().StringVar(&subject, "subject", "", "token subject (the webauth user UUID, not an email)")
	c.Flags().StringVar(&role, "role", "admin", "role to grant")
	c.Flags().StringVar(&dsn, "dsn", "", "database DSN (default: HERALD_DB_DSN)")
	return c
}
