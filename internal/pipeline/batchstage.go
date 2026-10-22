package pipeline

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/matthewjhunter/herald/internal/ai/batch"
	"github.com/matthewjhunter/herald/internal/output"
	"github.com/matthewjhunter/herald/internal/storage"
)

// Asynchronous scoring through a provider batch API (docs/batch-scoring.md).
//
// BatchStage drives any number of Kinds through one provider Submitter. Each
// cycle it polls the batches it has open, applies whatever ended, and then
// submits whatever is due. The stage knows nothing about articles or prompts;
// a Kind owns what a subject is, how to render it, and what to write back.

// Subject is one unit of work a Kind scores: an article, an article for a
// user, a user's day. Key is the kind's own identity for it and is what
// groups a subject's requests back together when results arrive.
type Subject struct {
	Key      string
	UserID   int64
	Articles []storage.Article
}

// Built is one subject rendered: provider requests and the matching rows to
// record, in the same order, with custom ids assigned on both.
type Built struct {
	Requests []batch.Request
	Rows     []storage.AIBatchRequest
}

// Kind is one category of batched work.
type Kind interface {
	// Name is the kind's identity in ai_batches.kind and in custom ids.
	Name() string
	// Due returns the subjects to submit now, claiming them where the kind
	// needs a claim, up to limit articles' worth.
	Due(ctx context.Context, limit int) ([]Subject, error)
	// Build renders one subject. A subject's requests always travel in one
	// batch, so Apply sees them together.
	Build(subject Subject) (Built, error)
	// Release undoes Due's claim on rows that will not get a verdict from this
	// submission. outcome says why, in storage.AIBatch* terms: errored spends
	// the attempt, anything else refunds it.
	Release(rows []storage.AIBatchRequest, outcome string)
	// Apply receives every row of one subject with that batch's results once
	// the batch ended, and writes the outcome.
	Apply(ctx context.Context, rows []storage.AIBatchRequest, results map[string]batch.Result) error
	// Adopt reconstructs a row from a custom id this kind issued, for a batch
	// found upstream that the table does not know. ok is false for an id that
	// is not this kind's.
	Adopt(customID string) (row storage.AIBatchRequest, ok bool)
}

// Mode decides, per user and kind, whether work takes the batch path. The
// first implementation reads configuration; a later one can read a user's
// tier.
type Mode func(userID int64, kind string) bool

// BatchStage submits due work and collects results.
type BatchStage struct {
	Store     storage.Store
	Submitter batch.Submitter
	Kinds     []Kind
	Formatter *output.Formatter

	// MaxRequests caps one provider submission. A subject larger than the cap
	// goes alone rather than being split.
	MaxRequests int
	// PollMinInterval spaces status checks on a running batch.
	PollMinInterval time.Duration
	// ExpiryGrace is how long past the provider's own expiry a batch may sit
	// without ending before the stage cancels it and treats it as expired.
	ExpiryGrace time.Duration

	// now is swapped by tests.
	now func() time.Time
}

const (
	defaultMaxRequests     = 1000
	defaultPollMinInterval = 2 * time.Minute
	defaultExpiryGrace     = time.Hour
	reconcileListLimit     = 50
)

func (b *BatchStage) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *BatchStage) maxRequests() int {
	if b.MaxRequests > 0 {
		return b.MaxRequests
	}
	return defaultMaxRequests
}

func (b *BatchStage) kind(name string) Kind {
	for _, k := range b.Kinds {
		if k.Name() == name {
			return k
		}
	}
	return nil
}

