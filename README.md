# Herald - Your AI-Powered News Herald

> *"Hear ye, hear ye! News of import, my lord."*

An intelligent RSS/Atom feed reader with AI-powered security screening and content curation. Herald monitors your feeds, filters for importance, and announces significant news via [Majordomo](https://github.com/matthewjhunter/majordomo) voice notifications.

Like a medieval herald who announces important news to their lord, Herald watches your RSS feeds and speaks only what matters.

**Key Features:**
- 🛡️ **AI Security Screening** - Gemma 2 detects malicious content and prompt injection
- 🎯 **Smart Curation** - Llama 3.2 scores articles for relevance and importance
- 🔊 **Voice Announcements** - Integrates with Majordomo for spoken notifications
- 📊 **Article Grouping** - Automatically clusters related stories from different sources
- 👥 **Multi-User** - Per-user feeds, summaries, and AI prompts
- 🎨 **Configurable Prompts** - Three-tier system: embedded defaults, config file, per-user database

## Features

- **RSS & Atom Support**: Parse and fetch feeds in both RSS 2.0 and Atom 1.0 formats
- **OPML Import**: Import your existing feed subscriptions from OPML files
- **AI Security Layer**: Use Gemma 2 to detect malicious content and prompt injection attacks
- **AI Curation**: Use Llama 3.2 to score articles by interest and relevance
- **Article Clustering**: Automatically group articles from different sources covering the same event
- **Multiple Output Formats**: JSON (default), tab-delimited text, or human-readable
- **Stdout/Stderr Separation**: Clean output for piping and automation
- **SQLite Storage**: Lightweight, file-based database with full read state tracking
- **CLI Interface**: Simple command-line interface designed for cron automation

## Architecture

### Security & Curation Pipeline

1. **Fetch**: Download RSS/Atom feeds from subscribed sources
2. **Security Check**: Gemma 2 analyzes content for threats (conservative filtering)
3. **Curation**: Llama 3.2 scores articles for interest (neutral scoring)
4. **Notification**: High-scoring articles trigger Majordomo voice notifications

### Two-Model Approach

- **Security Model (Gemma 2)**: Leverages Google's safety training for threat detection
- **Curation Model (Llama 3.2)**: Neutral model for unbiased interest scoring

This separation ensures security without sacrificing editorial neutrality.

## Installation

### Prerequisites

- Go 1.21+
- Ollama with models installed:
  - `gemma2` (security layer)
  - `llama3.2` (curation layer)
- Majordomo (optional, for voice notifications)

### Build

```bash
cd herald
go build -o herald ./cmd/herald
```

## Output Formats

FeedReader supports three output formats via the `--format` flag:

### JSON (default) - Machine Parseable
```bash
./herald list --format=json | jq '.[].Title'
./herald fetch --format=json > result.json
```

### Text - Tab-Delimited
```bash
./herald list --format=text | awk -F'\t' '{print $2}'
./herald fetch --format=text | grep "processed="
```

### Human - Formatted for Reading
```bash
./herald list --format=human | less
./herald fetch --format=human
```

All errors and warnings go to stderr, keeping stdout clean for piping.

## Article Clustering

Group articles covering the same event from different sources:

```bash
./herald list --cluster --format=human
```

The clustering feature uses Llama 3.2 to analyze article titles and detect related stories, providing topic summaries and grouping duplicate coverage.

## Usage

### 1. Initialize Configuration

```bash
./herald init-config
```

This creates `config/config.yaml` with default settings. Edit as needed:

```yaml
database:
  path: ./herald.db

ollama:
  base_url: http://localhost:11434
  security_model: gemma2
  curation_model: llama3.2

majordomo:
  enabled: true
  chat_command: majordomo
  target_persona: jarvis

thresholds:
  interest_score: 8.0
  security_score: 7.0

preferences:
  keywords:
    - technology
    - security
    - AI
  preferred_sources: []
```

### 2. Import Feeds from OPML

```bash
./herald import /path/to/subscriptions.opml
```

### 3. Fetch and Process Feeds

```bash
./herald fetch
```

This command:
- Fetches all enabled feeds
- Stores new articles
- Runs security checks (Gemma 2)
- Scores articles for interest (Llama 3.2)
- Notifies Majordomo about high-interest articles

### 4. List Unread Articles

```bash
./herald list --limit 20
```

### 5. Mark Article as Read

```bash
./herald read <article-id>
```

## Automation with Cron

Add to your crontab to fetch feeds every 30 minutes:

```cron
*/30 * * * * cd /path/to/herald && ./herald fetch >> fetch.log 2>&1
```

Or use Majordomo cron:

```bash
majordomo cron add "*/30 * * * *" "cd /path/to/herald && ./herald fetch"
```

## Database Schema

### Tables

- **feeds**: Subscription list with URLs and metadata
- **articles**: Feed items with content and timestamps
- **read_state**: Read status, interest scores, security scores
- **user_preferences**: Future multi-user support

## AI Model Selection

### Why Gemma 2 for Security?

Gemma 2 was trained with strong safety guardrails, making it excellent at:
- Detecting prompt injection attempts
- Identifying malicious content
- Conservative security decisions

### Why Llama 3.2 for Curation?

Llama 3.2 provides:
- Neutral interest scoring without content filtering
- Good reasoning about relevance and news value
- Balance between speed and quality

## Majordomo Integration

Feedreader integrates with [Majordomo](https://github.com/matthewjhunter/majordomo) for automated voice notifications of high-interest articles. The `process` command outputs JSON conforming to Majordomo's CommandOutput schema.

### Quick Setup

1. **Configure herald** (see Configuration above)
2. **Import feeds** for each user:
   ```bash
   herald import --user=1 ~/feeds.opml
   ```
3. **Add to Majordomo config** (`~/.config/majordomo/config.toml`):
   ```toml
   # Fetch feeds once
   [[daemon.schedule]]
   name = "herald-fetch"
   cron = "*/15 * * * *"
   persona = "jarvis"
   command = "herald fetch-feeds --format=json"
   format = "json"

   # Process per user
   [[daemon.schedule]]
   name = "herald-process-user1"
   cron = "*/15 * * * *"
   persona = "jarvis"
   command = "herald process --user=1 --format=json"
   format = "json"
   ```

See [docs/majordomo-integration.md](docs/majordomo-integration.md) for complete integration guide and [examples/majordomo-config.toml](examples/majordomo-config.toml) for configuration templates.

### Output Format
```
╔════════════════════════════════════════════════════════════════
║ 🔔 MAJORDOMO NOTIFICATION → jarvis
╠════════════════════════════════════════════════════════════════
📰 High Interest Article (8.5/10)

Title: Breaking News
URL: https://example.com/article
Summary: ...
╚════════════════════════════════════════════════════════════════
```

## Troubleshooting

### Ollama Connection Issues

Verify Ollama is running:

```bash
curl http://localhost:11434/api/tags
```

### Missing Models

Pull required models:

```bash
ollama pull gemma2
ollama pull llama3.2
```

### Database Location

Default database: `./herald.db`

To use a different location, edit `config/config.yaml`:

```yaml
database:
  path: /path/to/custom/herald.db
```

## Future Enhancements

- **Learning Mechanism**: Track reading patterns to improve curation
- **Multi-user Support**: Separate preferences and read states
- **Web Interface**: Browser-based feed reader with backend API
- **Full-text Extraction**: Fetch complete article content from web pages

## License

MIT License

## Contributing

Contributions welcome! Please open an issue or PR on GitHub.
