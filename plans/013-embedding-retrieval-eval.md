# Plan 013: Measuring embedding retrieval quality

Two decisions were made on reasoning rather than measurement, and both are now
cheap to test. A full re-embed of the corpus went from ~8.1 hours on the A380s
to roughly ten minutes on the Lemonade GPU pool (#289), which turns "we would
have to re-embed to find out" from a blocker into a coffee refill.

## The two questions

### 1. Are the task prefixes wrong for search?

Documents are embedded through `FormatRecordForTask(model, TaskClustering, ...)`,
rendering `"task: clustering | query:"`. Search queries go through
`GroupMatcher.EmbedText` (engine.go:380), which applies **no prefix at all**.

EmbeddingGemma is trained with asymmetric task prefixes: `TaskRetrievalDocument`
renders `"title: none | text:"` and `TaskRetrievalQuery` renders
`"task: search result | query:"`. Pairing a clustering-prefixed document with a
bare query compares across a task boundary the model was trained to distinguish.

`TaskClustering` was the right call when grouping was the only consumer. Grouping
is now `enabled = false` in production, so the live consumer is semantic search --
the embeddings are tuned for the feature that is off, at the expense of the one
that is on.

One vector cannot carry both prefixes, so this is a real trade, not an oversight
to correct blindly. Measure before switching, and remember the answer changes if
grouping is ever re-enabled.

### 2. Is the article summary worth half of every chunk?

Each chunk carries the fields, the task prefix, and the article summary as
retrieval context. Against `EMBEDDING_MAX_BYTES=1500` that header is roughly 700
bytes -- about **47% of every chunk is identical boilerplate**, and articles split
into ~3.5 chunks rather than the ~1.4 estimated.

The justification is contextual retrieval, published as reducing top-20 retrieval
failure by about a third. That result is not herald-specific: it used a per-chunk
LLM-generated blurb, while herald reuses one article-level summary on every chunk,
which is strictly weaker. It was adopted because it is free, not because it was
measured here.

It also has a cost beyond budget: repeated identical text makes chunks of the same
article resemble each other, which is the opposite of what chunking is for.

The question is therefore not on/off but **how much**. A sentence saying what the
document is about is the thing the technique calls for; a 500-character paragraph
is heavier than the published method, which used a short blurb. Test the summary
truncated to roughly a sentence as well as full and absent -- a short summary may
keep most of the benefit for a fraction of the budget, which would be the best
outcome available.

## Variants

Two axes: the task-prefix pairing, and how much summary each chunk carries. At
roughly ten minutes per run the full grid is affordable in an evening.

| run | document prefix | query prefix | summary in chunk |
|---|---|---|---|
| A | clustering | **none** | full (~500 chars) |
| A' | clustering | clustering | full |
| B | retrieval | retrieval-query | full |
| C | clustering | clustering | short (~150 chars) |
| D | retrieval | retrieval-query | short |
| E | clustering | clustering | none |
| F | retrieval | retrieval-query | none |

A is production as shipped and exists only as the historical baseline.

**A' is the one to run first, and it costs nothing.** Giving the query the same
prefix the documents already carry requires no re-embed -- it is a query-side
change measurable against the vectors already stored. If A' beats A, that is a
free improvement independent of everything else here, and it also establishes
that the harness can detect a difference at all before any re-embedding is spent.

Treat A' as the real baseline for the remaining runs; comparing anything against
a knowingly mismatched query path would overstate every result.

## Query set

The failure chunking exists to fix is a query about a passage deep in a long
article. The query set must therefore probe exactly that:

- Sample articles with **3 or more chunks** (`article_embedding_chunks`).
- Take a distinctive sentence from a chunk with **ordinal >= 2** -- never chunk 0.
- Target 300-500 queries for stable rates.

**The confound to avoid:** never draw the query from title, feed, author or
categories. Those appear in every chunk's header, so a title-derived query would
match the boilerplate rather than the content, and would flatter whichever variant
carries more header -- precisely the variable under test.

Near-duplicate news is a real hazard: several feeds cover one story, so a
competitor article outranking the source may be correct rather than a miss.
Prefer recall@10 over recall@1, and spot-check the top miss cases by hand rather
than trusting the aggregate.

## Chunk target is now an independent axis (#297)

Chunk size used to be whatever `EMBEDDING_MAX_BYTES` was -- the backend ceiling
doing double duty as the retrieval target. It is now a separate token figure
(`DefaultChunkTargetTokens`, 512), converted to bytes through the observed
bytes-per-token ratio, with the ceiling only able to make chunks smaller.

Two consequences for this eval. Chunk size can be varied without touching the
backend limit, so it is a dimension the eval can hold constant or sweep rather
than one that moves whenever the deployment config does. And the confound below
is smaller than when it was written: the summary and the body no longer compete
for the *whole* backend budget, only for the chunk target -- so shrinking the
summary still frees body bytes, but the swing is bounded by the target rather
than by whatever the ceiling happens to be set to.

The confound is not eliminated, because summary and body still share one chunk.
Measure coverage regardless.

## The coverage confound

Removing or shortening the summary frees header bytes, so **the body gets more of
the budget and chunks get bigger** -- which pushes more of them over the
backend's hard 512-token limit. The low-summary variants will therefore fail to
embed more articles than the baseline, and those articles are absent from the
index entirely rather than merely ranked worse.

That is not a retrieval-quality effect, and left uncorrected it biases the
comparison against exactly the variant expected to win. It also biases it in a
way that is easy to miss: a variant indexing fewer articles can post a *better*
recall rate over the queries it can still answer.

Observed on the production run at `EMBEDDING_MAX_BYTES=1500` with the full
summary: 57 articles (0.13%) exceeded 512 tokens and failed hard. With the header
shrunk by 350 bytes, expect materially more.

Two ways to keep it honest, and the runs should do both:

- **Equalise coverage.** Lower `EMBEDDING_MAX_BYTES` for the low-summary variants
  until their `HTTP 500` count matches the baseline's, so every variant indexes
  the same articles. This trades some of the freed budget back, which is the
  honest accounting -- the summary was never competing against nothing, it was
  competing against more body.
- **Report coverage as a metric, not a footnote.** Record embedded-article count
  and 500 count per run, and evaluate recall only over articles every variant
  managed to index. A query whose source article is missing from one index is not
  a fair test of ranking.

## Metrics

Per query, the rank of the source article in `SemanticSearch` results:

- **recall@1**, **recall@10** -- did the source article come back at all
- **MRR** -- rank-sensitive, catches "still found but demoted"
- **chunks per article** and **total chunk rows** -- the cost side; a variant that
  wins slightly while doubling the vector count may not be worth it
- **HTTP 500 count and embedded-article count** -- the coverage confound above.
  The summary-free variants leave more budget for body, so overruns *rise*
  rather than fall, and a variant that indexes fewer articles can post better
  rates over the ones it kept

Report all four runs together. A difference smaller than the spread between
repeated runs of the same variant is not a result.

## Harness

The eval needs no herald code: pull query sentences with SQL, embed them against
the backend over HTTP with the variant's query prefix, and run the distance query
directly against `article_embedding_chunks` with `DISTINCT ON (article_id)`, which
is what `SemanticSearch` does. That keeps the measurement independent of the code
being measured.

Producing the variants does need herald changes -- two knobs:

- an embed task selector (clustering vs retrieval) applied to both the document
  and query paths **from one setting**, so they cannot drift apart again -- the
  bare-query bug exists precisely because the two sides were set independently
- a chunk-context summary length: full, truncated to N characters, or omitted.
  A length rather than a boolean, so "shorter but still useful" is reachable

Both should default to today's behaviour so the baseline is genuinely the shipped
configuration. Note the asymmetry: the query prefix takes effect immediately,
while changing the document prefix or the summary length **requires a full
re-embed**: vectors built under
different prefixes are not comparable, and mixing them silently degrades results
rather than failing. Consider keying stored vectors by the variant as well as the
model if runs need to coexist.

## Watch alongside

`cluster_threshold = 0.85` was derived empirically against nomic vectors after
single-linkage chained the feed set into a 400-article group. It carries no
meaning on EmbeddingGemma vectors and must be re-derived before auto-grouping is
switched back on. If run B or D wins and documents move to the retrieval prefix,
that number needs deriving against whichever vectors grouping will actually see.
