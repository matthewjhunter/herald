-- name: CreateSession :exec
INSERT INTO sessions
  (id, user_sub, access_token, refresh_token, access_expiry, absolute_expiry)
VALUES (@id, @user_sub, @access_token, @refresh_token, @access_expiry, @absolute_expiry);

-- name: GetSession :one
SELECT id, user_sub, access_token, refresh_token, version,
       access_expiry, absolute_expiry, created_at
FROM sessions
WHERE id = @id;

-- name: RotateSessionTokens :execrows
-- Compare-and-swap on the version counter: write new tokens and bump the
-- version only if the stored version still matches the one the caller read.
UPDATE sessions
SET access_token = @access_token,
    refresh_token = @refresh_token,
    access_expiry = @access_expiry,
    version = version + 1,
    last_used_at = @last_used_at
WHERE id = @id AND version = @expect_version;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = @id;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE absolute_expiry <= @cutoff;
