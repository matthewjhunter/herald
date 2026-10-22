package pipeline

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/matthewjhunter/airlock/screen"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/ai/batch"
	"github.com/matthewjhunter/herald/internal/storage"
)

// Planner is the subset of *ai.AIProcessor the batch kinds call: the prompt
// builders and parsers, never the model. Declared on the consumer side so
// kinds can be tested with the real planner and no network.
type Planner interface {
	SecurityPlan(title, content string) (*ai.SecurityPlan, error)
	ParseSecurityFinding(responseText, evidence string) (screen.Finding, error)
	SecurityVerdict(findings []screen.Finding, screened string) *ai.SecurityResult
	CurationPlan(userID int64, title, content string, keywords []string) (*ai.CurationPlan, error)
}

// batchSecurityLeaseSeconds holds a screen claim across a provider batch's
// whole life: the 24-hour expiry plus the hour of grace the poller allows
// before it gives up on a batch, so no other screener reclaims an article
// that is still in flight.
const batchSecurityLeaseSeconds = 25 * 60 * 60

// KindNames.
const (
	KindScreen = "screen"
	KindCurate = "curate"
)

// ScreenKind batches the security screen. It claims articles with the long
// lease and writes verdicts through the same thresholds as the synchronous
// stage.
type ScreenKind struct {
	Stage   *Stage
	Planner Planner
	// Mode gates submission; nil means always. Results are applied regardless.
	Mode Mode
	// Provider names the batch provider whose results Apply records, for the
	// corpus row's via column. Batch results are never marked trainable.
	Provider string
}

func (k *ScreenKind) Name() string { return KindScreen }

func (k *ScreenKind) Due(_ context.Context, limit int) ([]Subject, error) {
	if k.Mode != nil && !k.Mode(0, KindScreen) {
		return nil, nil
	}
	arts, err := k.Stage.Store.ClaimUnscreenedArticles(limit, batchSecurityLeaseSeconds)
	if err != nil {
		return nil, err
	}
	subjects := make([]Subject, 0, len(arts))
	for _, a := range arts {
		// The same deterministic skips as securityOne: these never reach a model.
		content := articleContent(a)
		if content == "" {
			k.Stage.Formatter.Warning("skipping article %d %q: no content", a.ID, a.Title)
			k.Stage.Store.SkipArticleSecurity(a.ID, "no content") //nolint:errcheck
			continue
		}
		if minLen := k.Stage.Cfg.Summarization.MinArticleLength; minLen > 0 && len(content) < minLen {
			k.Stage.Formatter.Warning("skipping article %d: content too short (%d < %d)", a.ID, len(content), minLen)
			k.Stage.Store.SkipArticleSecurity(a.ID, fmt.Sprintf("content too short (%d < %d)", len(content), minLen)) //nolint:errcheck
			continue
		}
		subjects = append(subjects, Subject{Key: strconv.FormatInt(a.ID, 10), Articles: []storage.Article{a}})
	}
	return subjects, nil
}

func (k *ScreenKind) Build(subject Subject) (Built, error) {
	a := subject.Articles[0]
	plan, err := k.Planner.SecurityPlan(a.Title, articleContent(a))
	if err != nil {
		// Unrenderable now means unrenderable next cycle too; release without
		// refunding so the attempt budget bounds it.
		k.Stage.Store.ReleaseSecurityClaim(a.ID) //nolint:errcheck
		return Built{}, err
	}
	built := Built{Requests: make([]batch.Request, len(plan.Requests)), Rows: make([]storage.AIBatchRequest, len(plan.Requests))}
	id := a.ID
	for i, req := range plan.Requests {
		req.CustomID = screenCustomID(a.ID, i)
		built.Requests[i] = req
		built.Rows[i] = storage.AIBatchRequest{
			CustomID: req.CustomID, Kind: KindScreen, SubjectKey: subject.Key, ArticleID: &id,
			ChunkIndex: i, ChunkCount: len(plan.Requests), Model: plan.Model, PromptHash: plan.PromptHash,
		}
	}
	return built, nil
}

func screenCustomID(articleID int64, chunk int) string {
	return fmt.Sprintf("s-%d-%d", articleID, chunk)
}

func (k *ScreenKind) Release(rows []storage.AIBatchRequest, outcome string) {
	seen := map[int64]bool{}
	for _, r := range rows {
		if r.ArticleID == nil || seen[*r.ArticleID] {
			continue
		}
		seen[*r.ArticleID] = true
		if outcome == storage.AIBatchErrored {
			k.Stage.Store.ReleaseSecurityClaim(*r.ArticleID) //nolint:errcheck
		} else {
			k.Stage.Store.RefundSecurityClaim(*r.ArticleID) //nolint:errcheck
		}
	}
}

