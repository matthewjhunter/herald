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
│  │          │  │              │  │            │  │ (Postgres)│  │
│  │ - HTTP   │  │ - Security   │  │ - Embed    │  │          │  │
│  │ - OPML   │  │ - Curation   │  │ - Cosine   │  │ - Feeds  │  │
│  │ - ETag   │  │ - Summary    │  │ - Centroid │  │ - State  │  │
│  └──────────┘  └──────────────┘  └────────────┘  └──────────┘  │
└───────────────────────────────┬─────────────────────────────────┘
                                │
                    ┌─────────────┴─────────────┐
                    │                           │
               ┌────┴────┐                ┌─────┴───────┐
               │   CLI   │                │   Web UI    │
               │ herald (daemon + serve) │                │ (unified)  │
               └─────────┘                └─────────────┘
```

### Fetcher (`internal/feeds`)

Fetches RSS 2.0 and Atom 1.0 feeds over HTTP. Sends `If-None-Match` and `If-Modified-Since` headers on each request, storing ETag and Last-Modified values from responses. A 304 reply skips parsing entirely. Parses feeds via `gofeed`, stores articles with their authors and categories, and imports subscriptions from OPML files (including nested folder structures).

### AIProcessor (`internal/ai`)

Drives all Ollama inference. Three distinct operations:

- **Security check** — renders airlock's prompt-injection screening prompt (via `airlock/screen`, with Herald's feed-specific carve-outs), sends the fenced article to the security model (Gemma), and parses the reply with `screen.ParseVerdict` into a threat score (0 = clean, higher = worse), a category, and a cited evidence span. The model's citation is re-verified against the article -- a quote that is not present is a fabricated verdict and is rejected. A deterministic regex prescreen (`airlock/detect`) runs on the same content; the stored threat is the max of the two. On parse failure, the screen fails closed (retryable), never "no threat".
- **Curation** — sends title, content, and user keywords to the curation model (Llama), parses `interest_score` and `reasoning`. On parse failure, returns a neutral score of 5.0.
- **Summarization** — generates an AI summary for individual articles, and coherent narrative summaries for article groups. Summaries are per-user and cached in the database.

All prompts are loaded through the `PromptLoader` (see Prompt System below).

### GroupMatcher (`internal/ai`)

Embeds article text (title + AI summary) with a local model via `go-embedding`. The match itself -- nearest group centroid within the threshold -- runs as a pgvector distance query in the database (#186), not a cosine loop in the app.

### Store (`internal/storage`)

PostgreSQL-backed persistence via the `jackc/pgx/v5` driver (through `database/sql`). The `Store` interface abstracts all database operations; `PostgresStore` is the only implementation (herald is Postgres-only).

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
    ├─ unsafe (threat > threshold) ──► stored, excluded from downstream
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
Notification Output
```

### Security Screening

The security model receives article title and truncated content (2000 chars). The prompt instructs it to detect prompt injection, adversarial content intended to manipulate AI systems, and other malicious patterns. Temperature is 0.3 by default for consistent, conservative decisions.

The security model's purpose is purely protective — it does not score relevance or filter by topic. An article about a controversial subject is not inherently unsafe. Only content that appears to be attempting to manipulate downstream AI processing is flagged.

The security prompt is not *user*-customizable: allowing a regular user to modify it would create an obvious prompt injection vector. An operator can still override the global default (via the `user_id=0` admin prompt) to hot-patch screening without a redeploy -- the default is airlock's screening prompt, and an override that stops producing airlock's JSON verdict fails the parse (and the screen) closed rather than opening to "no threat".

### Interest Curation

The curation model receives title, truncated content, and the user's interest keywords. It returns a score from 0–10 and reasoning. Temperature is 0.5 by default, allowing some variability in borderline cases.

Keywords are incorporated into the prompt as preferences, not as hard filters. An article that scores well on general news value can still rank highly even if it matches no keywords; a keyword match boosts the score but does not guarantee a high result. This keeps the ranking system responsive to editorial judgment rather than pure keyword counting.

### Filter rules

Filter rules (`filter_rules`) apply *after* the model, at read time. An article's effective interest score is the model's score plus the sum of the user's matching rules, clamped to 0-10. It drives the ranked list, digest and newsletter selection, notifications, and the Fever sync API.

A rule names an **axis** and a **match mode**. Three axes match the labels a feed publishes -- `author`, `category`, `tag` -- and three match the article's own text: `title`, `summary`, `content`. The mode is `exact`, `substring` or `regex`, defaulting to `exact`, which is what every rule written before #274 was.

Read time, not scoring time. A rule therefore takes effect on articles that were scored long before it existed, with nothing re-run -- which is what a reader expects when they add one. The cost is that the stored `read_state.interest_score` stays the model's raw opinion, and the adjustment is recomputed per request. That split is deliberate: it keeps "what the model said" separately answerable from "what the reader saw", which is what makes the feedback corpus usable for evaluating the model.

