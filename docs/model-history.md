# Model history

Herald keeps every summary and every security scan it has made of an article, each with the model that made it. Two tables hold this (migration 0024): `article_summaries`, which became a history, and `article_screens`, which is new.

They exist so that the model behind herald can change without losing what the previous one said, and so that two models can be compared on the same articles.

## Summaries

`article_summaries` has one row per generation. A row is told apart from the others for its article by `model`, `prompt_hash` and `generated_at`.

| Column | Meaning |
|---|---|
| `id` | The row. |
| `article_id` | The article. No longer unique. |
| `ai_summary` | The text, or `''` on a skip marker. |
| `skip_reason` | Set on a skip marker: why no usable summary was produced. |
| `model`, `prompt_hash`, `duration_ms` | Provenance, recorded when the row was written. NULL on rows from before it was tracked: unknown, never "whatever is current". |
| `is_current` | The one row per article that readers use. A partial unique index holds it to one. |
| `generated_at` | When the row was written. |

**Current.** Everything that reads a summary reads the current row: the article page, the inline summaries on list pages, the embedder (which prefixes the summary to every chunk), group summaries, and the unsummarized queue. They all join on `is_current`, so a longer history never shows up as duplicate articles.

**Writing.** A new summary is appended and becomes current; the row it replaces stays, no longer current. A skip marker is appended too, and names the model that failed when one was asked. It becomes current only if the article has no summary text on record. One model failing to compress an article does not take away the summary another model produced.

Because the embedder uses the current summary, changing which row is current leaves that article's embedding built from the old one until it is re-embedded.

## Screens

The verdict that decides whether readers see an article is still the `security_*` columns on `articles`. `article_screens` is the history beside them: one row per scan.

| Column | Meaning |
|---|---|
| `model`, `prompt_hash` | Who scanned, under which instructions. |
| `threat`, `category`, `verified`, `flagged` | The verdict, in the same terms as the columns on `articles`. Threat is the combined value, max(model, regex). |
| `via` | `sync` or `batch:<provider>`. |
| `effective` | Whether this scan set the article's gating verdict. |
| `screened_at` | When. |

An **effective** scan is the ordinary case: the live screen writes the verdict to the article and the history row in one transaction. A scan that is not effective is a **shadow**. It is recorded and changes nothing readers can see. Shadows are how a second model's opinion of already-screened articles is collected without that model deciding anything.

Like the verdict columns, a row carries no model prose and no quoted evidence (plan 012). The completions themselves are in `model_outputs`.

Scans made before migration 0024 were attributed by the migration where `model_outputs` could show which model produced the verdict, which covers everything screened since migration 0021. Older verdicts have no history row. The model that produced them was never recorded.

## Relation to the model output corpus

`model_outputs` (docs/model-outputs.md) is the raw record of every call: the completion verbatim, including the ones herald rejected. These two tables are what herald kept from those calls. The corpus is for training; the history is what the application and the comparison report read.

## Regenerating

`herald regenerate` fills in the configured model's rows for articles it has not handled: ones another model did, and ones done before models were recorded. It is run by hand. It is not a daemon stage, because it is a one-off pass over the whole corpus and it competes with live scoring for the same backend.

```
herald regenerate summaries [--limit N]
herald regenerate screens   [--limit N]
```

- **`summaries`** summarizes every security-passed article with no summary row from the configured summary model. The new summary becomes current. Re-embed afterwards if the embeddings should reflect it.
- **`screens`** scans every article that already has a verdict and no scan from the configured security model, as shadows. No gating verdict changes in either direction; acting on what the comparison shows is a separate decision.

The model is whatever the config and the admin prompt pages resolve to at the time, the same as the live stages. Each run asks the configured backend and stops if a completion comes back attributed to a different model than the one it is filling in.

Both are resumable. The queue is "articles with no row from this model", walked once by descending id, so Ctrl-C or a failed call leaves those articles for the next run and finished ones are never repeated. The summary line reports how many were attempted, how many now have a row, and how many are left.

## Comparing

`herald compare` reads the two histories and writes nothing.

```
herald compare screens                       # the models on record
herald compare screens   MODEL-A MODEL-B [--disagreements N]
herald compare summaries MODEL-A MODEL-B [--samples N]
```

Each side is a model's latest row per article, and only articles with a row from both models count.

- **`screens`** reports how many articles the two models put in the same bucket (pass, borderline, block, by the configured thresholds), the two off-diagonal counts that matter most (one model would show readers an article the other would hide), the mean threat difference, the full 3x3 table, and the disagreeing articles with the widest gap first.
- **`summaries`** reports how many articles both summarized against how many only one did, average length and model-call time, and a sample printed side by side. The sample is the same articles on every run. It does not score quality.

Two limits on what the screen comparison means. The scans were not necessarily made over identical text: an article's content can be rewritten between them by the full-text fetch or the boilerplate trim, and `content_hash` in `model_outputs` is what tells. And threat is the combined value, so where the regex detector fired both models share that floor.

Titles and summaries are feed text and model output over feed text. The report replaces control and format characters before printing them.
