# FeedReader - Quick Start Guide

Get up and running in 5 minutes.

## Prerequisites Check

```bash
# Check Go version (need 1.21+)
go version

# Check if Ollama is running
curl http://localhost:11434/api/tags

# Check if required models are installed
ollama list | grep -E "(gemma2|llama3.2)"
```

## Installation

```bash
# 1. Build the application
task build
# or: make build
# or: go build -o feedreader ./cmd/feedreader

# 2. Initialize configuration
task init-config
# or: ./feedreader init-config

# 3. Import your feeds (optional - use test feeds for now)
task import OPML_FILE=test_feeds.opml
# or: ./feedreader import test_feeds.opml
```

## First Run

```bash
# Fetch feeds and process with AI
task fetch
# or: ./feedreader fetch

# List articles
task list
# or: ./feedreader list --limit 10

# Read an article (mark as read)
./feedreader read 1
```

## Expected Output

### Fetch Command
```
Fetched 50 new articles
Processing 50 unread articles...
📊 Processed: Article Title (interest: 7.5, security: 9.0)
...
Processed 50 articles

🔥 Found 3 high-interest articles (score >= 8.0)
```

### List Command
```
Unread articles (10):

ID: 1
Title: Breakthrough in Quantum Computing
URL: https://example.com/article
Published: 2026-02-17 14:30
---
```

## Common Issues

### Ollama Not Running
```bash
# Start Ollama
ollama serve

# Pull models
ollama pull gemma2
ollama pull llama3.2
```

### No Models Found
AI processing will be skipped, but feed fetching still works.

### Majordomo Not Available
Notifications will fall back to stdout.

## Next Steps

1. **Import your real feeds**: Export OPML from your current reader
2. **Tune thresholds**: Edit `config/config.yaml` to adjust sensitivity
3. **Set up cron**: Automate fetching (see INSTALL.md)
4. **Customize preferences**: Add keywords and preferred sources

## Key Commands

| Command | Purpose |
|---------|---------|
| `task build` | Build the application |
| `task test` | Run tests |
| `task fetch` | Fetch and process feeds |
| `task list` | List unread articles |
| `./feedreader read <id>` | Mark article as read |
| `./feedreader import <file>` | Import OPML |

## Configuration File

Location: `config/config.yaml`

Key settings:
- `thresholds.interest_score`: Notification threshold (default: 8.0)
- `thresholds.security_score`: Security threshold (default: 7.0)
- `preferences.keywords`: Topics of interest
- `ollama.security_model`: Security screening model (gemma2)
- `ollama.curation_model`: Interest scoring model (llama3.2)

## Full Documentation

- **README.md** - Architecture and overview
- **INSTALL.md** - Detailed installation
- **USAGE.md** - Complete usage guide
- **PROJECT_STATUS.md** - Development status

## Support

Check logs and error messages:
```bash
./feedreader fetch 2>&1 | tee fetch.log
```

Review database:
```bash
sqlite3 feedreader.db "SELECT * FROM feeds"
```

## Success Criteria

You're ready to go when:
- ✅ Build completes without errors
- ✅ Config file exists at `config/config.yaml`
- ✅ Feeds import successfully
- ✅ Articles fetch and appear in list
- ✅ Can mark articles as read

**Enjoy your intelligent feed reader!**
