package main

import (
	"context"

	herald "github.com/matthewjhunter/herald"
	"github.com/matthewjhunter/herald/internal/ai"
	"github.com/matthewjhunter/herald/internal/output"
	"github.com/matthewjhunter/herald/internal/storage"
	"golang.org/x/sync/errgroup"
)

// articleAI is the subset of *ai.AIProcessor used to screen and score a single
// article. It is declared on the consumer side so the screening pipeline can be
// unit-tested with a fake that records which model calls happen, and in what
// order.
type articleAI interface {
	SecurityCheck(ctx context.Context, userID int64, title, content string) (*ai.SecurityResult, error)
	SummarizeArticle(ctx context.Context, userID int64, title, content string, maxSummaryLength int) (string, error)
	CurateArticle(ctx context.Context, userID int64, title, content string, keywords []string) (*ai.CurationResult, error)
}

// articleScoreStore is the subset of storage.Store that screenAndScoreArticle
// persists through. Narrowed so tests need only a tiny fake.
type articleScoreStore interface {
	GetArticleSummary(userID, articleID int64) (*storage.ArticleSummary, error)
	UpdateArticleAISummary(userID, articleID int64, aiSummary string) error
	UpdateReadState(userID, articleID int64, read bool, interestScore, securityScore *float64, securityReason *string, securityFlagged *bool) error
	IncrementAIRetries(userID, articleID int64) error
}

// screenOutcome reports what happened to one article so the caller can decide
// whether to proceed to group matching.
type screenOutcome struct {
	scored    bool   // article passed security screening and was interest-scored
	aiSummary string // summary for group matching (empty unless scored with a usable summary)
}

// screenAndScoreArticle runs the security screen for one article and, only if it
// passes, summarizes and interest-scores it.
//
// The security check GATES every downstream model call: an article that fails
// the screen, is hard-blocked, or is flagged at the medium threshold never
// reaches the summarizer or curator, and no summary is generated or cached for
// it. This fixes the prior structure, where summarization ran concurrently with
// the security check and cached a summary regardless of the verdict (#90).
func screenAndScoreArticle(ctx context.Context, aiProc articleAI, store articleScoreStore, formatter *output.Formatter, appCfg *storage.Config, userID int64, article storage.Article, content string) screenOutcome {
	// 1. Security check first — it gates all downstream model calls.
	secResult, err := aiProc.SecurityCheck(ctx, userID, article.Title, content)
	if err != nil {
		formatter.Warning("security check failed for article %d: %v", article.ID, err)
		// Re-queue; after 3 failures the article falls out of the unscored query
		// and is not retried further.
		store.IncrementAIRetries(userID, article.ID) //nolint:errcheck
		return screenOutcome{}
	}

	mediumScore := appCfg.Thresholds.SecurityMediumScore
	if mediumScore == 0 {
		mediumScore = 4.0
	}

	if !secResult.Safe || secResult.Score < mediumScore {
		// Hard block: below the lower threshold entirely.
		secScore := secResult.Score
		interestScore := 0.0
		store.UpdateReadState(userID, article.ID, false, &interestScore, &secScore, &secResult.Reasoning, nil) //nolint:errcheck
		formatter.OutputProcessingStatus(article.ID, article.Title, interestScore, secScore, false)
		return screenOutcome{}
	}

	if secResult.Score < appCfg.Thresholds.SecurityScore {
		// Medium: clears the lower threshold but not the full one. Let through
		// without AI processing; flag for audit.
		secScore := secResult.Score
		interestScore := 0.0
		flagged := true
		store.UpdateReadState(userID, article.ID, false, &interestScore, &secScore, &secResult.Reasoning, &flagged) //nolint:errcheck
		formatter.OutputProcessingStatus(article.ID, article.Title, interestScore, secScore, false)
		return screenOutcome{}
	}

	// Passed security screening. Summarize and curate concurrently — both
	// operate only on screened content and are independent of each other.
	var (
		aiSummary string
		curResult *ai.CurationResult
		curErr    error
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		aiSummary = summarizeArticle(gctx, aiProc, store, formatter, appCfg, userID, article, content)
		return nil
	})
	g.Go(func() error {
		curResult, curErr = aiProc.CurateArticle(gctx, userID, article.Title, content, appCfg.Preferences.Keywords)
		return nil
	})
	g.Wait() //nolint:errcheck

	if curErr != nil {
		formatter.Warning("curation failed for article %d: %v", article.ID, curErr)
		return screenOutcome{}
	}

	secScore := secResult.Score
	interestScore := curResult.InterestScore
	store.UpdateReadState(userID, article.ID, false, &interestScore, &secScore, &secResult.Reasoning, nil) //nolint:errcheck
	formatter.OutputProcessingStatus(article.ID, article.Title, interestScore, secScore, true)
	return screenOutcome{scored: true, aiSummary: aiSummary}
}

// summarizeArticle returns the cached summary if present, otherwise generates,
// validates, and caches one. All failures are non-fatal (logged); it returns ""
// when no usable summary is available.
func summarizeArticle(ctx context.Context, aiProc articleAI, store articleScoreStore, formatter *output.Formatter, appCfg *storage.Config, userID int64, article storage.Article, content string) string {
	existing, err := store.GetArticleSummary(userID, article.ID)
	if err != nil {
		formatter.Warning("failed to check article summary for %d: %v", article.ID, err)
		return ""
	}
	if existing != nil {
		return existing.AISummary
	}
	summary, err := aiProc.SummarizeArticle(ctx, userID, article.Title, content, appCfg.Summarization.MaxSummaryLength)
	if err != nil {
		formatter.Warning("summarization failed for article %d: %v", article.ID, err)
		return ""
	}
	if herald.LooksLikeGarbage(summary) {
		formatter.Warning("discarding garbled summary for article %d", article.ID)
		return ""
	}
	if err := store.UpdateArticleAISummary(userID, article.ID, summary); err != nil {
		formatter.Warning("failed to cache AI summary for %d: %v", article.ID, err)
	}
	return summary
}
