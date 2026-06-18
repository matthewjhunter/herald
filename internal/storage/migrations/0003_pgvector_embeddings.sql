-- +goose Up
--
-- Move article and group embeddings from raw BYTEA blobs to pgvector's
-- vector(768) type so similarity and grouping run as in-database ANN instead of
-- fetch-all-and-cosine-in-Go (#186). 768 is the dimension of nomic-embed-text,
-- herald's default (and only deployed) embedding model; switching to a model of
-- a different dimension would need a new migration to widen the column.
--
-- Existing vectors are NOT converted -- they are dropped and regenerated. The
-- old blobs are little-endian float32 with no in-SQL decode path, and embeddings
-- are cheap to recompute: clearing article_embeddings makes every article look
-- unembedded, so the embed stage repopulates it over the next few cycles via its
-- normal retry machinery. Group centroids are likewise cleared and rebuilt by
-- the cluster stage's centroid-repair pass as members re-embed.
--
-- DEPLOY ORDER: the shared homelab Postgres runs herald as a non-superuser role,
-- and pgvector's `vector` extension is not trusted, so herald cannot CREATE it.
-- The extension must be provisioned out-of-band (homelab host_vars) BEFORE this
-- migration runs, or startup fails here. CREATE EXTENSION IF NOT EXISTS is then a
-- no-op in prod; in CI and single-node compose, herald is a superuser and this
-- statement provisions the extension itself.
CREATE EXTENSION IF NOT EXISTS vector;

-- article_embeddings: clear all rows (regenerate), then retype the column to a
-- nullable vector. NULL now marks non-ok rows (status 1=too_short, 2=error),
-- replacing the one-byte placeholder the old NOT NULL BYTEA column required.
DELETE FROM article_embeddings;
ALTER TABLE article_embeddings DROP COLUMN embedding;
ALTER TABLE article_embeddings ADD COLUMN embedding vector(768);

-- article_groups: drop the BYTEA centroid, add the vector centroid (nullable --
-- a group has no centroid until its members are embedded), and reset the model
-- tag so the centroid-repair pass treats every existing group as needing a fresh
-- centroid.
ALTER TABLE article_groups DROP COLUMN embedding;
ALTER TABLE article_groups ADD COLUMN embedding vector(768);
UPDATE article_groups SET embedding_model = '';

-- HNSW index on the centroids: the JOIN phase orders a user's group centroids by
-- cosine distance to each incoming article, so this is the per-user ANN target
-- that grows with grouping activity. article_embeddings needs no vector index --
-- its only distance queries run over an explicit, bounded id set (the recency
-- window), selected by the article_id primary key, not by a global nearest scan.
CREATE INDEX idx_article_groups_embedding ON article_groups
    USING hnsw (embedding vector_cosine_ops);

-- +goose Down
DROP INDEX IF EXISTS idx_article_groups_embedding;

ALTER TABLE article_groups DROP COLUMN embedding;
ALTER TABLE article_groups ADD COLUMN embedding BYTEA;

DELETE FROM article_embeddings;
ALTER TABLE article_embeddings DROP COLUMN embedding;
ALTER TABLE article_embeddings ADD COLUMN embedding BYTEA NOT NULL;

-- The vector extension is left installed: other databases on a shared instance
-- may depend on it, and dropping it here would fail while any vector column or
-- index remains.
