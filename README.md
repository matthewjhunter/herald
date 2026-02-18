# Herald

[![CI](https://github.com/matthewjhunter/herald/actions/workflows/ci.yml/badge.svg)](https://github.com/matthewjhunter/herald/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/matthewjhunter/herald)](https://goreportcard.com/report/github.com/matthewjhunter/herald)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/matthewjhunter/herald)](go.mod)

AI-powered feed reader with security-first content screening and neutral interest curation.

## What It Does

Herald is an intelligent RSS/Atom reader that uses a two-model AI pipeline to filter and curate news. A security model (Gemma) screens content for prompt injection and adversarial manipulation before it ever reaches curation, while a separate model (Llama) scores articles by relevance — without imposing editorial bias. Related articles are automatically clustered using vector embeddings, and high-interest items can be delivered as voice notifications via [Majordomo](https://github.com/matthewjhunter/majordomo) integration. Herald runs in three modes: CLI for manual use, MCP server for AI persona integration, and a web interface for browsing.

## The Two-Model Approach

Most AI news tools either skip security entirely — leaving them vulnerable to poisoned feeds — or use a single model that conflates safety filtering with editorial judgment. Herald separates these concerns at the architectural level.

### Security Layer (Gemma)

Gemma screens every article before it reaches curation. It looks for prompt injection attempts, adversarial content designed to manipulate downstream AI systems, and other malicious patterns. The security check is conservative: when in doubt, it flags. Critically, it makes no judgment about whether content is interesting — only whether it is safe.

Articles that fail the security check are recorded with their score and reasoning but excluded from the curation pipeline entirely.

### Curation Layer (Llama)

Llama scores articles on news value, relevance, and alignment with user-defined keywords. It operates on content that has already been cleared by the security layer, so it has no reason to be defensive. The result is neutral relevance ranking — articles are scored on how interesting they are to you, not filtered based on content category or topic.

### Why This Matters

Security and editorial judgment are different problems that benefit from different model characteristics. Gemma was trained with strong safety guardrails, making it well-suited to threat detection. Llama provides neutral scoring without the conservative filtering bias that safety-trained models apply to content they find sensitive. Using one model for both tasks forces a tradeoff. Using two removes it.

## Key Features

- Two-model AI pipeline: security screening (Gemma) separated from interest curation (Llama)
- RSS 2.0 and Atom 1.0 support with OPML import
- Vector-based article clustering across sources using cosine similarity
- Per-user interest keywords, thresholds, and read state
- Customizable AI prompts with 3-tier fallback: database → config → embedded defaults
- Article summarization with per-user caching
- Conditional feed fetching (ETag / Last-Modified) to minimize bandwidth
- Majordomo voice notification integration for high-interest articles
- MCP server for AI persona access (26 tools)
- Web interface for browsing articles and groups
- Multi-user support: separate feeds, preferences, and read state per user
- Filter rules: score articles by author, category, or tag

## Architecture

```
RSS/Atom Feeds → Fetcher → Parser → SQLite
                                       |
                               Security Check (Gemma)
                                       |
                               Interest Scoring (Llama)
                                       |
                               Embedding + Clustering
                                       |
                         .-----------.-----------.
                        CLI      MCP Server    Web UI
```

See [docs/architecture.md](docs/architecture.md) for a detailed breakdown of each component.

## Binaries

| Binary | Purpose |
|--------|---------|
| `herald` | CLI for feed management, fetching, and reading |
| `herald-mcp` | MCP server for AI persona integration |
| `herald-web` | Read-only web interface for browsing articles |

## Getting Started

**Prerequisites**

- Go 1.25+
- [Ollama](https://ollama.com/) running locally with models pulled:
  ```bash
  ollama pull gemma3:4b
  ollama pull llama3
  ```
- Majordomo (optional, for voice notifications)

**Build**

```bash
go install ./cmd/herald ./cmd/herald-mcp ./cmd/herald-web
```

**Initialize configuration**

```bash
herald init-config
```

This creates `config/config.yaml`. Edit it to set your Ollama URL, model names, thresholds, and interest keywords.

**Import feeds**

```bash
herald import /path/to/subscriptions.opml
```

**Fetch and process**

```bash
herald fetch
```

This fetches all subscribed feeds, runs the security and curation pipeline on new articles, clusters related stories, and notifies Majordomo about high-interest items.

**Read articles**

```bash
herald list --limit 20 --format=human
herald list --cluster --format=human   # grouped by topic
```

**Automate with cron**

```cron
*/30 * * * * herald fetch >> ~/.local/log/herald.log 2>&1
```

## Configuration

Herald reads `config/config.yaml`. Key sections:

```yaml
ollama:
  base_url: http://localhost:11434
  security_model: gemma3:4b
  curation_model: llama3

thresholds:
  interest_score: 8.0    # articles above this score trigger notifications
  security_score: 7.0    # articles below this score are flagged unsafe

preferences:
  keywords:
    - security
    - AI
    - golang
```

AI prompts can be overridden in the config file or per-user in the database. See [docs/architecture.md](docs/architecture.md) for the full prompt system description.

## Majordomo Integration

Herald integrates with [Majordomo](https://github.com/matthewjhunter/majordomo) for voice notifications and AI persona access. The MCP server (`herald-mcp`) exposes 26 tools covering feed management, article reading, group browsing, preference management, and prompt customization.

See [docs/majordomo-integration.md](docs/majordomo-integration.md) for the complete integration guide and [examples/majordomo-config.toml](examples/majordomo-config.toml) for configuration templates.

## License

Apache 2.0 — see [LICENSE](LICENSE).
