package storage

// SchemaPostgres is the PostgreSQL database schema for Herald.
// All statements are idempotent (IF NOT EXISTS / CREATE EXTENSION IF NOT EXISTS).
const SchemaPostgres = `
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE IF NOT EXISTS feeds (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    url                TEXT NOT NULL UNIQUE,
    title              TEXT NOT NULL,
    description        TEXT,
    site_url           TEXT NOT NULL DEFAULT '',
    last_fetched       TIMESTAMPTZ,
    last_error         TEXT,
    etag               TEXT,
    last_modified      TEXT,
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    consecutive_errors BIGINT NOT NULL DEFAULT 0,
    next_fetch_at      TIMESTAMPTZ,
    status             TEXT NOT NULL DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS articles (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    feed_id           BIGINT NOT NULL,
    guid              TEXT NOT NULL,
    title             TEXT NOT NULL,
    url               TEXT NOT NULL,
    content           TEXT,
    summary           TEXT,
    author            TEXT,
    published_date    TIMESTAMPTZ,
    fetched_date      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    linked_url        TEXT NOT NULL DEFAULT '',
    linked_content    TEXT NOT NULL DEFAULT '',
    full_text_fetched BOOLEAN NOT NULL DEFAULT FALSE,
    images_cached     BOOLEAN NOT NULL DEFAULT FALSE,
    FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE,
    UNIQUE(feed_id, guid)
);

CREATE INDEX IF NOT EXISTS idx_articles_published ON articles(published_date DESC);

CREATE TABLE IF NOT EXISTS users (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       CITEXT NOT NULL UNIQUE,
    oidc_sub   TEXT UNIQUE,
    email      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS read_state (
    user_id        BIGINT NOT NULL DEFAULT 1,
    article_id     BIGINT NOT NULL,
    read           BOOLEAN NOT NULL DEFAULT FALSE,
    starred        BOOLEAN NOT NULL DEFAULT FALSE,
    interest_score DOUBLE PRECISION,
    security_score DOUBLE PRECISION,
    read_date      TIMESTAMPTZ,
    ai_scored      BOOLEAN NOT NULL DEFAULT FALSE,
    ai_retries     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, article_id),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_read_state_article_user ON read_state(article_id, user_id);
CREATE INDEX IF NOT EXISTS idx_read_state_user_starred ON read_state(user_id) WHERE starred = TRUE;
CREATE INDEX IF NOT EXISTS idx_read_state_user_unscored ON read_state(user_id) WHERE ai_scored = FALSE;

CREATE TABLE IF NOT EXISTS user_preferences (
    id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL DEFAULT 1,
    key     TEXT NOT NULL,
    value   TEXT NOT NULL,
    UNIQUE(user_id, key)
);

CREATE TABLE IF NOT EXISTS user_feeds (
    user_id       BIGINT NOT NULL DEFAULT 1,
    feed_id       BIGINT NOT NULL,
    subscribed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, feed_id),
    FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_user_feeds_feed ON user_feeds(feed_id);

CREATE TABLE IF NOT EXISTS article_summaries (
    user_id      BIGINT NOT NULL DEFAULT 1,
    article_id   BIGINT NOT NULL,
    ai_summary   TEXT NOT NULL,
    generated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, article_id),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_article_summaries_article ON article_summaries(article_id);

CREATE TABLE IF NOT EXISTS article_groups (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      BIGINT NOT NULL DEFAULT 1,
    topic        TEXT NOT NULL,
    embedding       BYTEA,
    embedding_model TEXT NOT NULL DEFAULT '',
    display_name    TEXT,
    muted        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_article_groups_user ON article_groups(user_id);

CREATE TABLE IF NOT EXISTS article_group_members (
    group_id   BIGINT NOT NULL,
    article_id BIGINT NOT NULL,
    added_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_id, article_id),
    FOREIGN KEY (group_id) REFERENCES article_groups(id) ON DELETE CASCADE,
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_group_members_article ON article_group_members(article_id);

CREATE TABLE IF NOT EXISTS group_summaries (
    group_id          BIGINT PRIMARY KEY,
    headline          TEXT NOT NULL DEFAULT '',
    summary           TEXT NOT NULL,
    article_count     BIGINT NOT NULL,
    max_interest_score DOUBLE PRECISION,
    generated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (group_id) REFERENCES article_groups(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS user_prompts (
    user_id         BIGINT NOT NULL DEFAULT 1,
    prompt_type     TEXT NOT NULL,
    prompt_template TEXT NOT NULL,
    temperature     DOUBLE PRECISION,
    model           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, prompt_type)
);

CREATE TABLE IF NOT EXISTS article_authors (
    article_id BIGINT NOT NULL,
    name       CITEXT NOT NULL,
    email      TEXT,
    PRIMARY KEY (article_id, name),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_article_authors_name ON article_authors(name);

CREATE TABLE IF NOT EXISTS article_categories (
    article_id BIGINT NOT NULL,
    category   CITEXT NOT NULL,
    PRIMARY KEY (article_id, category),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_article_categories_category ON article_categories(category);

CREATE TABLE IF NOT EXISTS filter_rules (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL,
    feed_id    BIGINT,
    axis       TEXT NOT NULL CHECK(axis IN ('author', 'category', 'tag')),
    value      CITEXT NOT NULL,
    score      BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_filter_rules_user ON filter_rules(user_id);
CREATE INDEX IF NOT EXISTS idx_filter_rules_lookup ON filter_rules(user_id, axis, value);
CREATE UNIQUE INDEX IF NOT EXISTS idx_filter_rules_unique
    ON filter_rules(user_id, COALESCE(feed_id, -1), axis, value);

CREATE TABLE IF NOT EXISTS fever_credentials (
    user_id BIGINT PRIMARY KEY,
    api_key TEXT NOT NULL UNIQUE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_fever_credentials_key ON fever_credentials(api_key);

CREATE TABLE IF NOT EXISTS article_images (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    article_id   BIGINT NOT NULL,
    original_url TEXT NOT NULL,
    data         BYTEA NOT NULL,
    mime_type    TEXT NOT NULL,
    width        BIGINT NOT NULL DEFAULT 0,
    height       BIGINT NOT NULL DEFAULT 0,
    fetched_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(article_id, original_url),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_article_images_article ON article_images(article_id);

CREATE TABLE IF NOT EXISTS feed_favicons (
    feed_id    BIGINT PRIMARY KEY,
    data       BYTEA NOT NULL,
    mime_type  TEXT NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS article_embeddings (
    article_id BIGINT PRIMARY KEY,
    embedding  BYTEA NOT NULL,
    embedding_model TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS newsletters (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id           BIGINT NOT NULL,
    name              TEXT NOT NULL,
    schedule          TEXT NOT NULL DEFAULT 'manual',
    config_json       TEXT NOT NULL DEFAULT '{}',
    prompt_template   TEXT NOT NULL DEFAULT '',
    email_recipient   TEXT NOT NULL DEFAULT '',
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    last_generated_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_newsletters_user ON newsletters(user_id);

CREATE TABLE IF NOT EXISTS newsletter_issues (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    newsletter_id    BIGINT NOT NULL,
    headline         TEXT NOT NULL DEFAULT '',
    content_html     TEXT NOT NULL,
    content_text     TEXT NOT NULL DEFAULT '',
    article_ids_json TEXT NOT NULL DEFAULT '[]',
    generated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at          TIMESTAMPTZ,
    FOREIGN KEY (newsletter_id) REFERENCES newsletters(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_newsletter_issues_newsletter ON newsletter_issues(newsletter_id);
CREATE INDEX IF NOT EXISTS idx_newsletter_issues_generated ON newsletter_issues(generated_at DESC);
`
