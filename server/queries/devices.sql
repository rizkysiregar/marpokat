-- name: UpsertDevice :exec
-- A push token belongs to whoever registered it last (shared device, account switch).
INSERT INTO devices (tenant_id, push_token, user_id, platform)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, push_token) DO UPDATE SET user_id = EXCLUDED.user_id, platform = EXCLUDED.platform;

-- name: DeleteDeviceOfUser :execrows
DELETE FROM devices WHERE tenant_id = $1 AND push_token = $2 AND user_id = $3;

-- name: DeleteDeviceByToken :exec
-- For tokens the push service reported as no longer valid, whoever owned them.
DELETE FROM devices WHERE tenant_id = $1 AND push_token = $2;

-- name: TrimDevicesOfUser :exec
-- Keeps the most recently registered tokens of a user: old phones that never unregistered must not pile up.
DELETE FROM devices dv
WHERE dv.tenant_id = sqlc.arg('tenant_id') AND dv.user_id = sqlc.arg('user_id')
  AND dv.push_token NOT IN (
      SELECT d.push_token FROM devices d
      WHERE d.tenant_id = sqlc.arg('tenant_id') AND d.user_id = sqlc.arg('user_id')
      ORDER BY d.updated_at DESC, d.push_token
      LIMIT sqlc.arg('keep'));

-- name: ListDevicesOfUsers :many
-- user_ids is a comma separated list of uuids, see MarkOutboxPublished.
SELECT user_id, push_token, platform FROM devices
WHERE tenant_id = sqlc.arg('tenant_id') AND user_id = ANY(string_to_array(sqlc.arg('user_ids')::text, ',')::uuid[]);
