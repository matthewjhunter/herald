package storage

const Schema = `
CREATE TABLE IF NOT EXISTS feeds (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    url TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL,
    description TEXT,
    site_url TEXT NOT NULL DEFAULT '',
    last_fetched DATETIME,
    last_error TEXT,
    etag TEXT,
    last_modified TEXT,
    enabled BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    consecutive_errors INTEGER NOT NULL DEFAULT 0,
    next_fetch_at DATETIME,
    status TEXT NOT NULL DEFAULT 'active'
);
CREATE TABLE IF NOT EXISTS articles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    feed_id INTEGER NOT NULL,
    guid TEXT NOT NULL,
    title TEXT NOT NULL,
    url TEXT NOT NULL,
    content TEXT,
    summary TEXT,
    author TEXT,
    published_date DATETIME,
    fetched_date DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    linked_url TEXT NOT NULL DEFAULT '',
    linked_content TEXT NOT NULL DEFAULT '',
    full_text_fetched BOOLEAN NOT NULL DEFAULT 0,
    images_cached BOOLEAN NOT NULL DEFAULT 0,
    -- Security verdict lives on the article, not per-user: maliciousness is a
    -- property of the content, so each article is screened once and the verdict
    -- is shared by every subscriber. security_screened_at distinguishes "screened
    -- but skipped" (set, score NULL) from "not yet screened" (NULL).
    security_score REAL,
    security_reason TEXT,
    security_flagged BOOLEAN NOT NULL DEFAULT 0,
    security_screened_at DATETIME,
    security_attempts INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE,
    UNIQUE(feed_id, guid)
);

CREATE INDEX IF NOT EXISTS idx_articles_published ON articles(published_date DESC);
-- idx_articles_unscreened (the partial index driving the security pass) is
-- created by the migrations, not here. On an upgrade-in-place the CREATE TABLE
-- above is a no-op, so security_screened_at does not exist until the ALTER
-- migration adds it; creating a WHERE security_screened_at IS NULL index in this
-- script would fail. The migration creates it after the column is guaranteed.

CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE COLLATE NOCASE,
    oidc_sub TEXT UNIQUE,
    email TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS read_state (
    user_id INTEGER NOT NULL DEFAULT 1,
    article_id INTEGER NOT NULL,
    read BOOLEAN NOT NULL DEFAULT 0,
    starred BOOLEAN NOT NULL DEFAULT 0,
    interest_score REAL,
    security_score REAL,
    read_date DATETIME,
    ai_scored BOOLEAN NOT NULL DEFAULT 0,
    ai_retries INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, article_id),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_read_state_article_user ON read_state(article_id, user_id);
CREATE INDEX IF NOT EXISTS idx_read_state_user_starred ON read_state(user_id) WHERE starred = 1;
CREATE INDEX IF NOT EXISTS idx_read_state_user_unscored ON read_state(user_id) WHERE ai_scored = 0;

CREATE TABLE IF NOT EXISTS user_preferences (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL DEFAULT 1,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    UNIQUE(user_id, key)
);

CREATE TABLE IF NOT EXISTS user_feeds (
    user_id INTEGER NOT NULL DEFAULT 1,
    feed_id INTEGER NOT NULL,
    subscribed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, feed_id),
    FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_user_feeds_feed ON user_feeds(feed_id);

CREATE TABLE IF NOT EXISTS feed_tags (
    user_id INTEGER NOT NULL DEFAULT 1,
    feed_id INTEGER NOT NULL,
    tag TEXT NOT NULL COLLATE NOCASE,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, feed_id, tag),
    FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_feed_tags_user_tag ON feed_tags(user_id, tag);

CREATE TABLE IF NOT EXISTS article_summaries (
    article_id INTEGER PRIMARY KEY,
    ai_summary TEXT NOT NULL,
    skip_reason TEXT,
    generated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS article_groups (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL DEFAULT 1,
    topic TEXT NOT NULL,
    embedding BLOB,
    embedding_model TEXT NOT NULL DEFAULT '',
    display_name TEXT,
    muted BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_article_groups_user ON article_groups(user_id);

CREATE TABLE IF NOT EXISTS article_group_members (
    group_id INTEGER NOT NULL,
    article_id INTEGER NOT NULL,
    added_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (group_id, article_id),
    FOREIGN KEY (group_id) REFERENCES article_groups(id) ON DELETE CASCADE,
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_group_members_article ON article_group_members(article_id);

CREATE TABLE IF NOT EXISTS group_summaries (
    group_id INTEGER PRIMARY KEY,
    headline TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL,
    article_count INTEGER NOT NULL,
    max_interest_score REAL,
    generated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (group_id) REFERENCES article_groups(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS user_prompts (
    user_id INTEGER NOT NULL DEFAULT 1,
    prompt_type TEXT NOT NULL,
    prompt_template TEXT NOT NULL,
    temperature REAL,
    model TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, prompt_type)
);

CREATE TABLE IF NOT EXISTS article_authors (
    article_id INTEGER NOT NULL,
    name TEXT NOT NULL COLLATE NOCASE,
    email TEXT,
    PRIMARY KEY (article_id, name),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_article_authors_name ON article_authors(name);

CREATE TABLE IF NOT EXISTS article_categories (
    article_id INTEGER NOT NULL,
    category TEXT NOT NULL COLLATE NOCASE,
    PRIMARY KEY (article_id, category),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_article_categories_category ON article_categories(category);

CREATE TABLE IF NOT EXISTS filter_rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    feed_id INTEGER,
    axis TEXT NOT NULL CHECK(axis IN ('author', 'category', 'tag')),
    value TEXT NOT NULL COLLATE NOCASE,
    score INTEGER NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_filter_rules_user ON filter_rules(user_id);
CREATE INDEX IF NOT EXISTS idx_filter_rules_lookup ON filter_rules(user_id, axis, value);
CREATE UNIQUE INDEX IF NOT EXISTS idx_filter_rules_unique
    ON filter_rules(user_id, COALESCE(feed_id, -1), axis, value);

CREATE TABLE IF NOT EXISTS fever_credentials (
    user_id INTEGER PRIMARY KEY,
    api_key TEXT NOT NULL UNIQUE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_fever_credentials_key ON fever_credentials(api_key);

CREATE TABLE IF NOT EXISTS article_images (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    article_id INTEGER NOT NULL,
    original_url TEXT NOT NULL,
    data BLOB NOT NULL,
    mime_type TEXT NOT NULL,
    width INTEGER NOT NULL DEFAULT 0,
    height INTEGER NOT NULL DEFAULT 0,
    fetched_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(article_id, original_url),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_article_images_article ON article_images(article_id);

CREATE TABLE IF NOT EXISTS feed_favicons (
    feed_id INTEGER PRIMARY KEY,
    data BLOB NOT NULL,
    mime_type TEXT NOT NULL,
    fetched_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE
);

-- See storage.EmbedStatus* constants for the meaning of status codes:
-- 0 = ok (real vector stored), 1 = too_short (deterministic skip, no
-- retry), 2 = error (transient failure, retryable while attempts < 5).
-- error_message holds the last error text when status=2; NULL otherwise,
-- and is critical for post-mortem diagnosis of backend failures.
CREATE TABLE IF NOT EXISTS article_embeddings (
    article_id INTEGER PRIMARY KEY,
    embedding BLOB NOT NULL,
    embedding_model TEXT NOT NULL,
    status SMALLINT NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    error_message TEXT,
    last_attempted_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS newsletters (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    schedule TEXT NOT NULL DEFAULT 'manual',
    config_json TEXT NOT NULL DEFAULT '{}',
    prompt_template TEXT NOT NULL DEFAULT '',
    email_recipient TEXT NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT 1,
    last_generated_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_newsletters_user ON newsletters(user_id);

CREATE TABLE IF NOT EXISTS newsletter_issues (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    newsletter_id INTEGER NOT NULL,
    headline TEXT NOT NULL DEFAULT '',
    content_html TEXT NOT NULL,
    content_text TEXT NOT NULL DEFAULT '',
    article_ids_json TEXT NOT NULL DEFAULT '[]',
    generated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    sent_at DATETIME,
    FOREIGN KEY (newsletter_id) REFERENCES newsletters(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_newsletter_issues_newsletter ON newsletter_issues(newsletter_id);
CREATE INDEX IF NOT EXISTS idx_newsletter_issues_generated ON newsletter_issues(generated_at DESC);

CREATE TABLE IF NOT EXISTS ai_summaries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    newsletter_id INTEGER,
    status TEXT NOT NULL DEFAULT 'generating',
    model TEXT NOT NULL DEFAULT '',
    prompt TEXT NOT NULL DEFAULT '',
    headline TEXT NOT NULL DEFAULT '',
    content_html TEXT NOT NULL DEFAULT '',
    article_ids_json TEXT NOT NULL DEFAULT '[]',
    article_count INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    generated_at DATETIME,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (newsletter_id) REFERENCES newsletters(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_ai_summaries_user ON ai_summaries(user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS cycle_stats (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    completed_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    duration_ms          INTEGER NOT NULL DEFAULT 0,
    feeds_total          INTEGER NOT NULL DEFAULT 0,
    feeds_downloaded     INTEGER NOT NULL DEFAULT 0,
    feeds_not_modified   INTEGER NOT NULL DEFAULT 0,
    feeds_errored        INTEGER NOT NULL DEFAULT 0,
    new_articles         INTEGER NOT NULL DEFAULT 0,
    processed            INTEGER NOT NULL DEFAULT 0,
    high_interest        INTEGER NOT NULL DEFAULT 0,
    ai_backend_available BOOLEAN NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_cycle_stats_completed ON cycle_stats(completed_at DESC);

-- Server-side OIDC sessions (#173). The browser holds only id (the opaque
-- session cookie); access_token and refresh_token never leave the server. The
-- refresh token rotates on every renewal and is the high-value credential.
-- Not user_id-keyed: a session exists from the callback (which records the OIDC
-- sub) before the Herald user row is provisioned on the first authed request.
CREATE TABLE IF NOT EXISTS sessions (
    id              TEXT PRIMARY KEY,
    user_sub        TEXT NOT NULL,
    access_token    TEXT NOT NULL,
    refresh_token   TEXT NOT NULL,
    access_expiry   DATETIME NOT NULL,
    absolute_expiry DATETIME NOT NULL,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_sessions_absolute_expiry ON sessions(absolute_expiry);
`
