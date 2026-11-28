-- +goose Up
--
-- More than one summary, and more than one security scan, per article.
--
-- Both verdicts were single cells: article_summaries had one row per article
-- and a re-summary overwrote it, and the screen's verdict lived only in the
-- security_* columns on articles, with no record of which model produced it.
-- That made two things impossible: keeping one model's output while another
-- model's replaces it for readers, and comparing what two models said about
-- the same article.
--
-- Summaries. article_summaries becomes a history: one row per generation,
-- told apart by model, prompt hash and date. is_current marks the single row
-- per article that every reader uses -- the article page, the embedder (which
-- prefixes the summary to each chunk), group summaries -- and the partial
-- unique index holds that to one. Existing rows are all current. A skip marker
-- (ai_summary = '', skip_reason set) is a row like any other and now carries
-- the model that failed, when a model was involved.
ALTER TABLE article_summaries DROP CONSTRAINT IF EXISTS article_summaries_pkey;
ALTER TABLE article_summaries ADD COLUMN id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY;
ALTER TABLE article_summaries ADD COLUMN is_current BOOLEAN NOT NULL DEFAULT TRUE;
CREATE UNIQUE INDEX idx_article_summaries_current ON article_summaries(article_id) WHERE is_current;
-- "Does this article have a row from model X" is the regeneration queue's test.
CREATE INDEX idx_article_summaries_article_model ON article_summaries(article_id, model);

-- Screens. The security_* columns on articles stay exactly what they were:
-- the verdict that gates the article. article_screens is the history beside
-- them, one row per scan. effective says whether that scan set the gating
-- verdict; a scan recorded with effective = false is a shadow, kept for
-- comparison, that changed nothing about what readers can see.
--
-- Like the columns it mirrors, a row is payload-free (plan 012): a threat
-- number, a closed-vocabulary category, a flag. Never the model's prose.
CREATE TABLE article_screens (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    article_id  BIGINT NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    model       TEXT NOT NULL,
    prompt_hash TEXT,
    -- The combined verdict, as stored on the article: max(model, regex).
    threat      DOUBLE PRECISION NOT NULL,
    category    TEXT,
    verified    BOOLEAN,
    flagged     BOOLEAN NOT NULL DEFAULT FALSE,
    -- 'sync' or 'batch:<provider>', as in model_outputs.
    via         TEXT NOT NULL,
    effective   BOOLEAN NOT NULL,
    screened_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_article_screens_article_model ON article_screens(article_id, model);

-- Attribute the verdicts that can be attributed. Since migration 0021 every
-- screen call has left a model_outputs row written moments before its verdict,
-- so an article whose latest such row sits beside its security_screened_at was
-- screened by that row's model. Articles screened before 0021 get no row:
-- which model screened them was never recorded and cannot be recovered.
INSERT INTO article_screens (article_id, model, prompt_hash, threat, category, verified, flagged, via, effective, screened_at)
SELECT a.id, mo.model, mo.prompt_hash, a.security_threat, a.security_category, a.security_verified,
       a.security_flagged, mo.via, TRUE, a.security_screened_at
FROM articles a
JOIN LATERAL (
    SELECT m.model, m.prompt_hash, m.via, m.created_at
    FROM model_outputs m
    WHERE m.article_id = a.id AND m.kind = 'screen'
    ORDER BY m.created_at DESC, m.id DESC
    LIMIT 1
) mo ON TRUE
WHERE a.security_threat IS NOT NULL
  AND a.security_screened_at IS NOT NULL
  AND mo.created_at BETWEEN a.security_screened_at - INTERVAL '10 minutes'
                        AND a.security_screened_at + INTERVAL '10 minutes';

-- +goose Down
DROP TABLE IF EXISTS article_screens;
-- Back to one row per article: only the current one survives.
DELETE FROM article_summaries WHERE NOT is_current;
DROP INDEX IF EXISTS idx_article_summaries_article_model;
DROP INDEX IF EXISTS idx_article_summaries_current;
ALTER TABLE article_summaries DROP COLUMN IF EXISTS is_current;
ALTER TABLE article_summaries DROP COLUMN IF EXISTS id;
ALTER TABLE article_summaries ADD PRIMARY KEY (article_id);
