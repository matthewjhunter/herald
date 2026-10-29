-- +goose Up
--
-- The image and favicon caches used to store whatever a server returned when it
-- could not be decoded, typed by the server's Content-Type, so an <img src> or
-- icon link pointing at HTML or SVG left a document in the cache that herald
-- then served from its own origin (issue #344). New rows are re-encoded rasters
-- and the serving paths refuse other types, so the rows deleted here could no
-- longer be served anyway. Deleting a favicon lets the next cycle fetch it
-- again through the new checks; a deleted article image is not re-fetched, and
-- the article falls back to the remote URL, which the CSP does not load.
DELETE FROM article_images
WHERE mime_type NOT IN ('image/png', 'image/jpeg', 'image/gif', 'image/webp', 'image/x-icon');

-- The old code also stored the Content-Type verbatim, so real ICO favicons sit
-- under variant labels. Relabel those whose bytes start with the ICONDIR header
-- (reserved 0, type 1) rather than delete icons a site may no longer serve.
UPDATE feed_favicons SET mime_type = 'image/x-icon'
WHERE mime_type <> 'image/x-icon'
  AND (lower(mime_type) LIKE 'image/x-icon;%' OR lower(mime_type) LIKE 'image/vnd.microsoft.icon%')
  AND substring(data FROM 1 FOR 4) = '\x00000100'::bytea;

DELETE FROM feed_favicons
WHERE mime_type NOT IN ('image/png', 'image/jpeg', 'image/gif', 'image/webp', 'image/x-icon');

-- +goose Down
-- The deleted rows were unservable; there is nothing to restore.
SELECT 1;
