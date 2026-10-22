# Article analysis

Design doc for replacing per-user curation with one user-independent analysis pass per article, and a cheap per-user mapping from that analysis to an interest score.

Status: **design, not implemented.** The interim step, a curation prompt that states its judgement rules and caches its system half, has shipped. Everything below is open for discussion.

## Why

Curation today is a model call per (article, user). The prompt embeds the user's keyword list, so the output is one number that belongs to one user and says nothing reusable about the article. Three costs follow from that shape:

- **It scales with users, not articles.** Ten subscribers to the same feed mean ten model calls per article that all read the same text. The shared-prompt free tier sketched in docs/batch-scoring.md cannot exist while the prompt contains the user's keywords.
- **Preferences are expensive to change.** Editing the keyword list invalidates every score, and recomputing means re-running the model over the backlog. So preferences are effectively frozen, and the feedback corpus (docs/feedback-events.md) has nothing cheap to adjust.
- **The model is asked a question it is bad at.** "How much does this user want this" folds together facts about the article (what it is about, whether it is breaking, how substantive it is) with a guess about a person it knows by a keyword list. The facts are stable and checkable; the guess is not, and it is where the value judgements creep in. The prompt rules added as the interim step push back on that, but a per-user prompt will always be tempted to decide that a recipe on a security blog is "off topic".

The alternative is to split the work. The model answers the factual questions once per article and writes a structured record. The interest score for a user is then a function of that record and the user's preferences and signals, computed in Go or SQL, re-runnable in milliseconds whenever either side changes.

## Principles

- **Lift, do not bury.** The analysis describes the article. Ranking lifts what matches a user's interests and what is broadly significant. Nothing is hidden because of model output alone; hiding comes from the user's own filters, votes, unsubscribes, and threshold. A personal post from a source the user chose is an ordinary post, scored on its own merits, not a lapse from the feed's usual subject.
- **Facts over verdicts.** Every field the model fills should be something a second reader could check against the article: its topics, whether it is a first report, how long it is, what stance it argues. "Quality" and "relevance" are not fields; they are things the mapping derives.
- **Closed vocabularies where the mapping needs them.** Content kind, timeliness, scope, and leaning are enums so the mapping and the filter rules can match on them without string games. Topics are open but normalized.
- **One prompt, shared by everyone.** The analysis prompt has no per-user content, so its system half is identical across every article and every user. That is the prefix the cache wants and the prompt the free tier shares. Per-user custom prompts remain possible as a second, optional pass that reads the analysis record and the article.
- **Provenance stays.** Every record carries the model and prompt hash that produced it, and every derived score carries the mapping version, so the feedback corpus can still say which prediction it is grading.

## The analysis record

One run per article produces one JSON object. Proposed fields, with the reason each earns its place:

| Field | Type | Why |
|---|---|---|
| `summary` | string, 2-3 sentences | Folds the summarization pass into the same call. Same length rules and skip reasons as today. |
| `topics` | list of `{name, source}` | What the article is about. `source` is `feed` when the topic restates a category or tag the feed supplied, `model` when inferred. The split lets the mapping trust feed-supplied topics more and lets a later pass measure how often the model agrees with the source. |
| `content_kind` | enum: `report`, `analysis`, `opinion`, `link`, `personal`, `announcement`, `roundup`, `reference`, `other` | Separates "this is a one-line link" from "this is thin" and "this is opinion" from "this is a first report" without a quality judgement. |
| `timeliness` | enum: `breaking`, `developing`, `follow_up`, `evergreen`, `dated` | Replaces the 9-10 rules in the curation prompt. `breaking` means a first report; `follow_up` is coverage of something already widely reported. |
| `scope` | enum: `global`, `national`, `regional`, `local`, `niche` | Who the event matters to. |
| `places` | list of strings | Named places, so "regional events near the user" becomes a lookup against the user's region instead of a guess. |
| `significance` | integer 0-10 | Importance to a general reader, independent of any user. The "everyone" axis: war breaking out is a 10, a product update is a 2. This is the only number the model emits. |
| `leaning` | enum: `none`, `left`, `center_left`, `center`, `center_right`, `right`, `mixed` | The stance the article argues, not the source's reputation. `none` is expected to be the most common value and the model must be told so. See the caveat below. |
| `substantive` | boolean | Whether there is enough text to analyze. False for link-only posts, truncated feeds, and image-only items. A fact about the input, not a verdict. |
| `language` | BCP 47 tag | Needed before any multilingual user exists; cheap to collect now. |
| `entities` | list of strings | People and organizations. Useful for interest matching and grouping. Candidate for a second version if it makes the record too noisy for local models. |

### Leaning is the field to be careful with

It is the one axis where the model's own bias is the measurement instrument. Three guards:

- It is data for the user's mapping, never a default ranking input. A user who wants to read both sides, or only one, or does not care, expresses that in preferences. The default weight is zero.
- The vocabulary describes the article's argument, not its author or outlet, and `none` is the expected answer for reporting, technical content, and most of what herald ingests. The prompt says so explicitly, and the record stores `none` as a positive assertion, distinct from a missing field.
- It is evaluated. The feedback corpus can test whether leaning predicts anything about reader behavior on this instance. If it does not, it stays as a filter axis and nothing more.

### Topics and the feed's own categories

RSS and Atom items carry categories and tags; herald already stores them in `article_categories`. They are the source's own description and belong in the analysis as input and as output. The prompt receives the feed's categories as a labeled hint; the record echoes the ones it kept with `source: feed` and adds its own with `source: model`. The mapping treats the two differently: feed-supplied topics are the author's claim, model topics are an inference. Per-user feed tags (`feed_tags`) are a user's own labels and stay out of the analysis entirely.

