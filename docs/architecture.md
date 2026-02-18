# Herald Architecture

## Overview

Herald is designed around three principles:

- **Security first.** Feed content is untrusted input. Before any AI system processes an article for relevance, a dedicated security model screens it for prompt injection and adversarial manipulation. Nothing reaches the curation layer unless it passes.
- **Editorial neutrality.** Interest scoring is separate from safety filtering. The curation model scores articles on relevance to the user's interests without applying content-category restrictions.
- **Local inference only.** All AI processing uses Ollama running locally. There are no cloud LLM dependencies, no API costs, and no feed content leaves the machine.

## System Components

```
┌─────────────────────────────────────────────────────────────────┐
│                           Engine                                 │
│  ┌──────────┐  ┌──────────────┐  ┌────────────┐  ┌──────────┐  │
│  │ Fetcher  │  │ AIProcessor  │  │GroupMatcher│  │  Store   │  │
│  │          │  │              │  │            │  │ (SQLite) │  │
│  │ - HTTP   │  │ - Security   │  │ - Embed    │  │          │  │
│  │ - OPML   │  │ - Curation   │  │ - Cosine   │  │ - Feeds  │  │
│  │ - ETag   │  │ - Summary    │  │ - Centroid │  │ - State  │  │
│  └──────────┘  └──────────────┘  └────────────┘  └──────────┘  │
└───────────────────────────────┬─────────────────────────────────┘
                                │
              ┌─────────────────┼─────────────────┐
              │                 │                  │
         ┌────┴────┐    ┌───────┴──────┐   ┌──────┴──────┐
         │   CLI   │    │  MCP Server  │   │   Web UI    │
         │ herald  │    │ herald-mcp   │   │ herald-web  │
         └─────────┘    └──────────────┘   └─────────────┘
```

### Fetcher (`internal/feeds`)

Fetches RSS 2.0 and Atom 1.0 feeds over HTTP. Sends `If-None-Match` and `If-Modified-Since` headers on each request, storing ETag and Last-Modified values from responses. A 304 reply skips parsing entirely. Parses feeds via `gofeed`, stores articles with their authors and categories, and imports subscriptions from OPML files (including nested folder structures).

### AIProcessor (`internal/ai`)

Drives all Ollama inference. Three distinct operations:

- **Security check** — sends article title and content to the security model (Gemma), parses a JSON response with `safe`, `score`, and `reasoning` fields. On parse failure, defaults to unsafe.
- **Curation** — sends title, content, and user keywords to the curation model (Llama), parses `interest_score` and `reasoning`. On parse failure, returns a neutral score of 5.0.
- **Summarization** — generates an AI summary for individual articles, and coherent narrative summaries for article groups. Summaries are per-user and cached in the database.

All prompts are loaded through the `PromptLoader` (see Prompt System below).

### GroupMatcher (`internal/ai`)

Performs incremental vector-based article-to-group matching. Uses a local embedding model via `go-embedding` to embed article text (title + AI summary), then computes cosine similarity against stored group centroids. Groups with no prior centroid are seeded directly from the first article's embedding.

### Store (`internal/storage`)

SQLite-backed persistence via `modernc.org/sqlite` (pure Go, no CGO). The `Store` interface abstracts all database operations. `SQLiteStore` is the production implementation.

### Output Formatters (`internal/output`)

Three output modes: JSON (default, machine-parseable), tab-delimited text (for shell pipelines), and human-readable formatted output. Errors go to stderr; article content goes to stdout.

## AI Pipeline

When `herald fetch` runs, new articles go through this sequence:

```
New Article
    │
    ▼
Security Check (Gemma)
    │
    ├─ unsafe (score < threshold) ──► stored with safe=false, skipped
    │
    ▼
Interest Scoring (Llama)
    │
    ├─ low score ──► stored, not notified
    │
    ▼
Summarization (Llama)
    │
    ▼
Group Matching (embedding + cosine similarity)
    │
    ├─ match found ──► added to existing group, centroid updated
    │
    └─ no match ──► new group created
    │
    ▼ (if interest score >= threshold)
Majordomo Notification
```

