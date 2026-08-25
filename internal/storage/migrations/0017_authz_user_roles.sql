-- +goose Up
--
-- Role grants for admin authorization, read by github.com/infodancer/authz.
-- Herald resolves admin from this table keyed on (issuer, subject) instead of
-- trusting a role claim in the access token, so a revoked grant takes effect at
-- once and a token cannot carry authority webauth no longer intends. Schema is
-- authz's canonical one; herald owns the migration so its own goose runner
-- creates it on startup.
CREATE TABLE IF NOT EXISTS authz_user_roles (
    issuer      TEXT        NOT NULL,
    subject     TEXT        NOT NULL,
    module      TEXT        NOT NULL DEFAULT '',
    role        TEXT        NOT NULL,
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    granted_by  TEXT        NOT NULL,
    PRIMARY KEY (issuer, subject, module, role)
);
CREATE INDEX IF NOT EXISTS authz_user_roles_lookup_idx ON authz_user_roles (issuer, subject);

-- +goose Down
DROP TABLE IF EXISTS authz_user_roles;
