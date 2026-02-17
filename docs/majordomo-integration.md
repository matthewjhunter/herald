# Majordomo Integration

This document describes how to integrate feedreader with the [Majordomo](https://github.com/matthewjhunter/majordomo) voice assistant daemon for automated feed monitoring and voice notifications.

## Overview

Majordomo's cron system can execute commands and deliver their output to AI personas for processing and voice notification. Feedreader provides JSON output compatible with Majordomo's CommandOutput schema.

## Architecture

The integration uses two separate commands for efficient multi-user operation:

1. **`feedreader fetch-feeds`** - Downloads all subscribed feeds once, stores articles globally
2. **`feedreader process --user=N --format=json`** - Performs per-user AI processing and outputs notifications

This architecture allows:
- Each feed is fetched only once per run, regardless of subscriber count
- Per-user processing can run in parallel
- Users can have different AI models and prompts
- Efficient resource usage with large user bases

## Majordomo Configuration

Add the following entries to your Majordomo config file (typically `~/.config/majordomo/config.toml`):

### Fetch All Feeds (Once Per Schedule)

```toml
[[daemon.schedule]]
name = "feedreader-fetch"
cron = "*/15 * * * *"  # Every 15 minutes
persona = "jarvis"
command = "feedreader fetch-feeds --format=json"
format = "json"
timeout_sec = 300  # 5 minutes for fetching all feeds
origin = "daemon"
presence = "queue"  # Queue if user is away
speak = "present"   # Only speak if user is present
```

### Process Feeds Per User

For each user, add a processing schedule:

```toml
[[daemon.schedule]]
name = "feedreader-process-user1"
cron = "*/15 * * * *"  # Match fetch schedule
persona = "jarvis"
command = "feedreader process --user=1 --format=json"
format = "json"
timeout_sec = 600  # 10 minutes for AI processing
origin = "user"
presence = "queue"  # Queue notifications if user is away
speak = "present"   # Speak notifications when user is present
```

For additional users:

```toml
[[daemon.schedule]]
name = "feedreader-process-user2"
cron = "*/15 * * * *"
persona = "jarvis"
command = "feedreader process --user=2 --format=json"
format = "json"
timeout_sec = 600
origin = "user"
presence = "queue"
speak = "present"
```

## Command Output Format

Both commands output JSON conforming to Majordomo's CommandOutput schema:

### Fetch Feeds Output

```json
{
  "text": "Fetched 42 new articles from 15 feeds",
  "title": "Feed Update",
  "format": "text",
  "metadata": {
    "new_articles": "42",
    "feeds_fetched": "15",
    "errors": "0"
  }
}
```

### Process Output (With High-Interest Articles)

```json
{
  "text": "Found 2 high-interest article(s):\n\n- [Breaking: Major Tech Announcement](https://example.com/article1)\n  Tech company reveals new product...\n\n- [Important Security Update](https://example.com/article2)\n  Critical vulnerability discovered...\n",
  "title": "Feed Digest",
  "format": "markdown",
  "user": "1",
  "metadata": {
    "new_articles": "42",
    "processed": "42",
    "high_interest": "2"
  }
}
```

### Process Output (No High-Interest Articles)

When no high-interest articles are found, the `text` field is empty, which causes Majordomo to skip delivery (no notification):

```json
{
  "text": "",
  "title": "Feed Digest",
  "format": "markdown",
  "user": "1",
  "metadata": {
    "new_articles": "15",
    "processed": "15",
    "high_interest": "0"
  }
}
```

## Execution Flow

1. **Cron triggers** at the scheduled time (e.g., every 15 minutes)
2. **fetch-feeds runs first** - downloads all feeds, stores articles
3. **process commands run** (can be parallel for multiple users):
   - Generate AI summaries for new articles
   - Score articles for security and interest
   - Group related articles
   - Update group summaries
   - Output high-interest notifications
4. **Majordomo receives JSON output**:
   - Empty `text` → skip delivery (nothing to report)
   - Non-empty `text` → deliver to persona
5. **Persona processes notification**:
   - Jarvis speaks the notification if user is present
   - Otherwise, queues for later delivery

## Scheduling Strategy

### Recommended Approach

- **Fetch feeds**: Every 15-30 minutes (balance freshness vs. load)
- **Process per user**: Same schedule as fetch, or slightly offset
- **Stagger processing**: For many users, offset cron times to spread load

Example with staggered processing:

```toml
# Fetch once at :00
[[daemon.schedule]]
name = "feedreader-fetch"
cron = "0 * * * *"  # Top of every hour
persona = "jarvis"
command = "feedreader fetch-feeds"
format = "json"

# Process users with 5-minute offsets
[[daemon.schedule]]
name = "feedreader-process-user1"
cron = "5 * * * *"  # :05 past hour
persona = "jarvis"
command = "feedreader process --user=1 --format=json"
format = "json"

[[daemon.schedule]]
name = "feedreader-process-user2"
cron = "10 * * * *"  # :10 past hour
persona = "jarvis"
command = "feedreader process --user=2 --format=json"
format = "json"
```

## Configuration Files

Feedreader configuration (typically `~/.config/feedreader/config.yaml`):

```yaml
database:
  path: "~/.local/share/feedreader/feeds.db"

ollama:
  base_url: "http://localhost:11434"
  security_model: "gemma2:2b"
  curation_model: "llama3.2:3b"

ai:
  security_threshold: 7.0
  interest_threshold: 7.5
```

## User Management

### Adding Users

Users are created automatically when they subscribe to feeds:

```bash
# Import OPML for user 1
feedreader import --user=1 ~/feeds.opml

# Import OPML for user 2
feedreader import --user=2 ~/other-feeds.opml
```

### Per-User Settings

Each user can have different:
- Feed subscriptions (via `user_feeds` table)
- AI summaries (different models/prompts)
- Article groups (different categorization)
- Read state (independent tracking)

## Monitoring

### Check Command Output

Test commands manually to see JSON output:

```bash
# Test fetch
feedreader fetch-feeds --format=json

# Test processing for user 1
feedreader process --user=1 --format=json
```

### Check Majordomo Logs

Majordomo logs command execution and output. Check logs for:
- Command execution time
- JSON parsing errors
- Delivery decisions (speak vs. queue vs. skip)

### Database Inspection

Query the database to see processing status:

```bash
sqlite3 ~/.local/share/feedreader/feeds.db

-- Check article counts
SELECT COUNT(*) FROM articles;

-- Check per-user summaries
SELECT user_id, COUNT(*) FROM article_summaries GROUP BY user_id;

-- Check subscriptions
SELECT u.user_id, COUNT(uf.feed_id) as feed_count
FROM user_feeds uf
GROUP BY u.user_id;
```

## Troubleshooting

### No Notifications

1. **Check if articles are being fetched**:
   ```bash
   feedreader fetch-feeds --format=human
   ```

2. **Check if AI processing is working**:
   ```bash
   feedreader process --user=1 --format=human
   ```

3. **Verify interest thresholds** in config.yaml - may be too high

### Timeouts

If commands timeout:

1. **Increase timeout** in Majordomo config
2. **Reduce load** - subscribe to fewer feeds per user
3. **Check Ollama** - ensure models are loaded and responsive

### Duplicate Notifications

Ensure fetch and process schedules are coordinated:
- Fetch should run before or at the same time as process
- Don't run process more frequently than fetch

## Security Considerations

- **Command injection**: Majordomo runs commands via `sh -c`, but user input is not interpolated
- **Rate limiting**: Respect feed source rate limits - avoid aggressive polling
- **API keys**: Ollama runs locally, no external API keys needed
- **User isolation**: Each user's AI summaries and groups are isolated

## Performance Optimization

### For Many Users

- Use staggered cron schedules to spread AI processing
- Consider running `fetch-feeds` once, then `process --user=N` in parallel
- Monitor Ollama resource usage

### For Many Feeds

- Adjust fetch timeout based on feed count
- Consider running fetch more frequently than processing
- Use feed-level priorities (future enhancement)

## Example Voice Interaction

When high-interest articles are found:

```
Jarvis: "Sir, found 2 high-interest articles.
         Breaking: Major Tech Announcement - Tech company reveals new product.
         Important Security Update - Critical vulnerability discovered."
```

When no high-interest articles:
- (No voice notification - empty text field causes skip)