#### Two evaluators, never at once

Where a rule is evaluated follows from its (axis, mode) pair:

| | `exact` | `substring` / `regex` |
|---|---|---|
| `author` / `category` / `tag` | SQL | Go |
| `title` / `summary` / `content` | Go | Go |

The SQL half is `filterRuleMatch` in `internal/storage/filterscore.go`: an indexed CITEXT equality inside a LATERAL join, which costs essentially nothing. The Go half is `internal/filtermatch`, evaluated in the engine against rows the query has already returned.

Go rather than Postgres for patterns because article text is attacker-supplied and Postgres regex backtracks: a pattern that is merely careless, run over content a feed controls, is a wedged page load. Go's `regexp` is RE2, linear in the input with no catastrophic backtracking.

**A user's rules are evaluated entirely in one place or entirely in the other.** A user with no pattern rules -- the common case -- takes the SQL path unchanged, with no extra queries. The moment one rule needs Go, Go takes all of them, including the exact ones SQL could have matched, and the SQL rule join and gate are switched off. The visibility gate forces this: it compares the *sum* of a user's matching rules against a threshold, and a rule set split between two evaluators leaves neither holding the number being compared. `filtermatch.New` encodes the choice by returning a nil matcher when SQL suffices.

Two consequences worth knowing. On the Go path the score arithmetic leaves SQL as well: the store returns raw interest scores (`GetScoredUnreadArticles`) because the ranking query's number is clamped and clamping is not invertible, and the engine adds, clamps, decays and sorts. And the scan is bounded (`filter_overfetch_factor` sizes each window, `filter_max_scan` caps the total): it walks forward a window at a time until the page has enough survivors or the budget runs out, so a page can still come back short, and an article that a rule *boosts* hard can fall outside the ranking window -- demotion, which is what nearly every rule does, is unaffected. Batch reading (#277) is where page composition gets designed properly.

Pattern rules carry their own quotas, well below the general one, because each is evaluated against every candidate row: `max_pattern_filter_rules_per_user` (50) and `max_content_filter_rules_per_user` (5). Content rules scan whole article bodies and measure at roughly 2ms per rule per 50-article page, against about 30us for a title rule.

Rules also feed a separate, opt-in **visibility gate**: when `filter_threshold` is non-zero, an article whose rule total falls below it is hidden entirely. The gate compares the rule total alone; the score adjustment compares model score plus rules. Two different numbers, and the settings page names both.

Unread counts do not apply the gate and never have, so a filtered list can show fewer articles than the badge promises. Rather than make the counts exact -- which would mean evaluating every unread article on every page load -- the reader gauge reports the gap as its own figure: "12 unread, 7 hidden". Approximate, bounded by the same scan window, and honest about it.

Three known inconsistencies, all deliberate. A group's `max_interest_score` is computed at cluster time from raw scores and cached, so a group header can disagree with the articles inside it. The score-distribution stats stay raw as well, because they measure model calibration -- folding reader policy in would make a badly-calibrated prompt indistinguishable from an aggressive rule. And a pattern rule on the `author` axis matches both the normalized `article_authors` names and the item's free-text `articles.author`, because feeds disagree about where the byline goes, while an exact rule -- matched in SQL -- sees only the normalized table.

None of this is the **security screen**, which is sitewide: its regex layer comes from `airlock/detect`, applies to every user identically, and is stored on the article because the verdict is a property of the article. Filter rules are per-user taste, owned by their creator, and no filter rule can move an article's threat score or change what another user sees.

### Summarization

Summaries are generated by the curation model (Llama) on demand. Individual article summaries use the article's title and up to 3000 chars of content. Group summaries synthesize the AI summaries of all member articles into a coherent narrative with a refined topic label.

Summaries are stored per-user in `article_summaries`. Once generated, they are not regenerated unless explicitly reset.

### Why Two Models

A single model handling both security and curation creates a tension: safety-trained models tend to apply conservative content filtering beyond what security requires, which introduces editorial bias into the relevance scores. Separating the roles lets each model operate in the domain it was trained for. Gemma's safety training is an asset for threat detection; Llama's neutrality is an asset for relevance scoring. The boundary between them is also an architectural firewall — the curation model never sees content the security model rejected.

## Article Clustering

Herald clusters articles covering the same event using vector embeddings rather than LLM-based grouping for batch article lists, and uses the `GroupMatcher` for incremental per-article matching during the fetch pipeline.

### Embedding

Article text (title + AI summary) is embedded using a local model via the `go-embedding` library. Embeddings are stored as pgvector `vector` values: `article_embeddings.embedding` per article, and `article_groups.embedding` for each group's centroid.

### Matching (pgvector ANN)