// Submit renders and sends everything due for each kind. A submission failure
// releases the rows that did not get sent and stops the cycle's submitting;
// the next cycle retries. A batch the provider accepted but the store could
// not record is logged as an orphan for Reconcile to adopt.
func (b *BatchStage) Submit(ctx context.Context) error {
	for _, k := range b.Kinds {
		if err := b.submitKind(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

func (b *BatchStage) submitKind(ctx context.Context, k Kind) error {
	subjects, err := k.Due(ctx, b.maxRequests())
	if err != nil {
		return fmt.Errorf("batch %s: due: %w", k.Name(), err)
	}
	if len(subjects) == 0 {
		return nil
	}

	// Group by model: a provider batch is one model's work (OpenAI requires
	// it; Anthropic does not care). Order within a model follows Due.
	byModel := map[string][]Built{}
	var models []string
	for _, subj := range subjects {
		built, err := k.Build(subj)
		if err != nil {
			b.Formatter.Warning("batch %s: build %s: %v", k.Name(), subj.Key, err)
			continue
		}
		if len(built.Requests) == 0 {
			continue
		}
		m := built.Requests[0].Model
		if _, seen := byModel[m]; !seen {
			models = append(models, m)
		}
		byModel[m] = append(byModel[m], built)
	}

	for _, m := range models {
		for _, chunk := range cut(byModel[m], b.maxRequests()) {
			if err := b.send(ctx, k, m, chunk); err != nil {
				// Nothing after this chunk was sent either; release it all.
				b.releaseRest(k, byModel, models, m, chunk)
				return err
			}
		}
	}
	return nil
}

// releaseRest releases every built subject from chunk onward in model m and
// everything in the models not yet reached, after a send failed.
func (b *BatchStage) releaseRest(k Kind, byModel map[string][]Built, models []string, failed string, chunk []Built) {
	reached := false
	for _, m := range models {
		if m == failed {
			reached = true
			start := indexOf(byModel[m], chunk[0])
			for _, built := range byModel[m][start:] {
				k.Release(built.Rows, "")
			}
			continue
		}
		if reached {
			for _, built := range byModel[m] {
				k.Release(built.Rows, "")
			}
		}
	}
}

func indexOf(all []Built, first Built) int {
	for i, b := range all {
		if len(b.Rows) > 0 && len(first.Rows) > 0 && b.Rows[0].CustomID == first.Rows[0].CustomID {
			return i
		}
	}
	return 0
}

// cut splits built subjects into runs of at most limit requests, never
// splitting a subject.
func cut(all []Built, limit int) [][]Built {
	var out [][]Built
	var cur []Built
	n := 0
	for _, built := range all {
		if n > 0 && n+len(built.Requests) > limit {
			out = append(out, cur)
			cur, n = nil, 0
		}
		cur = append(cur, built)
		n += len(built.Requests)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func (b *BatchStage) send(ctx context.Context, k Kind, model string, chunk []Built) error {
	var reqs []batch.Request
	var rows []storage.AIBatchRequest
	for _, built := range chunk {
		reqs = append(reqs, built.Requests...)
		rows = append(rows, built.Rows...)
	}
	sent, err := b.Submitter.Submit(ctx, reqs)
	if err != nil {
		return fmt.Errorf("batch %s: submit %d requests: %w", k.Name(), len(reqs), err)
	}
	rec := storage.AIBatch{
		ID: sent.ID, Provider: b.Submitter.Provider(), Kind: k.Name(), Model: model,
		SubmittedAt: b.clock(),
	}
	if !sent.ExpiresAt.IsZero() {
		exp := sent.ExpiresAt
		rec.ExpiresAt = &exp
	}
	if err := b.Store.RecordAIBatch(rec, rows); err != nil {
		// The provider has the batch; the table does not. Reconcile adopts it
		// on the next start, and the screen's claims hold until then.
		b.Formatter.Warning("batch %s: provider accepted %s (%d requests) but recording it failed, orphaned until reconcile: %v", k.Name(), sent.ID, len(reqs), err)
		return nil
	}
	b.Formatter.Info("batch %s: submitted %s with %d requests for %d subjects", k.Name(), sent.ID, len(reqs), len(chunk))
	return nil
}

// Poll checks every running batch of a registered kind and applies the ones
// that ended. Batches of other kinds belong to another process (the screen
// and curate daemons each register their own) and are left alone. A batch
// whose results cannot be fetched stays running for the next cycle. Results
// are applied before the batch is marked finished, so a crash mid-apply
// re-applies on the next poll; every write Apply makes is idempotent.
func (b *BatchStage) Poll(ctx context.Context) error {
	running, err := b.Store.ListRunningAIBatches()
	if err != nil {
		return fmt.Errorf("batch poll: %w", err)
	}
	now := b.clock()
	for _, rec := range running {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if b.kind(rec.Kind) == nil {
			continue
		}
		if rec.LastPolledAt != nil && b.PollMinInterval > 0 && now.Sub(*rec.LastPolledAt) < b.PollMinInterval {
			continue
		}
		b.Store.TouchAIBatchPolled(rec.ID) //nolint:errcheck
		if err := b.pollOne(ctx, rec, now); err != nil {
			b.Formatter.Warning("batch %s: %v", rec.ID, err)
		}
	}
	return nil
}

func (b *BatchStage) pollOne(ctx context.Context, rec storage.AIBatch, now time.Time) error {
	st, err := b.Submitter.Status(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	results := map[string]batch.Result{}
	finish := storage.AIBatchEnded
	switch st.Status {
	case batch.StatusRunning:
		expires := st.ExpiresAt
		if expires.IsZero() && rec.ExpiresAt != nil {
			expires = *rec.ExpiresAt
		}
		grace := b.ExpiryGrace
		if grace == 0 {
			grace = defaultExpiryGrace
		}
		if expires.IsZero() || now.Before(expires.Add(grace)) {
			return nil
		}
		// Past the provider's own deadline and still not ended: give up on it.
		if err := b.Submitter.Cancel(ctx, rec.ID); err != nil {
			b.Formatter.Warning("batch %s: cancel after expiry: %v", rec.ID, err)
		}
		finish = storage.AIBatchFailed
	case batch.StatusEnded:
		for r, err := range b.Submitter.Results(ctx, rec.ID) {
			if err != nil {
				return fmt.Errorf("results: %w", err)
			}
			results[r.CustomID] = r
		}
	case batch.StatusFailed:
		finish = storage.AIBatchFailed
	default:
		return fmt.Errorf("unknown status %q", st.Status)
	}

	rows, err := b.Store.ListAIBatchRequests(rec.ID)
	if err != nil {
		return fmt.Errorf("list requests: %w", err)
	}

	// Record every row's outcome first, so the in-flight set clears and the
	// usage is kept even if applying fails partway.
	var usage storage.AIBatchUsage
	for i := range rows {
		row := &rows[i]
		r, ok := results[row.CustomID]
		switch {
		case ok:
			row.Outcome = string(r.Outcome)
			if r.Err != nil {
				row.Error = r.Err.Error()
			}
			row.Usage = storage.AIBatchUsage{
				InputTokens: int64(r.Usage.InputTokens), OutputTokens: int64(r.Usage.OutputTokens),
				CacheReadTokens: int64(r.Usage.CacheReadTokens), CacheWriteTokens: int64(r.Usage.CacheWriteTokens),
			}
		case finish == storage.AIBatchFailed:
			row.Outcome = storage.AIBatchCanceled
		default:
			// Ended without a line for this request: never ran.
			row.Outcome = storage.AIBatchExpired
		}
		usage = usage.Add(row.Usage)
		if err := b.Store.ResolveAIBatchRequest(*row); err != nil {
			return fmt.Errorf("resolve %s: %w", row.CustomID, err)
		}
	}

	// Then hand each subject's rows to its kind.
	for _, group := range groupBySubject(rows) {
		k := b.kind(group[0].Kind)
		if k == nil {
			b.Formatter.Warning("batch %s: no kind %q registered for subject %s; %d rows left unapplied", rec.ID, group[0].Kind, group[0].SubjectKey, len(group))
			continue
		}
		if err := k.Apply(ctx, group, results); err != nil {
			b.Formatter.Warning("batch %s: apply %s %s: %v", rec.ID, group[0].Kind, group[0].SubjectKey, err)
		}
	}

	if err := b.Store.FinishAIBatch(rec.ID, finish, "", usage); err != nil {
		return fmt.Errorf("finish: %w", err)
	}
	b.Formatter.Info("batch %s %s: %d requests, %d succeeded, %d errored, %d expired, %d canceled; tokens in %d out %d cache read %d write %d",
		rec.Kind, rec.ID, len(rows), count(rows, storage.AIBatchSucceeded), count(rows, storage.AIBatchErrored),
		count(rows, storage.AIBatchExpired), count(rows, storage.AIBatchCanceled),
		usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens)
	return nil
}

func count(rows []storage.AIBatchRequest, outcome string) int {
	n := 0
	for _, r := range rows {
		if r.Outcome == outcome {
			n++
		}
	}
	return n
}

// groupBySubject returns rows grouped by kind and subject, each group ordered
// by chunk index, groups in first-seen order.
func groupBySubject(rows []storage.AIBatchRequest) [][]storage.AIBatchRequest {
	type key struct{ kind, subject string }
	idx := map[key]int{}
	var groups [][]storage.AIBatchRequest
	for _, r := range rows {
		k := key{r.Kind, r.SubjectKey}
		i, ok := idx[k]
		if !ok {
			i = len(groups)
			idx[k] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], r)
	}
	for _, g := range groups {
		sort.Slice(g, func(i, j int) bool { return g[i].ChunkIndex < g[j].ChunkIndex })
	}
	return groups
}

// Reconcile adopts ended batches the provider knows and the table does not,
// so a gateway retry that duplicated a submission, or a crash between the
// provider accepting a batch and the row being written, still gets its
// results applied rather than paid for twice. Only a batch whose every custom
// id parses as one of the registered kinds' is adopted; a running unknown
// batch is left for a later cycle, when it has ended and its ids are visible.
func (b *BatchStage) Reconcile(ctx context.Context) error {
	known, err := b.Submitter.List(ctx, reconcileListLimit)
	if err != nil {
		return fmt.Errorf("batch reconcile: list: %w", err)
	}
	for _, up := range known {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rec, err := b.Store.GetAIBatch(up.ID)
		if err != nil {
			return fmt.Errorf("batch reconcile: %w", err)
		}
		if rec != nil || up.Status != batch.StatusEnded {
			continue
		}
		if err := b.adopt(ctx, up); err != nil {
			b.Formatter.Warning("batch reconcile: %s: %v", up.ID, err)
		}
	}
	return nil
}

func (b *BatchStage) adopt(ctx context.Context, up batch.Batch) error {
	var rows []storage.AIBatchRequest
	kindName := ""
	for r, err := range b.Submitter.Results(ctx, up.ID) {
		if err != nil {
			return fmt.Errorf("results: %w", err)
		}
		row, k, ok := b.adoptRow(r.CustomID)
		if !ok {
			// Not ours (or not this deployment's kinds). Leave it alone.
			return nil
		}
		if kindName != "" && kindName != k {
			return nil
		}
		kindName = k
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	// Chunk counts are not in the id; derive them from the rows present.
	perSubject := map[string]int{}
	for _, r := range rows {
		perSubject[r.SubjectKey]++
	}
	for i := range rows {
		rows[i].ChunkCount = perSubject[rows[i].SubjectKey]
	}
	rec := storage.AIBatch{
		ID: up.ID, Provider: b.Submitter.Provider(), Kind: kindName, Adopted: true,
		SubmittedAt: up.CreatedAt,
	}
	if rec.SubmittedAt.IsZero() {
		rec.SubmittedAt = b.clock()
	}
	if err := b.Store.RecordAIBatch(rec, rows); err != nil {
		return fmt.Errorf("record: %w", err)
	}
	b.Formatter.Warning("batch reconcile: adopted %s %s with %d requests not in the table (prompt provenance is the current prompt, not the one sent)", kindName, up.ID, len(rows))
	return nil
}

func (b *BatchStage) adoptRow(customID string) (storage.AIBatchRequest, string, bool) {
	for _, k := range b.Kinds {
		if row, ok := k.Adopt(customID); ok {
			return row, k.Name(), true
		}
	}
	return storage.AIBatchRequest{}, "", false
}
