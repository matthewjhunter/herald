-- +goose Up
--
-- One vector per chunk, not one per article (#286).
--
-- nomic-embed-text truncates hard at 2048 tokens, and 9-41% of the corpus
-- exceeds that after being clipped to the model's byte budget -- roughly 30-40%
-- of an affected article silently discarded. Ollama truncates without saying so,
-- so long articles have been indexed from their opening third for as long as
-- this has been deployed. The fix is to split an article into chunks that fit
-- and embed each one.
--
-- That means an article is a set of vectors rather than a point, so the vectors
-- move out of article_embeddings into their own table. They are NOT pooled back
-- into one vector per article: averaging a long article into a single point is
-- the precise failure chunking exists to fix.
--
-- article_embeddings keeps its one row per article and loses its vector column.
-- What is left there is bookkeeping -- which model, whether the article embedded
-- at all, how many attempts, the last error -- and all of that is genuinely a
-- property of the article, not of any one chunk. Keeping the split means the
-- retry machinery (GetArticlesWithoutEmbeddings, ResetStuckEmbeddings, the
-- status sentinels) needs no ordinal-awareness at all.
--
-- Existing vectors are deleted rather than migrated forward as ordinal 0. They
-- were produced from clipped text without the article summary that now prefixes
-- every chunk, so they sit in a different region of the space than anything
-- produced after this migration; mixing the two would quietly degrade grouping
-- rather than loudly fail. As with 0003, embeddings are cheap to recompute:
-- clearing them makes every article look unembedded and the embed stage
-- repopulates over the next cycles through its normal retry path. Group
-- centroids are likewise cleared and rebuilt by the cluster stage's repair pass
-- as members re-embed.

CREATE TABLE IF NOT EXISTS article_embedding_chunks (
    article_id      BIGINT NOT NULL,
    embedding_model TEXT NOT NULL,
    -- Position of the chunk within the article, from 0.
    ordinal         INTEGER NOT NULL,
    embedding       vector(768) NOT NULL,
    -- Byte offsets of the chunk in the text that was split, so a chunk hit can
    -- be traced back to the span of the article it came from (the parent-child
    -- retrieval pattern). Offsets address the stripped embed body, not the
    -- stored article HTML.
    start_byte      INTEGER NOT NULL,
    end_byte        INTEGER NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (article_id, embedding_model, ordinal),
    FOREIGN KEY (article_id) REFERENCES articles(id) ON DELETE CASCADE
);

-- No vector index, for the same reason article_embeddings had none: every
-- distance query here either runs over an explicit bounded id set (the cluster
-- stage's recency window) or is an exact scan filtered to one user's subscribed
-- feeds by a JOIN that an HNSW iterative scan cannot see (#192).

DELETE FROM article_embeddings;
ALTER TABLE article_embeddings DROP COLUMN embedding;

UPDATE article_groups SET embedding = NULL, embedding_model = '';

-- +goose Down
ALTER TABLE article_embeddings ADD COLUMN embedding vector(768);
DELETE FROM article_embeddings;
DROP TABLE IF EXISTS article_embedding_chunks;
UPDATE article_groups SET embedding = NULL, embedding_model = '';
