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

## Metrics

Per query, the rank of the source article in `SemanticSearch` results:

- **recall@1**, **recall@10** -- did the source article come back at all
- **MRR** -- rank-sensitive, catches "still found but demoted"
- **chunks per article** and **total chunk rows** -- the cost side; a variant that
  wins slightly while doubling the vector count may not be worth it
- **HTTP 500 count** -- the summary-free variants leave more budget for body, so
  the 512-token overruns should fall

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
