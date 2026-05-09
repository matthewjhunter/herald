package storage

// EmbedStatus is the lifecycle state of an article_embeddings row.
//
// Stored as a SMALLINT in the database — 1-2 bytes vs ~5 for an enum-like
// TEXT, and integer comparisons are cheaper than text comparisons if the
// column ever needs an index. The constants below are the only valid
// values; storage methods are the only writers, so the value space stays
// closed without a CHECK constraint.
type EmbedStatus int16

const (
	// EmbedStatusOK marks a row whose `embedding` column contains a valid
	// vector. attempts=0, error_message=NULL.
	EmbedStatusOK EmbedStatus = 0

	// EmbedStatusTooShort marks a row that was deterministically skipped
	// because the article body fell below the minimum embedding length.
	// `embedding` holds placeholder bytes that the application never reads.
	// Never retried — content is persistently too short.
	EmbedStatusTooShort EmbedStatus = 1

	// EmbedStatusError marks a row whose embed call failed transiently
	// (backend error, network blip, model not loaded, etc.). Eligible for
	// retry while `attempts < EmbedMaxAttempts`. error_message holds the
	// last failure text for diagnosis.
	EmbedStatusError EmbedStatus = 2
)

// EmbedMaxAttempts caps retries on EmbedStatusError rows. After this many
// failures the row stays in the error state but is no longer returned by
// GetArticlesWithoutEmbeddings — operators can manually clear sentinels
// (e.g. via a "herald backfill embeddings" CLI) to reset the budget.
const EmbedMaxAttempts = 5
