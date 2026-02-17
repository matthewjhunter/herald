# Usage Guide

## Quick Start

```bash
# 1. Initialize configuration
./herald init-config

# 2. Import your feeds
./herald import subscriptions.opml

# 3. Fetch and process articles
./herald fetch

# 4. List unread articles
./herald list

# 5. Mark article as read
./herald read <article-id>
```

## Commands

### `init-config`

Creates a default configuration file.

```bash
./herald init-config

# Use custom path
./herald --config /path/to/config.yaml init-config
```

### `import`

Imports feeds from an OPML file.

```bash
./herald import feeds.opml
```

**OPML Format:**

```xml
<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Tech News" title="Tech News" type="rss"
             xmlUrl="https://example.com/feed.xml"/>
    <!-- More feeds... -->
  </body>
</opml>
```

**Supported:**
- Flat OPML (all feeds at root level)
- Nested OPML (feeds organized in folders)
- Standard RSS/Atom subscription exports

### `fetch`

Fetches all enabled feeds and processes articles with AI.

```bash
./herald fetch
```

**What happens:**

1. **Fetch**: Downloads new articles from all feeds
2. **Security Check**: Gemma 2 analyzes content for threats
   - Prompt injection detection
   - Malicious content screening
   - Articles below security threshold are rejected
3. **Curation**: Llama 3.2 scores articles for interest
   - Considers user keywords
   - Evaluates news value and relevance
   - Assigns 0-10 interest score
4. **Notification**: High-scoring articles trigger Majordomo

**Example output:**

```
Fetched 50 new articles
Processing 50 unread articles...
⚠️  Unsafe article detected (score: 4.2): Suspicious clickbait title
📊 Processed: Real Tech News (interest: 7.5, security: 9.0)
📊 Processed: Important Update (interest: 9.2, security: 9.5)
...
Processed 50 articles

🔥 Found 3 high-interest articles (score >= 8.0)
```

### `list`

Lists unread articles.

```bash
# List 20 most recent unread articles (default)
./herald list

# List specific number
./herald list --limit 50
./herald list -n 10
```

**Example output:**

```
Unread articles (10):

ID: 42
Title: Breakthrough in Quantum Computing
URL: https://example.com/quantum-breakthrough
Published: 2026-02-17 14:30
---
ID: 43
Title: New Security Vulnerability Discovered
URL: https://example.com/security-vuln
Published: 2026-02-17 14:15
---
```

### `read`

Marks an article as read.

```bash
./herald read 42
```

## Workflows

### Daily News Briefing

Set up a morning briefing:

```bash
# Add to crontab for 8 AM daily
0 8 * * * cd /path/to/herald && ./herald fetch && ./herald list --limit 20
```

### Continuous Monitoring

For breaking news:

```bash
# Fetch every 15 minutes
*/15 * * * * cd /path/to/herald && ./herald fetch
```

### Manual Curation

Review articles yourself:

```bash
# Fetch new articles
./herald fetch

# List them
./herald list --limit 50 > articles.txt

# Review in your editor
vim articles.txt

# Mark interesting ones as read after reading
./herald read 42
./herald read 43
```

### Batch Processing

Process articles in batches:

```bash
# Fetch without AI processing (if Ollama is down)
./herald fetch || echo "AI processing unavailable"

# Later, manually review
./herald list | less
```

## AI Processing Details

### Security Layer (Gemma 2)

**Purpose**: Detect malicious or manipulative content

**Checks for:**
- Prompt injection attempts
- Phishing links
- Malware distribution
- Misinformation patterns
- Manipulative clickbait

**Scoring (0-10):**
- 0-4: Unsafe, article rejected
- 5-6: Questionable, flagged
- 7-10: Safe, processed normally

**Default threshold**: 7.0 (configurable)

### Curation Layer (Llama 3.2)

**Purpose**: Score articles for personal interest

**Considers:**
- Relevance to user keywords
- News value and importance
- Timeliness and uniqueness
- Content quality

**Scoring (0-10):**
- 0-3: Low interest
- 4-6: Moderate interest
- 7-8: High interest
- 9-10: Exceptional interest

**Default notification threshold**: 8.0 (configurable)

## Customizing Curation

### Keywords

Add topics you care about in `config/config.yaml`:

```yaml
preferences:
  keywords:
    - artificial intelligence
    - cybersecurity
    - quantum computing
    - climate change
    - space exploration
```

