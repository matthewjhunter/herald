# Feedback events

Design doc for Herald's feedback event log: the append-only record of what
Herald predicted about an article, what the reader actually did with it, and
which of those two disagreed.

Status: **collection is built and running.** The event log, the passive signals,
and the explicit controls have shipped; nothing yet consumes the corpus.

- **#251** -- this table and its write paths (everything else depends on it).
  **Shipped** (PR #260).
- **#252** -- explicit vote controls, reason axes, unsubscribe reason.
  **Shipped** (PR #262).
- **#258** -- prompt versioning and scoring-time provenance. **Shipped**
  (PR #261). Not originally part of this plan; see "Prediction provenance".
- **#253** -- exploration slot. Deprioritized on this instance, for a reason
  worth reading before consuming the corpus -- see "Exploration".
- **#254** -- mining events into proposed filter rules. Unblocked by #259 (rules now affect scores); still needs the axis translation above.
- **#255** -- per-user kNN scorer over pgvector
- **#256** -- feedback for the summary, grouping, security, and link-extraction
  models

## Why

Herald already makes predictions and already observes outcomes -- it just never
connects the two. The interest scorer writes `read_state.interest_score` and
forgets it. The reader marks things read, stars them, mutes groups,
unsubscribes. None of that is ever compared against what the scorer said, so the
scorer cannot improve and no one can even measure whether it is any good.

The log exists to capture the disagreement. Its value is not "which articles
does this user like" -- that is already implicit in `read_state`. Its value is
"here is what the model predicted, here is what happened, here is the gap."

## Non-goals

- **Not analytics.** Nothing here is aggregate usage measurement, and nothing
  here is a product metric.
- **Not a replacement for `read_state`.** `read_state` stays the authority on
  current per-user state (is this read, is it starred). The event log is
  history: state transitions with context attached. They answer different
  questions and both are needed.
- **Not a training pipeline.** This doc covers collection only. What consumes
  the data is deliberately out of scope, because the collection design should
  not be contorted to fit whichever modeling approach wins.

## Privacy boundary

`docs/analytics.md` states, flatly, that the authenticated reader is never
tracked. That promise stands and this design does not weaken it. The distinction
is between *tracking* and *local state*:

- Feedback events **never leave the instance**. No third party, no Herald
  project endpoint, no default upload, no opt-out telemetry. There is no code
  path that transmits them anywhere, and adding one requires changing this
  document first.
- Events live in the operator's own database, next to the reading history the
  operator already stores in `read_state`. A self-hosted Herald learning from
  its own reader is a local preference file, not surveillance.
- **Deletion must be total.** Feedback events are per-user data and must be
  removed by the admin user-deletion path (plan 009). A user who deletes their
  account leaves no behavioral residue.
- **Export before sharing, always opt-in.** If a corpus is ever published (see
  "Durability", below), it is by explicit per-user export, never by default
  collection. A feed list plus a reading history is a quasi-identifier: it
  reveals politics, health, employer, and location with high confidence. Treat
  any export as deanonymizable and gate it accordingly.

The wording in `docs/analytics.md` should be extended when this ships, so the
promise reads "never tracked, and the personalization data never leaves your
server" rather than appearing to be contradicted by a new table.

## Signal taxonomy

The central problem is that **Herald has several different ways to mark an
article read, and they mean opposite things.** Collapsing them into one boolean,
which is what `read_state.read` does today, destroys the signal. The reader
opens articles that interest them, one at a time; then clears the remaining
queue in bulk at the end of a session. Both paths set `read = TRUE`. Only the
first says anything about interest, and the second is not merely weaker -- it is
noise that will actively poison a model if treated as engagement.

Every event therefore records **which code path produced it**.

### Explicit signals (user-initiated, high confidence)

| kind | Source | Meaning |
|---|---|---|
| `vote_up` / `vote_down` | vote control, article view and list row | Direct label: wanted / did not want to see this. A downvote also marks the article read so it stops resurfacing; that dismissal emits no read event, so the article never appears here as engaged with |
| `vote_cleared` | same control, voting the same way twice | Retraction. Like `unstar`, a withdrawal and not an opposite -- clearing a downvote does not mean the reader liked the article. Clearing a downvote also restores the article to the unread lists |
| `star` / `unstar` | `StarArticle` | Strong positive; unstar is a retraction, not a negative |
| `group_mute` | `MuteGroup` (`handleGroupMute`) | Strong negative on a whole story cluster |
| `group_disband` | `DisbandGroup` | Grouping was wrong -- a label on similarity, not on interest |
| `feed_unsubscribe` | `UnsubscribeUserFromFeed` | Feed-level rejection -- but see below; it is not always a content judgment |
| `summary_edit` | new UI control | Paired (bad, good) example -- the richest label Herald can collect |

### Reason axes do not mirror `filter_rules`

This document originally asserted that a vote's `axis` would mirror
`filter_rules.axis`, making rule proposal a direct copy. **That was wrong**, and
it is worth recording why rather than quietly dropping it.

`filter_rules.axis` carries `CHECK (axis IN ('author', 'category', 'tag'))`. The
reasons a reader actually gives do not fit inside that vocabulary:

| Vote reason | axis stored | Maps to a filter rule? |
|---|---|---|
| Not this topic | `topic` | Loosely -- `category` or `tag`, if the article carries either |
| Not this feed | `feed` | No. Feed scoping is a column (`filter_rules.feed_id`), not an axis |
| Not this source | `source` | No. The linked-to domain has no axis at all |
| Already saw this | `duplicate` | No, and it is not a content judgment in the first place |
| Not right now | `timing` | No, and it must never harden into a standing rule |

Three of the five have no representation. So the shipped vocabulary is its own
closed set, stored verbatim in `axis`, and **#254 is a translation step rather
than a copy** -- it has to decide what a `source` rejection even means in a rules
engine that cannot express one. Since #259 the rules do at least have an effect
to translate *into*: they adjust the interest score, so a mined rule changes
ranking rather than being stored and ignored.

Axes are validated against the closed set at the handler. They arrive from a
form field, and an unvalidated value would let a crafted request write arbitrary
strings into the training corpus, where a consumer grouping by axis would
silently split on typos. An unrecognized axis is dropped and the vote still
counts -- a bare vote is a valid label.

### Unsubscribe needs a reason, not just a fact

An unsubscribe is tempting to treat as a bulk negative over everything the feed
published. That is wrong often enough to be dangerous, because most unsubscribes
have nothing to do with content:

- The feed **broke** -- 404, parked domain, TLS expiry, a CMS migration that
  changed the feed URL.
- The feed **went quiet** and the reader is tidying up.
- The feed **changed format** -- full text became truncated stubs, or it started
  emitting one item per comment.
- The site is fine but the reader is **cutting volume**, not interest.

Only the last-resort case ("I no longer care about this subject") is a content
judgment, and it is probably the minority. Bulk-downranking a dead feed's back
catalogue teaches the model to avoid *topics* because a *server* went away --
and the older, still-relevant articles the reader genuinely liked get punished
for their publisher's outage.

Two mitigations, both required:

1. **Ask.** The unsubscribe control offers a one-click reason (`broken`,
   `volume`, `not_interested`, or no reason), stored in `axis`. Only
   `not_interested` may propagate as a content negative. "Just unsubscribe" is
   listed first and is the default: an unlabeled unsubscribe is honest, and a
   guessed one actively misleads. The unsubscribe vocabulary is validated
   separately from the vote vocabulary -- a vote axis arriving on an unsubscribe
   is a bug or a forgery, not a label, and is dropped.
2. **Snapshot feed health.** `feeds` already carries `consecutive_errors`,
   `last_error`, and `status`. Copy them onto the event. A feed unsubscribed
   with a nonzero error count or `status != 'active'` is presumed broken and its
   articles are excluded from negative-label mining regardless of what the
   reader clicked -- the machine evidence outranks a hurried click.

The same caution applies in weaker form to `group_mute`: muting a story cluster
usually means "enough of *this story*", not "never this topic again". Mute is a
negative on the cluster, decaying quickly, not a standing topic ban.

### Implicit signals (free, noisier, still worth having)

These cost the reader nothing and are generated in volume. They are weak
individually and strong in aggregate, and they only work if the read paths are
distinguished:

| kind | Source | Interpretation |
|---|---|---|
| `article_opened` | `handleArticleView` auto-mark (`handlers.go:970`) | Genuine engagement. The reader chose this one out of a list. **Positive.** |
| `read_toggled_on` | `POST /articles/{id}/read` (`handleReadToggle`) | Deliberate dismissal without opening. **Weak negative.** |
| `read_toggled_off` | same endpoint, `read=false` | Reader wants it back. **Weak positive.** |
| `bulk_dismissed` | `handleMarkAllRead`, `handleGroupMarkRead`, `handleSummaryMarkRead` | Queue bankruptcy. **Carries no interest signal at all.** Record it, weight it zero, and keep it only so the other kinds are not contaminated by it. |
| `external_read` | Fever API item mark (`fever.go`, `MarkArticleRead`) | Ambiguous -- see below. |
| `external_bulk_read` | Fever feed/group/all mark | Same as `bulk_dismissed`. |
| `clickthrough` | outbound link beacon | Reader left for the original site. Positive, but see below -- it must be read against `content_length`. |
| `search_result_click` | search UI | Reserved, unused. Search is not mined as a passive interest signal -- see below. |

### Search is not a passive interest signal

An earlier version of this doc listed a search-result click as "a relevance
pair, and a statement of interest the keyword prefs never captured", and
scoped query capture into #252. **That is dropped deliberately.** Search
interactions are not mined as passive signals, and the query text is not
recorded.

The two are different labels wearing the same shape:

- A search click is a **relevance** label. It says this document matched that
  query. The reader already knew what they wanted and went to find it.
- Every other signal here is an **interest** label. It says the reader wanted
  this surfaced *unprompted*, which is the only question the curation scorer
  actually asks.

Mining search would answer the wrong question well. A reader searching for a
term they have no standing interest in -- checking a fact, chasing a reference,
looking up something for someone else -- produces a strong-looking positive on a
topic they would not want in tomorrow's queue. The corpus would learn "surface
more of what he looked up", which is close to the opposite of what it is for.

Search also carries the sharpest privacy edge in the app. Queries are far more
revealing than reading, and "Herald keeps every search you type" is a much worse
sentence than anything else in this design, for a signal that answers the wrong
question anyway.

If search-driven interest ever matters, it should arrive through the explicit
controls: the reader can vote on a search result exactly as they can anywhere
else, and that vote is an interest label because they chose to give it. The
`search_result_click` kind and the `web-search` surface stay defined so that
existing events remain readable and so an explicit vote from search records
where it came from.

### Clickthrough is conditional on how much text the reader had

Leaving for the original site looks like a clean positive and mostly is, but its
strength depends entirely on whether the reader had a choice. A truncated feed
publishes a two-line teaser; clicking out is the only way to read the piece at
all, so the click says little beyond "opened it" and should sit barely above a
passive read. On a full-text article the same click means the reader wanted the
source, the comments, or the images -- a genuinely strong signal.

Herald can already tell these apart: `articles.content` is what the feed sent
and `linked_content` is the fetched full text. Both are folded into
`content_length`, with `has_full_text` recording whether the fetch happened.

**Store the covariate, not the weight.** The event records how much text was
available and leaves the curve to the consumer. Baking a multiplier in at
collection time would mean every event recorded under the old curve becomes
unusable the first time it is retuned -- the same reason provenance is
snapshotted rather than joined.

`content_length` rides on *every* event, not just clickthroughs, because dwell
needs the same denominator.

Capture is an `hx-post` beacon on the existing outbound links rather than a
redirect through Herald: the `href` keeps the real URL, so copy-link, the status
bar, and middle-click stay honest, and navigation does not depend on the beacon
succeeding. Middle-click and copy-paste do not fire it, so clickthroughs
undercount rather than misattribute -- the right direction to fail.

`bulk_dismissed` is the reason this table exists. Without it, a nightly
mark-all-read of forty articles looks identical to forty articles the reader
sought out and opened, and every downstream model learns that the reader loves
everything they ignore.

**Fever is a special case.** Third-party clients (Reeder, NetNewsWire, etc.)
auto-mark items read on scroll, on their own schedule, with behavior Herald does
not control and cannot introspect. A Fever `item/read` therefore cannot be
trusted as engagement the way `article_opened` can. It gets its own kind and its
own `surface`, so an operator whose reading happens mostly through a Fever client
can be handled separately rather than silently mislabeled.

### Derived: dwell time, and why it only works in one direction

Dwell needs no client-side JavaScript, no beacon, and no CSP widening. It falls
out of the event stream at analysis time: the gap between an `article_opened`
event and that user's next event.

**The signal is asymmetric, and only the short end is trustworthy.** A three
second gap is strong evidence the reader opened the article, saw what it was,
and bailed. A forty minute gap is evidence of nothing at all -- a switched tab, a
phone call, a walk away from the desk, a browser left open overnight. Herald
cannot distinguish "read carefully" from "abandoned in a background tab", and
server-side timing never will.

So the rule is: **dwell may lower a score, never raise it.**

- Below a floor (a few seconds, or far under the reading time implied by
  `content_length`), record a bounce. This is the useful case, it is common, and
  it is reliable.
- Above a ceiling (start at five minutes, operator-tunable), treat the value as
  **censored, not large**. The reader may have read every word or may have gone
  to lunch; the honest encoding is "unknown", and the difference between twenty
  minutes and two hours carries no information whatever.
- In between, a weak positive at most.

Compare against `content_length` rather than against absolute seconds. Ninety
seconds is a complete read of a short post and a skim of a long one.

The Page Visibility API could tell Herald when the tab lost focus and would
genuinely sharpen this. It is not worth it: it means shipping JavaScript into the
authenticated reader, which is exactly the boundary `docs/analytics.md` draws,
and it would upgrade "unknown" to "slightly less unknown" while making a much
worse promise about what Herald watches. The bounce signal is the part with real
value, and it needs none of that.

## Prediction provenance

**A feedback event without the prediction it contradicts is not training data.**
This is the part most feedback systems get wrong: they log the thumbs-down and
not the score, and then the corpus can never answer "was the model wrong here?"

Every event snapshots, at the moment of the interaction:

- `interest_score` -- what the scorer said
- `score_model` -- which model produced it
- `prompt_hash` -- which prompt produced it. See "Provenance is recorded at
  scoring time" below; this is the part the first implementation got wrong.
- `rules_fired` -- the `filter_rules` rows that contributed, as JSONB.
  Populated since #259, as a JSON array of the rules that adjusted this
  article's score. NULL means no rule matched, not that the feature is
  unimplemented.
- `list_position` -- where in the rendered list the article appeared
- `surface` -- `web-list`, `web-article`, `web-search`, `web-group`,
  `web-summary`, `web-feeds`, `fever`
- `exploration` -- whether this article was injected by the exploration slot

Snapshotting rather than joining is deliberate. Scores get rewritten, prompts get
edited, rules get deleted. A join against live tables reconstructs *today's*
prediction, not the one the reader was reacting to, and silently rewrites
history. The redundancy is the point.

### Provenance is recorded at scoring time, not at read time

The first implementation snapshotted `score_model` and `prompt_hash` by joining
`user_prompts` when the **reader** acted. That satisfies the letter of
"snapshot, don't join" while breaking its intent: it reconstructs the prompt in
force at *read* time, which is a different prompt whenever one was edited
between scoring and reading -- precisely the case that matters, since evaluating
an edit is the entire reason the labels are collected.

`read_state` therefore carries `score_model` and `prompt_hash`, written by the
curation stage at the moment the score is produced. That is the only point where
the answer is known for certain. The feedback insert copies them forward rather
than deriving them.

Both columns stay NULL for scores written before this landed. **NULL means
unknown, never "current."** A consumer that backfills the gap from `user_prompts`
would attribute old scores to a prompt that never saw them, which is worse than
having no attribution at all.

### Every prompt tier has an identity (#258)

`prompt_hash` was originally specified as a hash of the user's `prompt_template`
row. On a stock instance that produces NULL on every event, which is how the
problem was found in production: `user_prompts` was empty, no `[prompts]` config
section existed, curation ran on the built-in template, and **every event
recorded no provenance at all**. The default deployment was the unattributable
one.

Prompt resolution walks four tiers -- embedded default, config file, admin
override (`user_id = 0`), user row -- and the bottom two have no row to hash. So
`prompt_versions` is **content-addressed**: keyed by the sha256 of the resolved
template text, with a `source` column recording which tier supplied it. Hash
answers "which prompt", source answers "how did it come to be used", and they
are deliberately separate questions. A built-in prompt now registers itself the
first time a process resolves it, so a stock instance is as attributable as a
customized one.

Two consequences worth knowing:

- **The text behind a hash is recoverable.** The original design accepted a bare
  fingerprint. Versions store the template, so a hash on a two-year-old event
  can be read back.
- **Reverting a prompt appends rather than rewinds.** A version you rejected
  stays on the record, because scores written under it must remain
  attributable.

Resolution returns template, model, temperature and hash as one unit. Fetching
them through separate accessors is what allowed a hash and a model to describe
different prompts -- the structural cause of the bug above, not merely an
inefficiency.

`list_position` deserves specific mention: items at the top of a list get opened
more regardless of quality. A model trained on position-blind data learns "be at
the top" and nothing else. Recording position is what makes it possible to
correct for that later.

## Exploration

Every signal above is conditioned on what Herald chose to show. Articles the
scorer buried are never seen, never labeled, and never contradict the model that
buried them -- so the training set can only ever teach the scorer to sharpen its
existing bias. This is the standard bandit feedback loop, and it converges on a
confident, narrow, wrong model.

The fix is a small exploration slot: inject a random sample of low-scored
articles (start at 3%, operator-configurable, defaults on) into the list, flagged
`exploration = TRUE`. Upvotes on those are the highest-value labels in the
system, because they are the only evidence of false negatives that exists.

### How much this matters depends on how the reader scans

The argument above assumes the reader only ever sees what the scorer ranked
highly -- true when a list is long, ranked, and skimmed from the top until
attention runs out. It is **not** true of a reader who scans every headline in
the queue before deciding what to open.

That distinction changes what an absent signal means:

- **Ranked-and-truncated reading:** an unopened article may simply never have
  been seen. Absence carries no information, and a low-scored article that was
  never rendered cannot contradict the score that buried it.
- **Full-scan reading:** an unopened article was seen and passed over. Absence
  is a **real weak negative**, and a high-scored article that was scanned and
  skipped is direct evidence of a false positive -- collected for free, with no
  exploration slot at all.

The instance this was built for is the second kind, which is why #253 is
deprioritized here rather than treated as a prerequisite for #255. A consumer
that assumes standard position bias would misread this corpus: it would discard
as "unseen" exactly the passed-over articles that carry signal.

Herald does not currently record which mode a reader is in, and it cannot infer
it -- `list_position` tells you where an article sat, not how far down the reader
looked. Any consumer that treats non-interaction as a negative must make that
assumption explicit and operator-configurable rather than baking it in. On an
instance with ranked-and-truncated readers, the exploration slot goes back to
being the only source of false-negative evidence.

## Schema

Postgres only (Herald dropped SQLite; storage is sqlc/pgx). New migration.

Migration `0010_feedback_events.sql`, as shipped:

```sql
CREATE TABLE feedback_events (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Referent. article_id goes NULL when the article is pruned; the
    -- denormalized columns below keep the label usable. See "Durability".
    article_id     BIGINT REFERENCES articles(id) ON DELETE SET NULL,
    feed_id        BIGINT,
    article_title  TEXT,
    article_url    TEXT,
    embedding      vector(768),

    kind           TEXT NOT NULL,
    axis           TEXT,          -- reason vocabulary; NOT filter_rules.axis, see above
    axis_value     TEXT,

    -- prediction provenance, snapshotted at interaction time
    interest_score DOUBLE PRECISION,
    score_model    TEXT,
    prompt_hash    TEXT,
    rules_fired    JSONB,
    list_position  INTEGER,
    surface        TEXT NOT NULL,
    exploration    BOOLEAN NOT NULL DEFAULT FALSE,

    -- feed health at event time, so a broken feed's unsubscribe is not mined
    -- as a content negative (see "Unsubscribe needs a reason")
    feed_status    TEXT,
    feed_errors    INTEGER,

    -- how much body text the reader had, and whether full text was fetched --
    -- the covariates that make clickthrough and dwell interpretable
    content_length INTEGER,
    has_full_text  BOOLEAN,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_feedback_events_user_created ON feedback_events(user_id, created_at DESC);
CREATE INDEX idx_feedback_events_article ON feedback_events(article_id) WHERE article_id IS NOT NULL;
CREATE INDEX idx_feedback_events_kind ON feedback_events(user_id, kind);
```

**Append-only.** No `UPDATE`, no `DELETE` except by user deletion and retention.
A reader who upvotes, then downvotes, then upvotes again has produced three
events and changed their mind twice; that sequence is more informative than the
final state, and `read_state` already holds the final state anyway.

### Supporting tables

Three other migrations carry pieces of this design:

- **`0011_prompt_versions.sql`** -- append-only, content-addressed prompt
  history, plus `user_prompts.template_hash` so resolution stays a single
  indexed lookup on the hot path. See "Every prompt tier has an identity".
- **`0012_read_state_provenance.sql`** -- `read_state.score_model` and
  `read_state.prompt_hash`, written when the score is produced.
- **`0013_article_votes.sql`** -- current vote state, `(user_id, article_id)`,
  `vote` constrained to `-1` or `1` with no zero: a retraction deletes the row,
  so "no opinion" has exactly one representation.

`article_votes` holds **state only**; the history stays here in
`feedback_events`. It is deliberately not a column on `read_state`: a vote is an
opinion the reader volunteered, while read state is bookkeeping Herald maintains
on their behalf. Separate tables keep "never voted" distinguishable from "voted
neutral", and stop `ResetReadStateScores` from silently discarding opinions.

## Durability

`DeleteFeedArticlesBatch` deletes a feed's articles when its last subscriber
unsubscribes. A naively CASCADE-ing event log would therefore destroy a feed's
entire label history at the exact moment the reader produced the strongest
negative signal available -- the unsubscribe itself. Every article they rejected
vanishes along with the evidence that they rejected it.

Hence `ON DELETE SET NULL` plus denormalization. Title, URL, and a copy of the
embedding are snapshotted onto the event so the label survives the article. The
embedding copy is what keeps a pruned label useful to the kNN scorer, which
otherwise cannot place a vanished article in vector space; 768 floats per event
is cheap next to the article text it replaces.

Retention: keep events indefinitely by default at household scale. Revisit if a
public instance makes the table large -- but note that aggressive retention
directly trades away the model's memory, so any cap belongs in operator config
rather than being hardcoded.

## Evaluation before training

Freeze a labeled held-out set before building any consumer. Without it, every
subsequent "improvement" is a vibe, and there is no way to tell whether a new
scorer beats the current prompt or merely differs from it.

**Correction to an earlier version of this doc:** it said the interest-scoring
benchmark should live in #93's harness. It should not. #93 is unbuilt, and what
it specifies is a live-model, non-deterministic, opt-in
(`HERALD_RUN_LLM_EVALS=1`), off-CI evaluator aimed at injection resistance. A
kNN scorer calls no model and can be benchmarked deterministically in ordinary
CI. Putting it behind an opt-in LLM harness would mean it never runs.

What #93 *does* get from this table is the answer to its own open question about
where curation labels come from. `feedback_events` beats raw read/star history
because it keeps engagement separate from queue-clearing and carries the score,
model, prompt hash and list position each label was reacting to. An evaluator
using it has to respect the per-kind caveats above: `bulk_dismissed` weighted
zero, `unstar` and `vote_cleared` as retractions rather than negatives,
`group_mute` decaying, unsubscribes with `consecutive_errors > 0` treated as
dead-feed cleanup, and dwell only ever lowering.

The frozen set stays out of the repo, but for a different reason than #93's:
that rule exists to keep malicious payloads out of version control, while this
one is a reading history, which is a quasi-identifier.

## Rollout

The log is inert on its own -- it changes no behavior and shows the reader
nothing. That is a feature: it can ship first, accumulate real data, and let the
modeling decisions be made against a real corpus instead of a guess.

1. ~~Schema + write path, wired into every source in the taxonomy above.~~
   Shipped, PR #260.
2. ~~The vote control and reason axes -- the explicit labels.~~ Shipped,
   PR #262.
3. Exploration slot, so negatives are not the only labels available.
   Deprioritized -- see "How much this matters depends on how the reader scans".
4. Consumers: rule mining, then the kNN scorer, then anything heavier.

Collection changes and consumer changes are not interchangeable in ordering. A
consumer built in six months can run against six months of accumulated data; a
collection gap can never be filled retroactively. When in doubt, ship the
collection change first.

That asymmetry has already been paid once. Provenance recording (#258) landed
after 38,000 articles had been scored without it. Those scores cannot be
attributed after the fact, and rescoring them would only recover articles the
reader had not yet read -- a few hundred, against a queue that refills with
correctly-attributed scores in under a week. The backlog was written off.

On modeling weight: with one primary reader and a few hundred labels a month,
kNN over the existing pgvector embeddings will outperform a fine-tune for a long
time, at zero training cost and with same-day adaptation. Collect as though a
fine-tune is coming; do not train one until the simple approach has visibly
topped out.

## What shipped, and what it does not cover

Collection landed in PR #260 (schema, write paths, passive signals), the
explicit controls in PR #262, and provenance in PR #261. Known gaps, all
deliberate:

- **Fever bulk marks record nothing.** `FeverMarkFeedRead` and its siblings work
  by timestamp cutoff and never materialize article IDs, so enumerating them
  needs `RETURNING` on those queries. Bulk dismissal carries no interest signal
  anyway; the cost is that a consumer counting "articles passed over"
  undercounts for readers who live in a Fever client.
- **`rules_fired` is populated** since #259; NULL means no rule matched. Note
  that migration `0010`'s inline comment still says the column is always NULL.
  That was true when 0010 shipped; applied migrations are not edited, so this
  document is the correction.
- **Scores written before #258 have no provenance**, and it cannot be
  reconstructed. NULL there means unknown.
- **Search-result clicks record `article_opened` with `surface = web-search`**,
  and the query text is not captured. Not a gap -- a decision; see "Search is
  not a passive interest signal". No issue is open for it.
- **Dwell is derivable but nothing computes it.** Correctly a consumer concern.
- **The vote control has no keyboard shortcut, and is not getting one.** Two
  clicks is cheap enough in practice. Not a gap; no issue is open for it.

Article-scoped writes are gated on subscription (a join to `user_feeds`,
matching plan 003). The clickthrough beacon and the vote endpoint both take an
article ID straight from the request, so without it a crafted POST could write
articles the reader does not subscribe to into their own corpus. The vote upsert
carries the same guard, and a vote that writes no row records no event.

Reader-supplied context is validated rather than trusted throughout: `surface`
resolves against a closed set and falls back to the default, `list_position` is
bounded, and both axis vocabularies are closed sets checked separately. A forged
value can at worst mislabel that reader's own training data; the corpus stays
well-formed.

## Open questions

- Should `bulk_dismissed` record every article ID in the batch, or one event
  with a count? Per-article is more faithful and makes a nightly clear-out of
  200 articles produce 200 rows. Leaning per-article, since the batch composition
  is itself informative (what did the reader *not* open?). **Resolved:** shipped
  per-article.
- Does an `article_opened` event fire on re-opening an already-read article?
  Re-reads are a positive signal and worth keeping, but they inflate open counts.
- Should the digest/newsletter surface record impressions (what was sent) as
  well as interactions? Without impressions there is no denominator for the
  notification path.
- Should non-interaction be materialized as a negative label, or left for the
  consumer to infer? It depends entirely on the reader's scanning mode, which
  Herald does not record and cannot detect. Materializing it would bake an
  assumption into the corpus that a later consumer cannot undo; leaving it means
  every consumer re-derives the same judgment. Leaning toward leaving it, with
  the mode as operator config when a consumer needs it.
- How should a consumer weight a vote against a passive signal on the same
  article? A downvote on something the reader also opened and read to the end is
  a genuine conflict, not noise, and the explicit label should presumably win --
  but "presumably" is not a rule, and nothing tests it yet.
