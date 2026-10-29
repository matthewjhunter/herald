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

DELETE FROM feed_favicons
WHERE mime_type NOT IN ('image/png', 'image/jpeg', 'image/gif', 'image/webp', 'image/x-icon');

-- +goose Down
-- The deleted rows were unservable; there is nothing to restore.
SELECT 1;
