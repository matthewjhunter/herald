-- Model output corpus (docs/model-outputs.md, migration 0021).

-- name: InsertModelOutput :one
INSERT INTO model_outputs (
    kind, article_id, user_id, chunk_index, chunk_count,
    model, prompt_hash, content_hash, keywords,
    response, parsed, via, trainable
) VALUES (
    @kind, @article_id, sqlc.narg('user_id')::bigint, @chunk_index, @chunk_count,
    @model, @prompt_hash, @content_hash, sqlc.narg('keywords')::text,
    @response, sqlc.narg('parsed')::jsonb, @via, @trainable
)
RETURNING id;

-- name: ListModelOutputsByArticle :many
SELECT id, kind, article_id, user_id, chunk_index, chunk_count,
       model, prompt_hash, content_hash, keywords,
       response, parsed, via, trainable, created_at
FROM model_outputs
WHERE article_id = @article_id
ORDER BY id;

-- name: ListTrainableModelOutputs :many
-- The exporter's page: trainable rows of one kind after a cursor, oldest first.
SELECT id, kind, article_id, user_id, chunk_index, chunk_count,
       model, prompt_hash, content_hash, keywords,
       response, parsed, via, trainable, created_at
FROM model_outputs
WHERE kind = @kind AND trainable AND id > @after_id
ORDER BY id
LIMIT @lim;
