package storage

const Schema = `
CREATE TABLE IF NOT EXISTS feeds (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    url TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL,
    description TEXT,
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
    FOREIGN KEY (feed_id) REFERENCES feeds(id) ON DELETE CASCADE,
    UNIQUE(feed_id, guid)
);

CREATE INDEX IF NOT EXISTS idx_articles_published ON articles(published_date DESC);

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

CREATE TABLE IF NOT EXISTS article_summaries (
    user_id INTEGER NOT NULL DEFAULT 1,
    article_id INTEGER NOT NULL,
    ai_summary TEXT NOT NULL,
    generated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, article_id),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_article_summaries_article ON article_summaries(article_id);

CREATE TABLE IF NOT EXISTS article_groups (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL DEFAULT 1,
    topic TEXT NOT NULL,
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
`
