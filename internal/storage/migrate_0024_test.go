package storage

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
)

// migrateDownTo rolls the schema back to version, so the next NewStore applies
// every later migration over whatever the test seeded in between. It runs the
// real Down sections rather than editing goose's version table, which stops
// working the moment a later migration exists.
func migrateDownTo(t *testing.T, db *sql.DB, version int64) {
	t.Helper()
	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.DownTo(db, "migrations", version); err != nil {
		t.Fatalf("goose down to %d: %v", version, err)
	}
}

// TestMigration0024 covers what the migration does to a live database: the
// summaries already there all become current rows, and the screen verdicts
// that can be attributed are, while the ones that cannot are left alone.
func TestMigration0024(t *testing.T) {
	dsn, drop := testDSN(t)
	defer drop()

	s, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	attributed := historyArticle(t, s, "attr")
	legacy := historyArticle(t, s, "legacy")
	restale := historyArticle(t, s, "stale")
	unscreened := historyArticle(t, s, "unscreened")
	s.Close()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	migrateDownTo(t, db, 23)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	// The pre-0024 shape: one summary row per article, keyed by article.
	exec(`INSERT INTO article_summaries (article_id, ai_summary, model) VALUES ($1, 'old cloud summary', 'cloud-model')`, attributed)
	exec(`INSERT INTO article_summaries (article_id, ai_summary, skip_reason) VALUES ($1, '', 'no summarizable prose (3 < 100 chars)')`, legacy)

	const verdict = `UPDATE articles SET security_threat = $2, security_category = $3, security_verified = $4,
		security_flagged = $5, security_screened_at = $6::timestamptz WHERE id = $1`
	const output = `INSERT INTO model_outputs (kind, article_id, chunk_index, chunk_count, model, prompt_hash, content_hash, response, via, trainable, created_at)
		VALUES ('screen', $1, $2, 2, $3, 'ph', 'ch', '{}', $4, false, $5::timestamptz)`

	// Screened in two chunks by a batch, verdict written right after.
	exec(verdict, attributed, 5.0, "instruction_override", true, true, "2026-11-01 12:00:05+00")
	exec(output, attributed, 0, "cloud-model", "batch:anthropic", "2026-11-01 12:00:04+00")
	exec(output, attributed, 1, "cloud-model", "batch:anthropic", "2026-11-01 12:00:04+00")
	// Screened before model outputs were kept: nothing to attribute it to.
	exec(verdict, legacy, 0.0, "none", false, false, "2026-08-01 09:00:00+00")
	// A model output exists but belongs to an earlier scan than the verdict
	// on the article, so it says nothing about who produced that verdict.
	exec(verdict, restale, 2.0, "none", false, false, "2026-11-10 09:00:00+00")
	exec(output, restale, 0, "cloud-model", "sync", "2026-11-02 09:00:00+00")
	// Has a model output but no verdict (the call's verdict was rejected).
	exec(output, unscreened, 0, "cloud-model", "sync", "2026-11-03 09:00:00+00")

	s, err = NewStore(dsn)
	if err != nil {
		t.Fatalf("reopen (apply 0024): %v", err)
	}
	defer s.Close()

	for _, id := range []int64{attributed, legacy} {
		sum, err := s.GetArticleSummary(id)
		if err != nil || sum == nil || !sum.IsCurrent || sum.ID == 0 {
			t.Errorf("article %d: summary after migration = %+v, %v; want its existing row, current, with an id", id, sum, err)
		}
	}
	// And the migrated table takes a second summary for the same article.
	if err := s.UpdateArticleAISummary(attributed, "new local summary", SummaryProvenance{Model: "local-model"}); err != nil {
		t.Fatalf("second summary after migration: %v", err)
	}
	if all, _ := s.ListArticleSummaries(attributed); len(all) != 2 {
		t.Errorf("summary history = %d rows, want 2", len(all))
	}

	scans, err := s.ListArticleScreens(attributed)
	if err != nil {
		t.Fatalf("ListArticleScreens: %v", err)
	}
	if len(scans) != 1 {
		t.Fatalf("attributed article has %d screen rows, want 1 (one per scan, not per chunk)", len(scans))
	}
	got := scans[0]
	if got.Model != "cloud-model" || got.PromptHash != "ph" || got.Via != "batch:anthropic" || !got.Effective ||
		got.Threat != 5 || got.Category != "instruction_override" || !got.Verified || !got.Flagged {
		t.Errorf("backfilled scan = %+v, want the article's verdict attributed to cloud-model", got)
	}
	if want := "2026-11-01T12:00:05Z"; got.ScreenedAt.UTC().Format("2006-01-02T15:04:05Z") != want {
		t.Errorf("backfilled screened_at = %v, want %s", got.ScreenedAt.UTC(), want)
	}
	for name, id := range map[string]int64{"legacy": legacy, "stale": restale, "unscreened": unscreened} {
		if scans, _ := s.ListArticleScreens(id); len(scans) != 0 {
			t.Errorf("%s article got %d screen rows, want none: its verdict cannot be attributed", name, len(scans))
		}
	}
}