**Effect**: Articles matching these keywords get higher interest scores.

### Thresholds

Adjust notification sensitivity:

```yaml
thresholds:
  interest_score: 9.0    # Only notify for exceptional articles
  security_score: 6.0    # More permissive security
```

### Preferred Sources

Boost articles from trusted sources:

```yaml
preferences:
  preferred_sources:
    - https://arstechnica.com/
    - https://krebsonsecurity.com/
    - https://example.com/tech-blog/
```

## Integration Examples

### Majordomo Voice Notifications

When a high-interest article is found:

```
Jarvis: "Sir, high-interest article detected. Breakthrough in Quantum
Computing at MIT. Would you like to hear more?"
```

Configure in `config/config.yaml`:

```yaml
majordomo:
  enabled: true
  chat_command: majordomo
  target_persona: jarvis
```

### Slack Notifications

Add a custom notification script:

```bash
#!/bin/bash
# notify-slack.sh

ARTICLE_TITLE="$1"
ARTICLE_URL="$2"
SCORE="$3"

curl -X POST https://hooks.slack.com/services/YOUR/WEBHOOK/URL \
  -H 'Content-Type: application/json' \
  -d "{\"text\":\"🔥 High Interest Article ($SCORE/10)\\n*$ARTICLE_TITLE*\\n$ARTICLE_URL\"}"
```

Modify `internal/majordomo/notify.go` to call this script.

### Email Digest

Create a daily digest:

```bash
#!/bin/bash
# daily-digest.sh

cd /path/to/herald

# Fetch latest
./herald fetch

# Generate digest
{
  echo "Subject: Daily News Digest"
  echo ""
  ./herald list --limit 20
} | sendmail your@email.com
```

Add to crontab:

```cron
0 8 * * * /path/to/daily-digest.sh
```

## Advanced Usage

### Multiple Configurations

Use different configs for different topics:

```bash
# Tech news config
./herald --config tech-config.yaml fetch

# Security news config
./herald --config security-config.yaml fetch
```

### Database Queries

Direct SQL queries for analysis:

```bash
# Most popular feeds
sqlite3 herald.db "
SELECT f.title, COUNT(*) as articles
FROM articles a
JOIN feeds f ON a.feed_id = f.id
GROUP BY f.id
ORDER BY articles DESC
LIMIT 10"

# Average interest scores by feed
sqlite3 herald.db "
SELECT f.title, AVG(rs.interest_score) as avg_score
FROM read_state rs
JOIN articles a ON rs.article_id = a.id
JOIN feeds f ON a.feed_id = f.id
WHERE rs.interest_score IS NOT NULL
GROUP BY f.id
ORDER BY avg_score DESC"

# Most interesting articles
sqlite3 herald.db "
SELECT a.title, a.url, rs.interest_score
FROM articles a
JOIN read_state rs ON a.id = rs.article_id
WHERE rs.interest_score >= 8.0
ORDER BY rs.interest_score DESC
LIMIT 20"
```

### Export Read List

Track what you've read:

```bash
sqlite3 herald.db -header -csv "
SELECT a.title, a.url, a.published_date, rs.interest_score
FROM articles a
JOIN read_state rs ON a.id = rs.article_id
WHERE rs.read = 1
ORDER BY rs.read_date DESC" > read_articles.csv
```

## Tips & Best Practices

1. **Start with fewer feeds**: Import 10-20 feeds initially, add more later
2. **Tune thresholds gradually**: Start with defaults, adjust based on results
3. **Review security alerts**: Investigate articles flagged as unsafe
4. **Adjust keywords**: Add keywords as you identify interests
5. **Clean up periodically**: Remove inactive feeds and old articles
6. **Monitor Ollama**: Ensure models are loaded and responsive
7. **Check logs**: Review fetch logs for errors or issues

## Troubleshooting

### No Articles Appear

```bash
# Check feed URLs
sqlite3 herald.db "SELECT * FROM feeds WHERE enabled = 1"

# Test a feed manually
curl https://example.com/feed.xml
```

### All Articles Rejected

Security threshold may be too high:

```yaml
thresholds:
  security_score: 5.0  # Lower threshold
```

### No Notifications

Interest threshold may be too high:

```yaml
thresholds:
  interest_score: 6.0  # Lower threshold
```

### Slow Processing

Reduce articles per fetch or use faster models:

```yaml
ollama:
  curation_model: llama3.2:1b  # Smaller, faster model
```
