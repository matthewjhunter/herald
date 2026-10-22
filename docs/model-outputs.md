# Model output corpus

The `model_outputs` table (migration 0021) keeps one row per scoring call: the model's completion verbatim and the identity of the input it answered. It exists so a local model can be trained on what herald's own models said.

Status: **collecting.** Every screen, summarize, and curate call writes a row. Nothing reads the corpus yet beyond the export query.

## Why a separate table

Each scoring call already records its verdict and, since #258, the model and prompt hash that produced it. What the live rows cannot hold is the training pair:

- **The completion is consumed by parsing.** `read_state` keeps a score, `articles` keeps a threat and category, `article_summaries` keeps the trimmed text. The reasoning sentence, the finding's evidence quote, and the untrimmed summary were thrown away.
- **The live rows are overwritten.** Rescoring under a new prompt replaces the score; a second summary replaces the first. A corpus has to be append-only or it only ever holds the latest opinion.
- **The input moves.** Article content is rewritten after scoring by the full-text fetch, the boilerplate trim, and the repair passes. A row has to say what the model saw, or at least be able to tell when the live article no longer matches.

## What a row holds

| Column | Meaning |
|---|---|
| `kind` | `screen`, `curate`, or `summarize`. |
| `article_id`, `user_id` | The article; the user for per-user kinds (curate), NULL for the article-global ones. |
| `chunk_index`, `chunk_count` | The screen sends one call per window of a long article; a row is one call. |
| `model`, `prompt_hash` | Provenance, as on the live rows. The screen prompt gained a hash for this: airlock's rendered instructions, or the override template. |
| `content_hash` | sha256 of the sanitized content the prompt was rendered from, the string the pipeline's `articleContent` returns. An exporter recomputes it against the live article and skips rows that no longer match. |
| `keywords` | The curation interests list as rendered, NULL when the user has none or for other kinds. Part of the input, so it travels with the output. |
| `response` | The completion, verbatim, capped at 16 KB. |
| `parsed` | What herald extracted: `{interest_score, reasoning}`, `{threat, category, verified}`, or `{length, accepted, reason}` for a summary that was kept or rejected. |
| `via` | `sync` for the pipeline's own call, `batch:<provider>` for one that came back through a provider batch API. |
| `trainable` | Whether the row may be used to train a local model. See below. |

## Trainable

A herald instance normally runs local models, whose output is its own, so the column defaults to true and the synchronous stages write true. The batch stage writes false: batch output comes from a cloud provider, and provider terms commonly restrict training other models on it. The rule is deliberately coarse. A synchronous call that happens to go through a gateway to a cloud model is still marked trainable, because the pipeline cannot see past the gateway; `model` is recorded on every row, so an exporter filters those by name.

## Trust

`response` is raw model output over attacker-controlled article text. It is never rendered by the web UI and never fed back into a prompt. Treat the table exactly like `articles.content`: untrusted bytes.

## Export

`ListTrainableModelOutputs(kind, afterID, limit)` pages the trainable rows of one kind in arrival order; that is the whole read surface today. A trainer joins each row to its article, recomputes `content_hash` over `articleContent`, drops mismatches, and renders the prompt from `prompt_versions` by hash to rebuild the exact input. Rows whose article has been deleted are gone too (`ON DELETE CASCADE`), since without the content there is nothing to train on.

## What it is not

It is not the feedback corpus (docs/feedback-events.md). This table records what a model said; that one records what the reader did. Training a local model on this table alone produces a copy of the teacher. Training on this table filtered by the reader's signals is what makes the copy better than the teacher, and the two join on `article_id`.

See docs/article-analysis.md for the structured record that is the intended long-term distillation target.