// Apply assembles one article's verdict from its chunk results. Any chunk
// that did not succeed fails the whole article closed: expired and canceled
// chunks refund the attempt, errored ones spend it. The chunks are recomputed
// from the article as stored now; if the content changed in flight the chunk
// count differs and the article is released to be screened afresh.
func (k *ScreenKind) Apply(_ context.Context, rows []storage.AIBatchRequest, results map[string]batch.Result) error {
	if len(rows) == 0 || rows[0].ArticleID == nil {
		return nil
	}
	articleID := *rows[0].ArticleID
	for _, r := range rows {
		switch r.Outcome {
		case storage.AIBatchSucceeded:
		case storage.AIBatchErrored:
			k.Release(rows, storage.AIBatchErrored)
			return fmt.Errorf("article %d chunk %d errored: %s", articleID, r.ChunkIndex, r.Error)
		default:
			k.Release(rows, r.Outcome)
			return nil
		}
	}

	article, err := k.Stage.Store.GetArticle(articleID)
	if err != nil {
		k.Release(rows, "")
		return fmt.Errorf("article %d: %w", articleID, err)
	}
	if article == nil {
		return nil // deleted in flight; nothing to write
	}
	content := articleContent(*article)
	plan, err := k.Planner.SecurityPlan(article.Title, content)
	if err != nil {
		k.Release(rows, storage.AIBatchErrored)
		return fmt.Errorf("article %d: re-render: %w", articleID, err)
	}
	if len(plan.Chunks) != len(rows) {
		k.Release(rows, "")
		return fmt.Errorf("article %d: content changed in flight (%d chunks sent, %d now); released to be screened afresh", articleID, len(rows), len(plan.Chunks))
	}
	findings := make([]screen.Finding, 0, len(rows))
	for _, r := range rows {
		if r.ChunkIndex < 0 || r.ChunkIndex >= len(plan.Chunks) {
			k.Release(rows, "")
			return fmt.Errorf("article %d: chunk index %d out of range", articleID, r.ChunkIndex)
		}
		f, err := k.Planner.ParseSecurityFinding(results[r.CustomID].Text, plan.Chunks[r.ChunkIndex])
		if err != nil {
			// Fail closed, exactly as the synchronous path: an unparseable or
			// fabricated verdict spends the attempt and releases the claim.
			k.Release(rows, storage.AIBatchErrored)
			return fmt.Errorf("article %d chunk %d: %w", articleID, r.ChunkIndex, err)
		}
		findings = append(findings, f)
		threat := float64(f.Threat)
		category := f.Category
		verified := f.Verified
		r.Threat, r.Category, r.Verified = &threat, &category, &verified
		k.Stage.Store.ResolveAIBatchRequest(r) //nolint:errcheck // the finding is a diagnostic copy; the verdict below is the record
		k.Stage.recordModelOutput(storage.ModelOutput{
			Kind: storage.ModelOutputScreen, ArticleID: articleID, ChunkIndex: r.ChunkIndex, ChunkCount: len(rows),
			Model: plan.Model, PromptHash: plan.PromptHash, ContentHash: storage.ContentHash(content),
			Response: results[r.CustomID].Text, Parsed: screenParsed(f.Threat, f.Category, f.Verified),
			Via: storage.ModelOutputViaBatch(k.Provider), Trainable: false,
		})
	}
	k.Stage.recordSecurityVerdict(*article, k.Planner.SecurityVerdict(findings, plan.Screened))
	return nil
}

func (k *ScreenKind) Adopt(customID string) (storage.AIBatchRequest, bool) {
	parts := strings.Split(customID, "-")
	if len(parts) != 3 || parts[0] != "s" {
		return storage.AIBatchRequest{}, false
	}
	id, err1 := strconv.ParseInt(parts[1], 10, 64)
	chunk, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || id <= 0 || chunk < 0 {
		return storage.AIBatchRequest{}, false
	}
	return storage.AIBatchRequest{
		CustomID: customID, Kind: KindScreen, SubjectKey: parts[1], ArticleID: &id, ChunkIndex: chunk,
	}, true
}

// CurateKind batches curation for every user Users returns. There is no
// claim: the pending request rows are the in-flight set, and a request that
// did not succeed simply leaves the article eligible for the next cycle.
type CurateKind struct {
	Stage   *Stage
	Planner Planner
	// Users lists the users to curate for; nil means the stage's own user.
	Users func() ([]int64, error)
	// Mode gates submission per user; nil means always.
	Mode Mode
	// Provider names the batch provider whose results Apply records, for the
	// corpus row's via column. Batch results are never marked trainable.
	Provider string
}

