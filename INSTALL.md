# Installation Guide

## Prerequisites

### 1. Install Go

FeedReader requires Go 1.21 or later.

```bash
# Check if Go is installed
go version

# If not installed, download from https://go.dev/dl/
```

### 2. Install Ollama

```bash
# Install Ollama
curl -fsSL https://ollama.com/install.sh | sh

# Pull required models
ollama pull gemma2
ollama pull llama3.2

# Verify Ollama is running
curl http://localhost:11434/api/tags
```

### 3. Install Majordomo (Optional)

If you want voice notifications:

```bash
# Follow Majordomo installation instructions
# https://github.com/your-majordomo-repo
```

## Building FeedReader

```bash
# Clone or navigate to the feedreader directory
cd feedreader

# Build the binary
make build

# Or manually:
go build -o feedreader ./cmd/feedreader
```

## Configuration

### 1. Create Config File

```bash
# Initialize default config
./feedreader init-config

# Or copy the example
cp config/config.yaml.example config/config.yaml
```

### 2. Customize Configuration

Edit `config/config.yaml`:

```yaml
database:
  path: ./feedreader.db  # Database location

ollama:
  base_url: http://localhost:11434
  security_model: gemma2      # For threat detection
  curation_model: llama3.2    # For interest scoring

majordomo:
  enabled: true               # Enable/disable notifications
  chat_command: majordomo
  target_persona: jarvis

thresholds:
  interest_score: 8.0         # Notify for articles scoring >= 8
  security_score: 7.0         # Reject articles scoring < 7

preferences:
  keywords:                   # Topics of interest
    - technology
    - security
    - AI
  preferred_sources: []       # Preferred feed URLs
```

## Import Your Feeds

### Export from Your Current Reader

Most RSS readers support OPML export:

- **Feedly**: Settings → OPML
- **Inoreader**: Settings → Import/Export → Export to OPML file
- **NewsBlur**: Account → Import/Export → Export
- **The Old Reader**: Settings → Import/Export → Export

### Import into FeedReader

```bash
./feedreader import /path/to/subscriptions.opml
```

## Set Up Automatic Fetching

### Option 1: System Cron

Edit your crontab:

```bash
crontab -e
```

Add this line to fetch every 30 minutes:

```cron
*/30 * * * * cd /path/to/feedreader && ./feedreader fetch >> logs/fetch.log 2>&1
```

### Option 2: Majordomo Cron

If you're using Majordomo:

```bash
majordomo cron add "*/30 * * * *" "cd /path/to/feedreader && ./feedreader fetch"
```

### Option 3: Systemd Timer

Create `/etc/systemd/system/feedreader-fetch.service`:

```ini
[Unit]
Description=FeedReader Fetch
After=network.target

[Service]
Type=oneshot
User=your-username
WorkingDirectory=/path/to/feedreader
ExecStart=/path/to/feedreader/feedreader fetch
```

Create `/etc/systemd/system/feedreader-fetch.timer`:

```ini
[Unit]
Description=Run FeedReader every 30 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=30min

[Install]
WantedBy=timers.target
```

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable feedreader-fetch.timer
sudo systemctl start feedreader-fetch.timer

# Check status
systemctl status feedreader-fetch.timer
```

## Verify Installation

### 1. Manual Fetch

```bash
./feedreader fetch
```

You should see output like:

```
Fetched 50 new articles
Processing 50 unread articles...
📊 Processed: Article Title (interest: 7.5, security: 9.0)
...
Processed 50 articles

🔥 Found 3 high-interest articles (score >= 8.0)
```

### 2. List Articles

```bash
./feedreader list --limit 10
```

### 3. Mark as Read

```bash
./feedreader read 1
```

## Troubleshooting

### Ollama Not Running

```bash
# Check if Ollama is running
curl http://localhost:11434/api/tags

# If not, start it
ollama serve
```

### Models Not Found

```bash
# Pull required models
ollama pull gemma2
ollama pull llama3.2

# List installed models
ollama list
```

### Database Issues

```bash
# Check database file exists and is readable
ls -lh feedreader.db

# If corrupted, remove and re-fetch
rm feedreader.db
./feedreader fetch
```

### No Articles Fetched

Check feed URLs are accessible:

```bash
# Test a feed URL
curl -I https://hnrss.org/frontpage
```

### Majordomo Notifications Not Working

```bash
# Test majordomo command
majordomo chat --recipients jarvis --text "Test notification"

# If fails, check majordomo installation
which majordomo
majordomo --version
```

## Performance Tuning

### Reduce AI Processing Load

Edit `config/config.yaml`:

```yaml
thresholds:
  security_score: 6.0    # More permissive (faster)
  interest_score: 9.0    # Only notify for very high scores
```

### Fetch Frequency

Balance timeliness vs. resource usage:

- **High frequency** (every 15 min): More current, higher load
- **Medium frequency** (every 30 min): Balanced (recommended)
- **Low frequency** (every 2 hours): Lighter load, less current

### Database Maintenance

Periodically clean old read articles:

```bash
sqlite3 feedreader.db "DELETE FROM articles WHERE id IN (SELECT article_id FROM read_state WHERE read = 1 AND read_date < datetime('now', '-30 days'))"
```

## Uninstall

```bash
# Remove binary
make clean

# Remove database
rm -f feedreader.db

# Remove config
rm -rf config/

# Remove cron job
crontab -e  # Delete the feedreader line
```
