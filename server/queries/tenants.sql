-- The tenants table is the only one queried without tenant_id (PLAN D1).

-- name: CreateTenant :one
INSERT INTO tenants (id, slug, name, api_key, api_secret_enc, config, account_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- A key works only while its tenant is neither switched off (by its owner) nor suspended (by the platform admin).
-- name: GetTenantByAPIKey :one
SELECT * FROM tenants WHERE api_key = $1 AND disabled_at IS NULL AND suspended_at IS NULL;

-- name: SetTenantSuspended :one
UPDATE tenants
SET suspended_at = CASE WHEN sqlc.arg(suspended)::boolean THEN coalesce(suspended_at, now()) ELSE NULL END,
    suspended_reason = CASE WHEN sqlc.arg(suspended)::boolean THEN sqlc.narg(reason)::text ELSE NULL END
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: SetTenantEnvironment :one
UPDATE tenants SET environment = sqlc.narg(environment)::text WHERE id = sqlc.arg(id) RETURNING *;

-- name: GetTenantBySlug :one
SELECT * FROM tenants WHERE slug = $1;

-- name: ListTenants :many
SELECT * FROM tenants ORDER BY created_at;

-- name: UpdateTenantSecret :execrows
UPDATE tenants SET api_secret_enc = $2 WHERE id = $1;

-- name: UpdateTenantName :execrows
UPDATE tenants SET name = $2 WHERE id = $1;

-- name: UpdateTenantConfig :execrows
UPDATE tenants SET config = $2 WHERE id = $1;

-- name: UpdateTenantFCMCredentials :execrows
UPDATE tenants SET fcm_credentials_enc = $2 WHERE id = $1;

-- name: UpdateTenantWebhook :execrows
UPDATE tenants SET webhook_url = $2, webhook_secret_enc = $3 WHERE id = $1;

-- name: SetTenantDisabled :execrows
UPDATE tenants
SET disabled_at = CASE WHEN sqlc.arg(disabled)::boolean THEN coalesce(disabled_at, now()) ELSE NULL END
WHERE id = $1;

-- name: GetTenantByID :one
SELECT * FROM tenants WHERE id = $1;