Grouping runs as in-database nearest-neighbour queries rather than fetching every embedding into the app (#186). For each freshly embedded article the JOIN phase finds the nearest existing group centroid within the threshold (`embedding <=> centroid`, an HNSW `vector_cosine_ops` index backs the search); articles that match join that group, and the rest are clustered among themselves (single-linkage over pgvector distance edges) into new groups. A group's centroid is the mean of its members' embeddings, recomputed in the database with pgvector's `AVG` aggregate after membership changes.

### Matching Threshold

A configurable similarity threshold (cosine similarity, 0–1) controls how aggressively articles are merged into existing groups; pgvector measures cosine distance, so the threshold is applied as `distance <= 1 - threshold`. If no group centroid is near enough, a new group is created. Group topics are refined when a group reaches 3+ articles.

### LLM-Based Batch Clustering

The `ClusterArticles` method provides an alternative clustering path for batch list operations, asking the curation model to group a set of articles by topic. This is used by `herald list --cluster` for ad-hoc grouping of displayed results, separate from the persistent group state maintained during fetch.

## Storage

PostgreSQL schema managed by [goose](https://github.com/pressly/goose) migrations embedded under `internal/storage/migrations/`. `NewPostgresStore` runs `goose up` on every open, so a fresh database is built from the migrations and an already-current one is left untouched. `0001_initial_schema.sql` is the idempotent baseline (it doubles as a no-op against the pre-goose production database); later migrations are ordinary forward steps. Key tables:

| Table | Purpose |
|-------|---------|
| `feeds` | Feed subscriptions with URL, title, ETag, Last-Modified, and error state |
| `articles` | Article content: title, URL, content, summary, author, published date |
| `article_authors` | Normalized author records per article (supports multi-author) |
| `article_categories` | Normalized category tags per article |
| `read_state` | Per-user read flag, starred flag, interest score, security threat |
| `user_preferences` | Key-value preference store per user (keywords, thresholds, notification settings) |
| `user_feeds` | Many-to-many subscription mapping between users and feeds |
| `article_summaries` | Cached AI summaries, one per article, shared by all users |
| `article_groups` | Topic clusters with centroid embeddings |
| `article_group_members` | Many-to-many membership between groups and articles |
| `group_summaries` | Cached group narrative summaries with max interest score |
| `user_prompts` | Per-user custom prompt templates and temperatures |
| `filter_rules` | Per-user scoring rules: an axis (author/category/tag/title/summary/content), a match mode (exact/substring/regex), and a positive or negative score |
| `users` | Registered users for multi-user deployments |

Feeds are shared across users; `user_feeds` tracks subscriptions. Articles are stored once; `read_state` tracks per-user scores and read status. Summaries are per-article and shared by every subscriber, like the security verdict: each article is summarized once with the global summarization prompt.

## Prompt System

Herald uses Go `text/template` for all AI prompts, with a 3-tier fallback:

```
Tier 1 (lowest priority): embedded defaults — compiled into the binary via go:embed
Tier 2: config file — prompts.* fields in config.toml
Tier 3 (highest priority): user database — per-user custom templates in user_prompts
```

The five prompt types are:

| Type | Model | Purpose |
|------|-------|---------|
| `security` | Gemma | Threat detection and prompt injection screening |
| `curation` | Llama | Interest scoring with user keywords |
| `summarization` | Llama | Single-article AI summary generation |
| `group_summary` | Llama | Multi-article narrative synthesis |

Each prompt type also has a configurable temperature following the same 3-tier fallback.

The `security` prompt type is intentionally excluded from user customization — it cannot be viewed or modified through the web UI or config overrides. The `summarization` prompt is global: summaries are shared per-article, so only the admin (user 0) override applies and per-user customization is rejected.

## Design Decisions

### Two-Model Separation

Security and editorial judgment are fundamentally different problems. Conflating them in a single model forces every relevance scoring decision to also carry safety filtering weight, which introduces systematic bias into interest scores. Treating the security boundary as an architectural boundary — a distinct model with a distinct purpose — keeps the two concerns independent and separately tunable.

### Ollama for Local Inference

All AI inference runs through Ollama on localhost. This means feed content never leaves the machine, there are no per-token API costs, and the system works offline. Model selection is configurable; users can swap Gemma or Llama for any Ollama-compatible model by changing two config values.

### Vector Clustering over LLM-Based Grouping

Persistent article grouping uses vector embeddings and pgvector similarity rather than asking an LLM to group articles. The staged cluster pass matches each article against the existing group centroids and links the remainder into new groups, using the LLM only to name a group once it forms. LLM-based grouping is available for ad-hoc batch clustering (`herald list --cluster`) but is not used for the persistent group state, where it would require re-running the LLM over all articles on each fetch.

### Config-Driven AI Prompts

Prompts are treated as configuration, not code. Users can customize every prompt type (except security) through the web UI or config file without modifying source code. The 3-tier fallback ensures embedded defaults always work out of the box, config-file overrides apply globally, and per-user database overrides allow individual customization in multi-user deployments.