### Security Screening

The security model receives article title and truncated content (2000 chars). The prompt instructs it to detect prompt injection, adversarial content intended to manipulate AI systems, and other malicious patterns. Temperature is 0.3 by default for consistent, conservative decisions.

The security model's purpose is purely protective — it does not score relevance or filter by topic. An article about a controversial subject is not inherently unsafe. Only content that appears to be attempting to manipulate downstream AI processing is flagged.

The security prompt is not user-customizable via the MCP tools. This is intentional: allowing a persona to modify the security prompt would create an obvious prompt injection vector.

### Interest Curation

The curation model receives title, truncated content, and the user's interest keywords. It returns a score from 0–10 and reasoning. Temperature is 0.5 by default, allowing some variability in borderline cases.

Keywords are incorporated into the prompt as preferences, not as hard filters. An article that scores well on general news value can still rank highly even if it matches no keywords; a keyword match boosts the score but does not guarantee a high result. This keeps the ranking system responsive to editorial judgment rather than pure keyword counting.

### Summarization

Summaries are generated by the curation model (Llama) on demand. Individual article summaries use the article's title and up to 3000 chars of content. Group summaries synthesize the AI summaries of all member articles into a coherent narrative with a refined topic label.

Summaries are stored per-user in `article_summaries`. Once generated, they are not regenerated unless explicitly reset.

### Why Two Models

A single model handling both security and curation creates a tension: safety-trained models tend to apply conservative content filtering beyond what security requires, which introduces editorial bias into the relevance scores. Separating the roles lets each model operate in the domain it was trained for. Gemma's safety training is an asset for threat detection; Llama's neutrality is an asset for relevance scoring. The boundary between them is also an architectural firewall — the curation model never sees content the security model rejected.

## Article Clustering

Herald clusters articles covering the same event using vector embeddings rather than LLM-based grouping for batch article lists, and uses the `GroupMatcher` for incremental per-article matching during the fetch pipeline.

### Embedding

Article text (title + AI summary) is embedded using a local model via the `go-embedding` library. Embeddings are stored as binary-encoded float32 slices in the `article_groups` table alongside the group centroid.

### Incremental Centroid Update

When an article is assigned to a group, the group's centroid is updated incrementally without recomputing from scratch:

```
C_new = (C_old * N + V_new) / (N + 1)
```

where `C_old` is the current centroid, `N` is the article count before adding this one, and `V_new` is the new article's embedding. This keeps clustering O(1) per article rather than O(n) across all group members.

### Matching Threshold

A configurable similarity threshold (cosine similarity, 0–1) controls how aggressively articles are merged into existing groups. If no group centroid exceeds the threshold, a new group is created. Group topics are refined when a group reaches 3+ articles.

### LLM-Based Batch Clustering

The `ClusterArticles` method provides an alternative clustering path for batch list operations, asking the curation model to group a set of articles by topic. This is used by `herald list --cluster` for ad-hoc grouping of displayed results, separate from the persistent group state maintained during fetch.

## Storage

SQLite schema managed by `internal/storage/schema.go`. Key tables:

| Table | Purpose |
|-------|---------|
| `feeds` | Feed subscriptions with URL, title, ETag, Last-Modified, and error state |
| `articles` | Article content: title, URL, content, summary, author, published date |
| `article_authors` | Normalized author records per article (supports multi-author) |
| `article_categories` | Normalized category tags per article |
| `read_state` | Per-user read flag, starred flag, interest score, security score |
| `user_preferences` | Key-value preference store per user (keywords, thresholds, notification settings) |
| `user_feeds` | Many-to-many subscription mapping between users and feeds |
| `article_summaries` | Cached AI summaries per user per article |
| `article_groups` | Topic clusters with centroid embeddings |
| `article_group_members` | Many-to-many membership between groups and articles |
| `group_summaries` | Cached group narrative summaries with max interest score |
| `user_prompts` | Per-user custom prompt templates and temperatures |
| `filter_rules` | Scoring rules by author, category, or tag (positive or negative) |
| `users` | Registered users for multi-user deployments |

