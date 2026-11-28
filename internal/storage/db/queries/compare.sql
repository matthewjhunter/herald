-- Model comparison (`herald compare`). Each side is a model's latest row per
-- article, so a model that scanned or summarized an article twice counts once.
-- Only articles with a row from both models are compared.

-- name: CompareScreenBuckets :many
-- Verdict agreement as a 3x3 count: bucket 0 = pass, 1 = borderline, 2 = block,
-- by the thresholds passed in.
WITH a AS (
  SELECT DISTINCT ON (article_id) article_id, threat
  FROM article_screens WHERE model = @model_a::text
  ORDER BY article_id, screened_at DESC, id DESC
), b AS (
  SELECT DISTINCT ON (article_id) article_id, threat
  FROM article_screens WHERE model = @model_b::text
  ORDER BY article_id, screened_at DESC, id DESC
)
SELECT (CASE WHEN a.threat <= @pass::double precision THEN 0
             WHEN a.threat <= @border::double precision THEN 1 ELSE 2 END)::int AS bucket_a,
       (CASE WHEN b.threat <= @pass::double precision THEN 0
             WHEN b.threat <= @border::double precision THEN 1 ELSE 2 END)::int AS bucket_b,
       COUNT(*)::int AS n,
       COALESCE(SUM(ABS(a.threat - b.threat)), 0)::double precision AS abs_delta
FROM a JOIN b ON b.article_id = a.article_id
GROUP BY 1, 2
ORDER BY 1, 2;

-- name: ListScreenDisagreements :many
-- Articles the two models put in different buckets, widest gap first.
WITH a AS (
  SELECT DISTINCT ON (article_id) article_id, threat, category
  FROM article_screens WHERE model = @model_a::text
  ORDER BY article_id, screened_at DESC, id DESC
), b AS (
  SELECT DISTINCT ON (article_id) article_id, threat, category
  FROM article_screens WHERE model = @model_b::text
  ORDER BY article_id, screened_at DESC, id DESC
)
SELECT art.id, art.title,
       a.threat AS threat_a, COALESCE(a.category, '')::text AS category_a,
       b.threat AS threat_b, COALESCE(b.category, '')::text AS category_b
FROM a
JOIN b ON b.article_id = a.article_id
JOIN articles art ON art.id = a.article_id
WHERE (CASE WHEN a.threat <= @pass::double precision THEN 0
            WHEN a.threat <= @border::double precision THEN 1 ELSE 2 END)
   <> (CASE WHEN b.threat <= @pass::double precision THEN 0
            WHEN b.threat <= @border::double precision THEN 1 ELSE 2 END)
ORDER BY ABS(a.threat - b.threat) DESC, art.id DESC
LIMIT @lim;

-- name: CompareSummaryCounts :one
WITH a AS (
  SELECT DISTINCT ON (article_id) article_id, ai_summary, duration_ms
  FROM article_summaries WHERE model = @model_a::text
  ORDER BY article_id, generated_at DESC, id DESC
), b AS (
  SELECT DISTINCT ON (article_id) article_id, ai_summary, duration_ms
  FROM article_summaries WHERE model = @model_b::text
  ORDER BY article_id, generated_at DESC, id DESC
)
SELECT COUNT(*)::int AS both_rows,
       (COUNT(*) FILTER (WHERE a.ai_summary <> '' AND b.ai_summary <> ''))::int AS both_text,
       (COUNT(*) FILTER (WHERE a.ai_summary <> '' AND b.ai_summary = ''))::int AS only_a_text,
       (COUNT(*) FILTER (WHERE a.ai_summary = '' AND b.ai_summary <> ''))::int AS only_b_text,
       (COUNT(*) FILTER (WHERE a.ai_summary = '' AND b.ai_summary = ''))::int AS neither_text,
       COALESCE(AVG(length(a.ai_summary)) FILTER (WHERE a.ai_summary <> '' AND b.ai_summary <> ''), 0)::double precision AS avg_chars_a,
       COALESCE(AVG(length(b.ai_summary)) FILTER (WHERE a.ai_summary <> '' AND b.ai_summary <> ''), 0)::double precision AS avg_chars_b,
       COALESCE(AVG(a.duration_ms) FILTER (WHERE a.ai_summary <> ''), 0)::double precision AS avg_ms_a,
       COALESCE(AVG(b.duration_ms) FILTER (WHERE b.ai_summary <> ''), 0)::double precision AS avg_ms_b
FROM a JOIN b ON b.article_id = a.article_id;

-- name: SampleSummaryPairs :many
-- A fixed pseudo-random sample of articles both models summarized: ordered by
-- a hash of the id, so the same sample comes back on every run and grows
-- without reshuffling as @lim does.
WITH a AS (
  SELECT DISTINCT ON (article_id) article_id, ai_summary
  FROM article_summaries WHERE model = @model_a::text AND ai_summary <> ''
  ORDER BY article_id, generated_at DESC, id DESC
), b AS (
  SELECT DISTINCT ON (article_id) article_id, ai_summary
  FROM article_summaries WHERE model = @model_b::text AND ai_summary <> ''
  ORDER BY article_id, generated_at DESC, id DESC
)
SELECT art.id, art.title, a.ai_summary AS summary_a, b.ai_summary AS summary_b
FROM a
JOIN b ON b.article_id = a.article_id
JOIN articles art ON art.id = a.article_id
ORDER BY md5(art.id::text), art.id
LIMIT @lim;

-- name: ListSummaryModels :many
SELECT COALESCE(model, '')::text AS model, COUNT(*)::int AS n
FROM article_summaries GROUP BY 1 ORDER BY 2 DESC;

-- name: ListScreenModels :many
SELECT model, COUNT(*)::int AS n
FROM article_screens GROUP BY 1 ORDER BY 2 DESC;
