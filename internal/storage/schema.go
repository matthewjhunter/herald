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
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
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

CREATE INDEX IF NOT EXISTS idx_articles_feed_id ON articles(feed_id);
CREATE INDEX IF NOT EXISTS idx_articles_published ON articles(published_date DESC);

CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE COLLATE NOCASE,
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
    PRIMARY KEY (user_id, article_id),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);

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

CREATE INDEX IF NOT EXISTS idx_user_feeds_user ON user_feeds(user_id);
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

CREATE INDEX IF NOT EXISTS idx_user_prompts_user ON user_prompts(user_id);
`
