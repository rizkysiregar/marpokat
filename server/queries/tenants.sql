-- Every query on tenant data takes tenant_id (PLAN D1). The tenants table itself is the only exception.

-- name: GetTenantByAPIKey :one
SELECT * FROM tenants WHERE api_key = $1 AND disabled_at IS NULL;

-- name: GetTenantBySlug :one
SELECT * FROM tenants WHERE slug = $1;
