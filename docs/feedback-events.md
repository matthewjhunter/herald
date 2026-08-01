# Feedback events

Design doc for Herald's feedback event log: the append-only record of what
Herald predicted about an article, what the reader actually did with it, and
which of those two disagreed.

Status: **design, not built**. Nothing in this document ships until the schema
below lands. See issue #TBD-1 (this doc), and the consumers that depend on it:
rule mining, the per-user kNN scorer, and per-model feedback.

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
| `vote_up` / `vote_down` | new UI control | Direct label: wanted / did not want to see this |
| `star` / `unstar` | `StarArticle` | Strong positive; unstar is a retraction, not a negative |
| `group_mute` | `MuteGroup` (`handleGroupMute`) | Strong negative on a whole story cluster |
| `group_disband` | `DisbandGroup` | Grouping was wrong -- a label on similarity, not on interest |
| `feed_unsubscribe` | `UnsubscribeUserFromFeed` | Feed-level rejection -- but see below; it is not always a content judgment |
| `summary_edit` | new UI control | Paired (bad, good) example -- the richest label Herald can collect |

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
   `too much volume`, `not interested`, `no reason given`), stored in
   `axis`/`axis_value`. Only `not interested` may propagate as a content
   negative. Default to no reason and treat unlabeled unsubscribes as neutral.
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
| `search_result_click` | search UI | Query plus chosen result: a relevance pair, and a statement of interest the keyword prefs never captured. |

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

### Derived: dwell time

Dwell is valuable and needs no client-side JavaScript, no beacon, and no CSP
widening. It falls out of the event stream at analysis time: the gap between an
`article_opened` event and that user's next event is a serviceable proxy for
time on article. Sessions ending mid-article produce an open-ended interval;
discard those rather than guessing. Herald should not add a tracking script to
the authenticated reader to measure this more precisely -- the approximation is
worth far less than the privacy boundary it would cost.

## Prediction provenance

**A feedback event without the prediction it contradicts is not training data.**
This is the part most feedback systems get wrong: they log the thumbs-down and
not the score, and then the corpus can never answer "was the model wrong here?"

Every event snapshots, at the moment of the interaction:

- `interest_score` -- what the scorer said
- `score_model` -- which Ollama model produced it
- `prompt_hash` -- which prompt produced the score. Note that `user_prompts` has
  **no version column today** -- only `updated_at`, and the row is mutated in
  place, so an edited prompt leaves no trace of what the previous text was.
  Snapshot a hash of `prompt_template` on the event (cheap, no schema change to
  `user_prompts`, and enough to partition the corpus by prompt generation). If
  reconstructing the prompt text itself later matters, `user_prompts` needs
  proper versioning first -- that is a separate change and not a prerequisite.
- `rules_fired` -- the `filter_rules` rows that contributed, as JSONB
- `list_position` -- where in the rendered list the article appeared
- `surface` -- `web-list`, `web-article`, `web-search`, `fever`, `digest`,
  `newsletter`
- `exploration` -- whether this article was injected by the exploration slot

Snapshotting rather than joining is deliberate. Scores get rewritten, prompts get
edited, rules get deleted. A join against live tables reconstructs *today's*
prediction, not the one the reader was reacting to, and silently rewrites
history. The redundancy is the point.

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

## Schema

Postgres only (Herald dropped SQLite; storage is sqlc/pgx). New migration.

```sql
CREATE TABLE feedback_events (
    id             BIGSERIAL PRIMARY KEY,
    user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Referent. article_id goes NULL when the article is pruned; the
    -- denormalized columns below keep the label usable. See "Durability".
    article_id     BIGINT REFERENCES articles(id) ON DELETE SET NULL,
    feed_id        BIGINT,
    article_title  TEXT,
    article_url    TEXT,
    embedding      vector(768),

    kind           TEXT NOT NULL,
    axis           TEXT,          -- mirrors filter_rules.axis when a reason was given
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

The `axis` / `axis_value` columns intentionally mirror `filter_rules`. A
downvote with a reason ("not this topic", "not this source") is a supervised
label on exactly the dimension the rules engine already operates on, which is
what makes automatic rule proposal tractable instead of guesswork.

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
scorer beats the current prompt or merely differs from it. Issue #93 already
plans an AI evaluation harness (injection-resistance first, corpus outside the
repo); the interest-scoring benchmark should live in that harness rather than
becoming a second parallel eval mechanism.

## Rollout

The log is inert on its own -- it changes no behavior and shows the reader
nothing. That is a feature: it can ship first, accumulate real data, and let the
modeling decisions be made against a real corpus instead of a guess.

1. Schema + write path, wired into every source in the taxonomy above.
2. The vote control and reason axes -- the explicit labels.
3. Exploration slot, so negatives are not the only labels available.
4. Consumers: rule mining, then the kNN scorer, then anything heavier.

On modeling weight: with one primary reader and a few hundred labels a month,
kNN over the existing pgvector embeddings will outperform a fine-tune for a long
time, at zero training cost and with same-day adaptation. Collect as though a
fine-tune is coming; do not train one until the simple approach has visibly
topped out.

## Open questions

- Should `bulk_dismissed` record every article ID in the batch, or one event
  with a count? Per-article is more faithful and makes a nightly clear-out of
  200 articles produce 200 rows. Leaning per-article, since the batch composition
  is itself informative (what did the reader *not* open?).
- Does an `article_opened` event fire on re-opening an already-read article?
  Re-reads are a positive signal and worth keeping, but they inflate open counts.
- Should the digest/newsletter surface record impressions (what was sent) as
  well as interactions? Without impressions there is no denominator for the
  notification path.