Free-form topics will fragment ("infosec", "security", "cybersecurity"). Two mitigations, in order: a seed taxonomy of a hundred or so names the prompt is asked to prefer, with free-form additions allowed; and, later, embedding-based normalization of the free-form tail against that seed list. Do not block the first version on the second step.

## Mapping a record to a user

The interest score becomes a function, not a model output:

```
score = clamp(0..10,
    base(significance, timeliness, scope, places vs user region)
  + interest_match(topics, entities) * user weight
  + feed_weight(user_feeds, feed_tags)
  + rule_adjustments(filter_rules)
  + signal_adjustments(votes, feedback history))
```

Properties the function must have:

- **Neutral default.** With no preferences at all, the score is `base`, which is significance tempered by timeliness. A fresh account sees a general-reader ranking, not a wall of zeros.
- **Interests boost, never gate.** `interest_match` is additive. An article with no topic overlap keeps its base score.
- **Downranking comes from the user.** Negative adjustments come from `filter_rules`, votes, and the signal history, which the user controls and can inspect. The model's fields never subtract on their own.
- **Recomputable.** Changing keywords, region, or weights re-runs the function over stored records. No model call.
- **Versioned.** The mapping has a version string recorded beside the score, in the slot `prompt_hash` fills today.

The per-user kNN scorer (#255) fits here as another term, or as a replacement for the hand-written `interest_match` once the corpus can train it. The analysis record gives it features that are not just the raw embedding.

## Prompt shape

- **System half:** the whole rubric, the vocabularies, the summary length rules, the JSON schema. Expected to be 1,500 to 2,500 tokens. No per-user content, so one cache entry serves everything. On local models this is the part to measure: a 4B model handed a long schema will drop fields, so the parser accepts partial records and the vocabulary check rejects invalid values field by field rather than failing the article.
- **User half:** the nonce-fenced title and content as today, plus the feed's categories as a labeled hint block, and the article's publication date and the feed's title. Nothing from the user's account.
- **Long context:** on cloud models the whole article should go in; the 3,000-character cap exists for local models. Lifting it for cloud is the follow-up already noted in docs/batch-scoring.md.
- **Custom per-user prompts:** keep the mechanism (user_prompts) for an optional second pass. Its input is the article plus the analysis record, and its output is a per-user override of the mapped score. Not batched, not shared, the paid-tier shape from the tiers note.

## Storage

- `article_analysis`: `article_id` PK, `model`, `prompt_hash`, `analyzed_at`, the enum fields as text columns with CHECK constraints, `significance`, `substantive`, `language`, `summary`, and `record JSONB` holding the full object for fields added later. Writing `summary` through to `article_summaries` keeps every existing summary consumer unchanged during rollout.
- `article_topics`: `(article_id, topic, source)` with a lowercase index. Replaces nothing; `article_categories` stays as the raw feed data.
- `article_entities`, `article_places`: same shape, or folded into `article_topics` with a `kind` column. Decide when the first mapping needs them.
- `read_state.interest_score` and its provenance columns stay. `prompt_hash` records the mapping version for mapped scores; `score_model` records the analysis model.

## Pipeline and batch

A new batch kind, `analyze`, with one subject per article and custom id `a-<article>`. It fits the existing Kind interface without changes: `Due` claims analyzed-less articles that passed screening, `Build` renders the shared prompt, `Apply` validates and stores the record, then runs the mapping for every subscriber of the feed. The curate kind stays registered until the mapping replaces it, and the synchronous path gets the same stage for local models.

Mapping runs in the curate daemon's cycle as a pure store operation: for every (user, article) pair where the record is newer than the score, or the user's mapping inputs changed, recompute. It is the only per-user work left in the cycle.

## Rollout

1. **Shadow.** Ship the prompt, parser, tables, and the `analyze` kind. Run it beside curate. Nothing reads the records yet. This is where the local-model measurement happens: the same sample through Gemma-4-E4B and Qwen3.5-4B, checking field completeness and vocabulary validity, not score agreement.
2. **Evaluate.** Compute mapped scores from the records and compare them with the curate scores against the feedback corpus, using the method in docs/feedback-events.md. The question is not whether they agree with curate; it is which one the reader's behavior agrees with.
3. **Switch.** Behind a config flag, write mapped scores to `read_state`. Unregister the curate kind. Keep the summary write-through.
4. **Fold in summaries.** Stop running the summarization pass on articles that have a record. The summarization prompt remains for local deployments that do not run analysis.
5. **Source profiles.** Aggregate records per feed over a window: typical topics, typical leaning, share of link posts. That is the data for "this source usually writes about X" done honestly, from observation, and it feeds the grouping labels and the feed directory.

## Open questions

- Whether `significance` should exist at all, or whether `timeliness` plus `scope` already carries it. One number from the model is a small concession to practicality; two would be the old problem back.
- Seed taxonomy: who writes it, and whether it is per-instance config or embedded. Leaning toward embedded with config additions, the same tiering as prompts.
- Whether `entities` and `places` earn their fields in the first version, or wait until a mapping term wants them.
- How the mapping exposes its terms to the user. A settings page with weights is the obvious answer and the wrong one for most readers; a few named presets plus the existing keywords and threshold may be all that is needed, with weights as an admin or power-user surface.
- Re-analysis policy when the prompt changes. Records are versioned by hash, so the question is only how far back to re-run, and the batch discount makes "all of it" plausible.

See docs/batch-scoring.md for the batch path the analyze kind rides on, docs/feedback-events.md for the corpus that evaluates the mapping, and docs/prompts.md for the prompt tiers.
