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

## Variants

Two independent binary choices, so four runs at roughly ten minutes each:

| run | document prefix | query prefix | summary in chunk |
|---|---|---|---|
| A | clustering | none | yes (current production) |
| B | retrieval | retrieval-query | yes |
| C | clustering | none | no |
| D | retrieval | retrieval-query | no |

A is the baseline. B isolates the prefix question, C the summary question, D
tests whether they interact.

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
  and query paths, so they cannot drift apart again
- a switch for including the summary as chunk context

Both should default to today's behaviour so the baseline is genuinely the shipped
configuration. Flipping either **requires a full re-embed**: vectors built under
different prefixes are not comparable, and mixing them silently degrades results
rather than failing. Consider keying stored vectors by the variant as well as the
model if runs need to coexist.

## Watch alongside

`cluster_threshold = 0.85` was derived empirically against nomic vectors after
single-linkage chained the feed set into a 400-article group. It carries no
meaning on EmbeddingGemma vectors and must be re-derived before auto-grouping is
switched back on. If run B or D wins and documents move to the retrieval prefix,
that number needs deriving against whichever vectors grouping will actually see.
