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
ollama pull gemma3:4b
ollama pull llama3.1:8b

# Verify Ollama is running
curl http://localhost:11434/api/tags
```

## Building FeedReader

```bash
# Clone or navigate to the herald directory
cd herald

# Build the binary
make build

# Or manually:
go build -o herald ./cmd/herald
```

## Configuration

### 1. Create Config File

```bash
# Initialize default config
./herald init-config

# Or copy the example
cp config/config.toml.example config/config.toml
```

### 2. Customize Configuration

Edit `config/config.toml`:

```yaml
database:
  # PostgreSQL DSN (herald is Postgres-only)
  path: postgres://localhost:5432/herald?sslmode=disable

ollama:
  base_url: http://localhost:11434
  security_model: gemma3:4b    # For threat detection
  curation_model: llama3.1:8b  # For interest scoring

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
./herald import /path/to/subscriptions.opml
```

## Set Up Automatic Fetching

### Option 1: System Cron

Edit your crontab:

```bash
crontab -e
```

Add this line to fetch every 30 minutes:

```cron
*/30 * * * * cd /path/to/herald && ./herald fetch >> logs/fetch.log 2>&1
```

### Option 3: Systemd Timer

Create `/etc/systemd/system/herald-fetch.service`:

```ini
[Unit]
Description=FeedReader Fetch
After=network.target

[Service]
Type=oneshot
User=your-username
WorkingDirectory=/path/to/herald
ExecStart=/path/to/herald/herald fetch
```

Create `/etc/systemd/system/herald-fetch.timer`:

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
sudo systemctl enable herald-fetch.timer
sudo systemctl start herald-fetch.timer

# Check status
systemctl status herald-fetch.timer
```

## Verify Installation

### 1. Manual Fetch

```bash
./herald fetch
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
./herald list --limit 10
```

### 3. Mark as Read

```bash
./herald read 1
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
ollama pull gemma3:4b
ollama pull llama3.1:8b

# List installed models
ollama list
```

### Database Issues

```bash
# Check the database is reachable
psql "$HERALD_DB_DSN" -c '\dt'

# Re-fetch
./herald fetch
```

### No Articles Fetched

Check feed URLs are accessible:

```bash
# Test a feed URL
curl -I https://hnrss.org/frontpage
```

## Performance Tuning

### Reduce AI Processing Load

Edit `config/config.toml`:

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
psql "$HERALD_DB_DSN" -c "DELETE FROM articles WHERE id IN (SELECT article_id FROM read_state WHERE read AND read_date < now() - interval '30 days')"
```

## Uninstall

```bash
# Remove binary
make clean

# Remove database
dropdb herald   # or: psql -c 'DROP DATABASE herald'

# Remove config
rm -rf config/

# Remove cron job
crontab -e  # Delete the herald line
```
