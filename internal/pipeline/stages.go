package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/storage"
)

// Security screens each article and records the verdict on the article itself
// (#141): maliciousness is a property of the content, so each article is
// screened once and the verdict is shared by every subscriber. It is the gate
// for the whole pipeline: only articles that clear the full security threshold
// are returned (for summarization, scoring, embedding, and clustering). Articles
// with no content or too little content are marked skipped; hard-blocked and
// medium-flagged articles are recorded with their score (and so excluded from
// the passing set); a model-failure increments the per-article retry budget
// unless the backend was simply unavailable (#90, #100).
//
// This stage runs in the global, once-per-cycle security pass (RunSecurity), not
// in the per-user pipeline — there is no user_id in the verdict.
func (s *Stage) Security(ctx context.Context, in []storage.Article) []storage.Article {
	if !s.AI.BackendAvailable() {
		s.Formatter.Warning("pipeline: skipping security stage — AI backend unavailable (breaker open)")
		// The drain already claimed this batch (attempt +1, claim stamped) while
		// the breaker looked closed; it can open in the window before we run. Refund
		// every claim rather than dropping the batch: we never screened these, so a
		// backend outage must not burn their retry budget (#100). Without this, an
		// olla restart parks whole batches at security_attempts>=3 with claims still
		// held, stranding the queue until an operator resets them.
		for _, a := range in {
			s.Store.RefundSecurityClaim(a.ID) //nolint:errcheck
		}
		return nil
	}
	return s.mapArticles(ctx, in, s.securityOne)
}

func (s *Stage) securityOne(ctx context.Context, article storage.Article) *storage.Article {
	// The drain already claimed this article (attempt +1, claim stamped). If the
	// ctx was cancelled before we got here (daemon shutdown mid-batch), we never
	// screen it, so refund the claim rather than letting it strand at a burned
	// attempt with a held claim (#245).
	if ctx.Err() != nil {
		s.Store.RefundSecurityClaim(article.ID) //nolint:errcheck
		return nil
	}

	content := articleContent(article)
	if content == "" {
		s.Formatter.Warning("skipping article %d %q: no content", article.ID, article.Title)
		// Mark screened-but-skipped (NULL score) so it leaves the queue without
		// polluting security metrics and is distinguishable from "not screened".
		s.Store.SkipArticleSecurity(article.ID, "no content") //nolint:errcheck
		return nil
	}

	minLen := s.Cfg.Summarization.MinArticleLength
	if minLen > 0 && len(content) < minLen {
		s.Formatter.Warning("skipping article %d: content too short (%d < %d)", article.ID, len(content), minLen)
		s.Store.SkipArticleSecurity(article.ID, fmt.Sprintf("content too short (%d < %d)", len(content), minLen)) //nolint:errcheck
		return nil
	}

	secResult, err := s.AI.SecurityCheck(ctx, article.Title, content)
	if err != nil {
		s.Formatter.Warning("security check failed for article %d: %v", article.ID, err)
		// The attempt was counted when the article was claimed (#233). A genuine
		// model-response failure keeps that attempt (bounding retries) and just
		// releases the claim so it can be retried before the lease expires. A
		// backend-unavailable error means we never got a verdict, so refund the
		// attempt too -- an olla outage must not burn the retry budget (#100).
		if errors.Is(err, ai.ErrBackendUnavailable) {
			s.Store.RefundSecurityClaim(article.ID) //nolint:errcheck
		} else {
			s.Store.ReleaseSecurityClaim(article.ID) //nolint:errcheck
		}
		return nil
	}

	// Threat scale (plan 012): 0 = clean, higher = worse. A LOW threat passes.
	//   threat <= passThreat            -> pass, queued for curation
	//   passThreat < threat <= border   -> borderline, flagged for audit, excluded
	//   threat > border                 -> hard block, excluded
	passThreat := s.Cfg.Thresholds.MaxSecurityThreat
	if passThreat == 0 {
		passThreat = 3.0
	}
	borderThreat := s.Cfg.Thresholds.SecurityBorderlineThreat
	if borderThreat == 0 {
		borderThreat = 6.0
	}

	if secResult.Threat > borderThreat {
		// Hard block: above the borderline ceiling. Record the verdict; the threat
		// gates it out of every downstream (passing) query.
		s.Store.ScreenArticleSecurity(article.ID, secResult.Threat, secResult.Category, secResult.Verified, false) //nolint:errcheck
		s.Formatter.OutputProcessingStatus(article.ID, article.Title, 0, secResult.Threat, false)
		return nil
	}

	if secResult.Threat > passThreat {
		// Borderline: clears the hard block but not the pass ceiling. Flag for audit.
		s.Store.ScreenArticleSecurity(article.ID, secResult.Threat, secResult.Category, secResult.Verified, true) //nolint:errcheck
		s.Formatter.OutputProcessingStatus(article.ID, article.Title, 0, secResult.Threat, false)
		return nil
	}

	// Passed (low threat). Record the verdict on the article; the per-user
	// curation stage picks it up via the threat ceiling and assigns each user's
	// interest.
	if err := s.Store.ScreenArticleSecurity(article.ID, secResult.Threat, secResult.Category, secResult.Verified, false); err != nil {
		s.Formatter.Warning("failed to record security verdict for article %d: %v", article.ID, err)
		return nil
	}
	s.Formatter.Debug("article %d passed security (threat %.1f), queued for curation: %s", article.ID, secResult.Threat, article.Title)
	return &article
}

