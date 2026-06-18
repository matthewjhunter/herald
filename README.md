# Herald

[![CI](https://github.com/matthewjhunter/herald/actions/workflows/ci.yml/badge.svg)](https://github.com/matthewjhunter/herald/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/matthewjhunter/herald)](https://goreportcard.com/report/github.com/matthewjhunter/herald)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/matthewjhunter/herald)](go.mod)

AI-powered feed reader with security-first content screening and neutral interest curation.

## What It Does

Herald is an intelligent RSS/Atom reader that uses a two-model AI pipeline to filter and curate news. A security model (Gemma) screens content for prompt injection and adversarial manipulation before it ever reaches curation, while a separate model (Llama) scores articles by relevance — without imposing editorial bias. Related articles are automatically clustered using vector embeddings, and high-interest items are surfaced as formatted notification output. Herald runs in two modes: CLI for manual use and a web interface for browsing.

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
- RSS 2.0 and Atom 1.0 support with OPML import, export, and sync URL for RSS client integration
- Vector-based article clustering across sources using cosine similarity
- Per-user interest keywords, thresholds, and read state
- Customizable AI prompts with 3-tier fallback: database → config → embedded defaults
- Article summarization, cached once per article and shared by all users
- Conditional feed fetching (ETag / Last-Modified) to minimize bandwidth
- Formatted notification output for high-interest articles
- Web interface for browsing articles and groups
- Multi-user support: separate feeds, preferences, and read state per user
- Filter rules: score articles by author, category, or tag

## Architecture

```
RSS/Atom Feeds → Fetcher → Parser → PostgreSQL
                                       |
                               Security Check (Gemma)
                                       |
                               Interest Scoring (Llama)
                                       |
                               Embedding + Clustering
                                       |
                         .-----------------------.
                        CLI                    Web UI
```

See [docs/architecture.md](docs/architecture.md) for a detailed breakdown of each component.

## Binaries

| Binary | Purpose |
|--------|---------|
| `herald` | CLI for feed management, fetching, and reading |
| `herald serve` | Read-only web interface for browsing articles (subcommand) |

## Getting Started

**Prerequisites**

- Go 1.25+
- [Ollama](https://ollama.com/) running locally with models pulled:
  ```bash
  ollama pull gemma3:4b
  ollama pull llama3.1:8b
  ```
  See [Choosing models](#choosing-models) for sizing by available VRAM.

**Build**

```bash
go install ./cmd/herald
```

**Initialize configuration**

```bash
herald init-config
```

This creates `config/config.toml`. Edit it to set your Ollama URL, model names, thresholds, and interest keywords.

**Import feeds**

```bash
herald import /path/to/subscriptions.opml
```

**Fetch and process**

```bash
herald fetch
```

This fetches all subscribed feeds, runs the security and curation pipeline on new articles, clusters related stories, and emits notification output for high-interest items.

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

Herald reads `config/config.toml`. Key sections:

```toml
[ollama]
base_url = "http://localhost:11434"
security_model = "gemma3:4b"
curation_model = "llama3.1:8b"

[thresholds]
interest_score = 8.0    # articles above this score trigger notifications
security_score = 7.0    # articles below this score are flagged unsafe

[preferences]
keywords = ["security"]
    - AI
    - golang
```

AI prompts can be overridden in the config file or per-user in the database. See [docs/architecture.md](docs/architecture.md) for the full prompt system description.

## Choosing models

Herald runs two local models with separate jobs, so size them independently:

| Role | Config key | Runs on | What to optimize |
|------|-----------|---------|------------------|
| Security screening | `security_model` | Every fetched article, before curation | Small and fast -- it gates throughput. A 4B model is enough. |
| Curation / scoring | `curation_model` | Articles that pass screening | Judgment quality. Use the largest model your VRAM allows. |

Both can be resident at once, so budget for the **combined** size.

| VRAM | `security_model` | `curation_model` |
|------|------------------|------------------|
| 8-10 GB | `gemma3:4b` | `gemma3:4b` (reuse one model for both) |
| 12-16 GB | `gemma3:4b` | `llama3.1:8b` |
| 24 GB | `gemma3:4b` | `gemma3:12b` |
| 24 GB+ / multi-GPU | `gemma3:12b` | `gemma3:27b` |

Any Ollama chat model works -- these are starting points, not requirements. Pull your pick and set both keys:

```bash
ollama pull gemma3:4b
ollama pull llama3.1:8b
```

```yaml
ollama:
  security_model: gemma3:4b
  curation_model: llama3.1:8b
```

Google's **Gemma 4** family is newer and ships in several sizes -- compact `E2B` and `E4B` variants through larger `26B-A4B` and `31B` builds -- and is worth experimenting with for both roles as it lands in Ollama.

A discrete GPU is strongly recommended. CPU-only inference works but runs multiple seconds per article; GPUs with under ~6 GB are fine for embeddings but not for the screening and curation models.

> **Experimental:** large-context summarization (a separate, in-progress feature) pairs better with a long-context model such as `qwen3` -- noted here only for that use, not for routine screening or curation.

## License

Apache 2.0 — see [LICENSE](LICENSE).
