package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/matthewjhunter/herald"
)

// poller runs a background feed-fetch and article-scoring loop.
type poller struct {
	engine    *herald.Engine
	userID    int64
	interval  time.Duration
	threshold float64

	mu   sync.Mutex
	done chan struct{}
}

func newPoller(engine *herald.Engine, userID int64, interval time.Duration, threshold float64) *poller {
	return &poller{
		engine:    engine,
		userID:    userID,
		interval:  interval,
		threshold: threshold,
		done:      make(chan struct{}),
	}
}

// start launches the background poll loop. It polls immediately, then on
// each tick of the configured interval.
func (p *poller) start(ctx context.Context) {
	go p.loop(ctx)
	log.Printf("poller: started (interval=%s, threshold=%.1f)", p.interval, p.threshold)
}

// stop signals the poll loop to exit.
func (p *poller) stop() {
	close(p.done)
	log.Printf("poller: stopped")
}

// poll runs a single fetch-score cycle. Exported for the poll_now MCP tool.
func (p *poller) poll(ctx context.Context) (*herald.FetchResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	result, err := p.engine.FetchAllFeeds(ctx)
	if err != nil {
		return nil, err
	}

	unsummarized, unscored, pendErr := p.engine.PendingCounts(p.userID)
	if pendErr != nil {
		log.Printf("poller: pending counts: %v", pendErr)
	}

	log.Printf("poller: %d/%d feeds downloaded, %d not modified, %d errors, %d new articles, %d pending content scan, %d pending security scan",
		result.FeedsDownloaded, result.FeedsTotal,
		result.FeedsNotModified, result.FeedsErrored,
		result.NewArticles, unsummarized, unscored)

	if result.NewArticles == 0 {
		return result, nil
	}

	scored, err := p.engine.ProcessNewArticles(ctx, p.userID)
	if err != nil {
		return result, err
	}

	var highCount int
	for _, s := range scored {
		if s.InterestScore >= p.threshold {
			highCount++
		}
	}

	result.ProcessedCount = len(scored)
	result.HighInterest = highCount

	if highCount > 0 {
		log.Printf("poller: %d high-interest articles (of %d scored)", highCount, len(scored))
	}

	return result, nil
}

func (p *poller) loop(ctx context.Context) {
	if _, err := p.poll(ctx); err != nil {
		log.Printf("poller: initial poll error: %v", err)
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := p.poll(ctx); err != nil {
				log.Printf("poller: poll error: %v", err)
			}
		}
	}
}