// Summarize generates and caches an AI summary for each (already security-passed)
// article that lacks one. Like the security verdict (#141), the summary is a
// property of the article, shared by every subscriber (#162) — this stage runs
// in the global, once-per-cycle summarize pass (RunSummaries), not in the
// per-user pipeline. Articles that already have a cached summary pass through
// unchanged. Transient failures (backend error, garbled output) are left
// unsummarized for a later cycle to retry; deterministic rejections (summary not
// shorter than the source, or over the length budget) are marked skipped so they
// are not retried forever. Returns the articles that now have a usable summary.
func (s *Stage) Summarize(ctx context.Context, in []storage.Article) []storage.Article {
	if !s.AI.BackendAvailable() {
		s.Formatter.Warning("pipeline: skipping summarize stage — AI backend unavailable (breaker open)")
		return nil
	}
	return s.mapArticles(ctx, in, s.summarizeOne)
}

func (s *Stage) summarizeOne(ctx context.Context, article storage.Article) *storage.Article {
	existing, err := s.Store.GetArticleSummary(article.ID)
	if err != nil {
		s.Formatter.Warning("failed to check article summary for %d: %v", article.ID, err)
		return nil
	}
	if existing != nil {
		// Already summarized (or marked skipped) — advance unchanged.
		return &article
	}

	content := articleContent(article)
	maxLen := s.Cfg.Summarization.MaxSummaryLength
	summary, err := s.AI.SummarizeArticle(ctx, article.Title, content, maxLen)
	if err != nil {
		// Transient — leave unsummarized, retry next cycle.
		s.Formatter.Warning("summarization failed for article %d: %v", article.ID, err)
		return nil
	}
	if ai.LooksLikeGarbage(summary) {
		// Mostly load-related, sometimes recovers under lower load — retry.
		s.Formatter.Warning("discarding garbled summary for article %d", article.ID)
		return nil
	}
	// Deterministic rejections: the model can't compress this content any
	// further. Mark skipped so we stop retrying, and still advance the article.
	if len(summary) > len(content) {
		reason := fmt.Sprintf("summary longer than content (%d > %d)", len(summary), len(content))
		s.Formatter.Warning("marking article %d summarization skipped: %s", article.ID, reason)
		s.Store.MarkSummarizationSkipped(article.ID, reason) //nolint:errcheck
		return &article
	}
	if maxLen > 0 && len(summary) > maxLen+maxLen*15/100 {
		reason := fmt.Sprintf("summary exceeds max length by >15%% (%d > %d)", len(summary), maxLen)
		s.Formatter.Warning("marking article %d summarization skipped: %s", article.ID, reason)
		s.Store.MarkSummarizationSkipped(article.ID, reason) //nolint:errcheck
		return &article
	}

	if err := s.Store.UpdateArticleAISummary(article.ID, summary); err != nil {
		s.Formatter.Warning("failed to cache AI summary for %d: %v", article.ID, err)
		return nil
	}
	return &article
}

// Curate assigns an interest score to each security-passed article via the
// curation model and records it. Run as its own stage (separate from security)
// so a distinct curation model can be used and so the order is defined. Articles
// whose curation fails are left with interest_score NULL for a later cycle.
// Returns the articles that were scored.
func (s *Stage) Curate(ctx context.Context, in []storage.Article) []storage.Article {
	if !s.AI.BackendAvailable() {
		s.Formatter.Warning("pipeline: skipping curation stage for user %d — AI backend unavailable (breaker open)", s.UserID)
		return nil
	}
	// The security verdict lives on the article, not on the in-memory Article
	// struct, so fetch it once for the batch to report alongside interest (#119).
	batchIDs := make([]int64, len(in))
	for i, a := range in {
		batchIDs[i] = a.ID
	}
	secScores, err := s.Store.GetArticleSecurityScores(batchIDs)
	if err != nil {
		s.Formatter.Warning("pipeline: could not load security scores for curation reporting: %v", err)
		secScores = nil
	}
	return s.mapArticles(ctx, in, func(ctx context.Context, article storage.Article) *storage.Article {
		return s.curateOne(ctx, article, secScores[article.ID])
	})
}

func (s *Stage) curateOne(ctx context.Context, article storage.Article, securityScore float64) *storage.Article {
	content := articleContent(article)
	curResult, err := s.AI.CurateArticle(ctx, s.UserID, article.Title, content, s.Cfg.Preferences.Keywords)
	if err != nil {
		s.Formatter.Warning("curation failed for article %d: %v", article.ID, err)
		return nil
	}
	// Record only the interest score; the security verdict was written by the
	// security stage and must not be clobbered (SetInterestScore leaves it).
	// Model and prompt hash go down with it: this is the one point where which
	// prompt produced this score is known, and reconstructing it later reads
	// back whatever is current instead (#258).
	s.Store.SetInterestScore(s.UserID, article.ID, curResult.InterestScore, //nolint:errcheck
		curResult.Model, curResult.PromptHash)
	s.Formatter.OutputProcessingStatus(article.ID, article.Title, curResult.InterestScore, securityScore, true)
	s.Formatter.Info("scored article %d: interest=%.1f security=%.1f: %s", article.ID, curResult.InterestScore, securityScore, article.Title)
	return &article
}
