-- The tenants table is the only one queried without tenant_id (PLAN D1).

-- name: CreateTenant :one
INSERT INTO tenants (id, slug, name, api_key, api_secret_enc, config)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetTenantByAPIKey :one
SELECT * FROM tenants WHERE api_key = $1 AND disabled_at IS NULL;

-- name: GetTenantBySlug :one
SELECT * FROM tenants WHERE slug = $1;

-- name: ListTenants :many
SELECT * FROM tenants ORDER BY created_at;

-- name: UpdateTenantSecret :execrows
UPDATE tenants SET api_secret_enc = $2 WHERE id = $1;

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
