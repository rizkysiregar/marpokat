-- name: UpsertUser :one
-- Re-upserting a deleted user brings it back: the tenant decides who exists.
INSERT INTO users (id, tenant_id, external_id, name, avatar_url, metadata)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, external_id) DO UPDATE
SET name = EXCLUDED.name,
    avatar_url = EXCLUDED.avatar_url,
    metadata = EXCLUDED.metadata,
    deleted_at = NULL
RETURNING *;

-- name: GetUserByExternalID :one
SELECT * FROM users
WHERE tenant_id = $1 AND external_id = $2 AND deleted_at IS NULL;

-- name: GetUserIDByExternalID :one
SELECT id FROM users
WHERE tenant_id = $1 AND external_id = $2 AND deleted_at IS NULL;

-- name: AnonymizeUser :one
-- Messages keep pointing at the row, so conversations keep their shape; the personal data goes.
UPDATE users
SET name = '', avatar_url = NULL, metadata = '{}'::jsonb, deleted_at = now()
WHERE tenant_id = $1 AND external_id = $2 AND deleted_at IS NULL
RETURNING id;

-- name: DeleteUserDevices :exec
DELETE FROM devices WHERE tenant_id = $1 AND user_id = $2;

-- name: LeaveAllChannels :exec
UPDATE channel_members SET left_at = now()
WHERE tenant_id = $1 AND user_id = $2 AND left_at IS NULL;

-- name: GetUsersByExternalIDs :many
SELECT * FROM users WHERE tenant_id = $1 AND external_id = ANY($2::text[]) AND deleted_at IS NULL;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1 AND tenant_id = $2;