Feeds are shared across users; `user_feeds` tracks subscriptions. Articles are stored once; `read_state` tracks per-user scores and read status. Summaries are per-user because different users may have different summarization prompts.

## Prompt System

Herald uses Go `text/template` for all AI prompts, with a 3-tier fallback:

```
Tier 1 (lowest priority): embedded defaults — compiled into the binary via go:embed
Tier 2: config file — prompts.* fields in config.yaml
Tier 3 (highest priority): user database — per-user custom templates in user_prompts
```

The five prompt types are:

| Type | Model | Purpose |
|------|-------|---------|
| `security` | Gemma | Threat detection and prompt injection screening |
| `curation` | Llama | Interest scoring with user keywords |
| `summarization` | Llama | Single-article AI summary generation |
| `group_summary` | Llama | Multi-article narrative synthesis |
| `related_groups` | Llama | Determining if an article belongs to an existing group |

Each prompt type also has a configurable temperature following the same 3-tier fallback.

The `security` prompt type is intentionally excluded from MCP access — it cannot be viewed or modified through the MCP tools.

## MCP Integration

`herald-mcp` exposes 26 tools over stdio using the [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk). All tools accept an optional `speaker` parameter that resolves to a registered user ID, enabling multi-user access from a single MCP server instance.

Tool categories:

| Category | Tools |
|----------|-------|
| Articles | `articles_unread`, `articles_get`, `articles_mark_read`, `article_star` |
| Feeds | `feeds_list`, `feed_subscribe`, `feed_unsubscribe`, `feed_rename`, `feed_stats`, `feed_metadata` |
| Groups | `article_groups`, `article_group_get` |
| Polling | `poll_now` (requires `--poll` flag) |
| Preferences | `preferences_get`, `preference_set` |
| Prompts | `prompts_list`, `prompt_get`, `prompt_set`, `prompt_reset` |
| Filter rules | `filter_rules_list`, `filter_rule_add`, `filter_rule_update`, `filter_rule_delete` |
| Users | `user_register`, `user_list` |
| Briefing | `briefing` |

The `briefing` tool generates a formatted markdown digest of high-interest unread articles, intended for delivery as a voice briefing through Majordomo.

When started with `--poll`, the server runs a background polling loop at a configurable interval. The `poll_now` tool triggers an immediate poll cycle.

See [docs/majordomo-integration.md](majordomo-integration.md) for Majordomo-specific setup.

## Design Decisions

### Two-Model Separation

Security and editorial judgment are fundamentally different problems. Conflating them in a single model forces every relevance scoring decision to also carry safety filtering weight, which introduces systematic bias into interest scores. Treating the security boundary as an architectural boundary — a distinct model with a distinct purpose — keeps the two concerns independent and separately tunable.

### Ollama for Local Inference

All AI inference runs through Ollama on localhost. This means feed content never leaves the machine, there are no per-token API costs, and the system works offline. Model selection is configurable; users can swap Gemma or Llama for any Ollama-compatible model by changing two config values.

### Vector Clustering over LLM-Based Grouping

Persistent article grouping uses vector embeddings and cosine similarity rather than asking an LLM to group articles. The incremental centroid update formula keeps the cost of assigning each article to a group constant regardless of group size. LLM-based grouping is available for ad-hoc batch clustering (`herald list --cluster`) but is not used for the persistent group state, where it would require re-running the LLM over all articles on each fetch.

### Config-Driven AI Prompts

Prompts are treated as configuration, not code. Users can customize every prompt type (except security) through the MCP tools or config file without modifying source code. The 3-tier fallback ensures embedded defaults always work out of the box, config-file overrides apply globally, and per-user database overrides allow individual customization in multi-user deployments.