func (k *CurateKind) Name() string { return KindCurate }

func (k *CurateKind) Due(_ context.Context, limit int) ([]Subject, error) {
	users := []int64{k.Stage.UserID}
	if k.Users != nil {
		var err error
		if users, err = k.Users(); err != nil {
			return nil, err
		}
	}
	var subjects []Subject
	for _, user := range users {
		if k.Mode != nil && !k.Mode(user, KindCurate) {
			continue
		}
		remaining := limit - len(subjects)
		if remaining <= 0 {
			break
		}
		arts, err := k.Stage.Store.GetUnbatchedCurationArticles(user, k.Stage.Cfg.Thresholds.MaxSecurityThreat, remaining)
		if err != nil {
			return nil, err
		}
		for _, a := range arts {
			subjects = append(subjects, Subject{Key: strconv.FormatInt(a.ID, 10), UserID: user, Articles: []storage.Article{a}})
		}
	}
	return subjects, nil
}

func (k *CurateKind) Build(subject Subject) (Built, error) {
	a := subject.Articles[0]
	plan, err := k.Planner.CurationPlan(subject.UserID, a.Title, articleContent(a), k.Stage.Cfg.Preferences.Keywords)
	if err != nil {
		return Built{}, err
	}
	req := plan.Request
	req.CustomID = curateCustomID(a.ID, subject.UserID)
	id := a.ID
	return Built{
		Requests: []batch.Request{req},
		Rows: []storage.AIBatchRequest{{
			CustomID: req.CustomID, Kind: KindCurate, UserID: subject.UserID, SubjectKey: subject.Key,
			ArticleID: &id, ChunkCount: 1, Model: plan.Model, PromptHash: plan.PromptHash,
		}},
	}, nil
}

func curateCustomID(articleID, userID int64) string {
	return fmt.Sprintf("c-%d-%d", articleID, userID)
}

func (k *CurateKind) Release([]storage.AIBatchRequest, string) {}

func (k *CurateKind) Apply(_ context.Context, rows []storage.AIBatchRequest, results map[string]batch.Result) error {
	if len(rows) == 0 || rows[0].ArticleID == nil {
		return nil
	}
	row := rows[0]
	r := results[row.CustomID]
	if row.Outcome != storage.AIBatchSucceeded {
		if row.Outcome == storage.AIBatchErrored {
			return fmt.Errorf("article %d for user %d errored: %s", *row.ArticleID, row.UserID, row.Error)
		}
		return nil
	}
	plan := &ai.CurationPlan{Model: row.Model, PromptHash: row.PromptHash, Keywords: strings.Join(k.Stage.Cfg.Preferences.Keywords, ", ")}
	res := plan.ParseCuration(r.Text)
	if err := k.Stage.Store.SetInterestScore(row.UserID, *row.ArticleID, res.InterestScore, res.Model, res.PromptHash); err != nil {
		return fmt.Errorf("article %d for user %d: %w", *row.ArticleID, row.UserID, err)
	}
	title := row.SubjectKey
	if article, err := k.Stage.Store.GetArticle(*row.ArticleID); err == nil && article != nil {
		title = article.Title
		// Hashed from the live article: the exporter compares against the
		// same, so a rewrite between submit and apply shows up as a mismatch
		// there rather than being silently recorded as the input.
		userID := row.UserID
		k.Stage.recordModelOutput(storage.ModelOutput{
			Kind: storage.ModelOutputCurate, ArticleID: *row.ArticleID, UserID: &userID,
			Model: res.Model, PromptHash: res.PromptHash, ContentHash: storage.ContentHash(articleContent(*article)),
			Keywords: optionalKeywords(res.Keywords), Response: res.Response, Parsed: curationParsed(res),
			Via: storage.ModelOutputViaBatch(k.Provider), Trainable: false,
		})
	}
	k.Stage.Formatter.Info("scored article %d (batch): interest=%.1f: %s", *row.ArticleID, res.InterestScore, title)
	return nil
}

func (k *CurateKind) Adopt(customID string) (storage.AIBatchRequest, bool) {
	parts := strings.Split(customID, "-")
	if len(parts) != 3 || parts[0] != "c" {
		return storage.AIBatchRequest{}, false
	}
	id, err1 := strconv.ParseInt(parts[1], 10, 64)
	user, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || id <= 0 || user < 0 {
		return storage.AIBatchRequest{}, false
	}
	return storage.AIBatchRequest{
		CustomID: customID, Kind: KindCurate, UserID: user, SubjectKey: parts[1], ArticleID: &id, ChunkCount: 1,
	}, true
}
