-- The tenants table is the only one queried without tenant_id (PLAN D1).

-- name: CreateTenant :one
INSERT INTO tenants (id, slug, name, api_key, api_secret_enc, config, account_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- A key works only while its tenant is neither switched off (by its owner) nor suspended (by the platform admin).
-- name: GetTenantByAPIKey :one
SELECT * FROM tenants WHERE api_key = $1 AND disabled_at IS NULL AND suspended_at IS NULL;

-- A suspension of the project itself (the platform admin, source 'project').
-- name: SetTenantSuspended :one
UPDATE tenants
SET suspended_at = CASE WHEN sqlc.arg(suspended)::boolean THEN coalesce(suspended_at, now()) ELSE NULL END,
    suspended_reason = CASE WHEN sqlc.arg(suspended)::boolean THEN sqlc.narg(reason)::text ELSE NULL END,
    suspension_source = CASE WHEN sqlc.arg(suspended)::boolean THEN 'project' ELSE NULL END
WHERE id = sqlc.arg(id)
RETURNING *;

-- The projects of an account that is suspended or being deleted. Those already suspended keep their own reason.
-- name: SuspendTenantsOfAccount :many
UPDATE tenants SET suspended_at = now(), suspended_reason = sqlc.arg(reason)::text, suspension_source = 'account'
WHERE account_id = sqlc.arg(account_id)::uuid AND suspended_at IS NULL
RETURNING id;

-- Lifts only what the account caused; a suspension the admin put on a project stays.
-- name: LiftAccountSuspensionsOfTenants :many
UPDATE tenants SET suspended_at = NULL, suspended_reason = NULL, suspension_source = NULL
WHERE account_id = $1 AND suspension_source = 'account'
RETURNING id;

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
