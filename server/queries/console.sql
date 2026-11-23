-- The website's accounts, sessions and the projects (tenants) they own. Tenant chat data is never touched here.

-- name: CreateAccount :one
INSERT INTO accounts (id, email, name, password_hash) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetAccountByEmail :one
SELECT * FROM accounts WHERE lower(email) = lower($1);

-- name: GetAccountByID :one
SELECT * FROM accounts WHERE id = $1;

-- name: UpdateAccountName :one
UPDATE accounts SET name = $2 WHERE id = $1 RETURNING *;

-- name: UpdateAccountPassword :exec
UPDATE accounts SET password_hash = $2 WHERE id = $1;

-- name: SetAccountEmailVerified :exec
UPDATE accounts SET email_verified_at = coalesce(email_verified_at, now()) WHERE id = $1;

-- name: SetAccountDisabled :one
UPDATE accounts
SET disabled_at = CASE WHEN sqlc.arg(disabled)::boolean THEN coalesce(disabled_at, now()) ELSE NULL END
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: SetAccountPlatformAdmin :execrows
UPDATE accounts SET is_platform_admin = sqlc.arg(admin)::boolean WHERE lower(email) = lower(sqlc.arg(email));

-- name: ListAccounts :many
SELECT * FROM accounts WHERE id > sqlc.arg(after)::uuid ORDER BY id LIMIT sqlc.arg(max_rows);

-- name: CountAccounts :one
SELECT count(*) FROM accounts;

-- name: InsertAccountToken :exec
INSERT INTO account_tokens (account_id, kind, token_hash, expires_at) VALUES ($1, $2, $3, $4);

-- name: DeleteAccountTokens :exec
DELETE FROM account_tokens WHERE account_id = $1 AND kind = $2;

-- A token that can still be used, locked: it is spent in the same transaction.
-- name: ConsumeAccountToken :one
UPDATE account_tokens SET used_at = now()
WHERE token_hash = $1 AND kind = $2 AND used_at IS NULL AND expires_at > now()
RETURNING account_id;

-- name: CountRecentAccountTokens :one
SELECT count(*) FROM account_tokens WHERE account_id = $1 AND kind = $2 AND created_at > $3;

-- name: InsertSession :exec
INSERT INTO console_sessions (id, account_id, token_hash, expires_at, absolute_expires_at, user_agent, ip)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetSessionByHash :one
SELECT * FROM console_sessions WHERE token_hash = $1 AND expires_at > now() AND absolute_expires_at > now();

-- name: TouchSession :exec
UPDATE console_sessions SET last_seen_at = now(), expires_at = $2 WHERE id = $1;

-- name: DeleteSession :exec
DELETE FROM console_sessions WHERE id = $1;

-- name: DeleteAccountSessions :exec
DELETE FROM console_sessions WHERE account_id = $1;

-- name: DeleteOtherSessions :exec
DELETE FROM console_sessions WHERE account_id = $1 AND id <> $2;

-- name: ListAccountSessions :many
SELECT * FROM console_sessions
WHERE account_id = $1 AND expires_at > now() AND absolute_expires_at > now()
ORDER BY last_seen_at DESC;

-- name: DeleteExpiredSessions :execrows
DELETE FROM console_sessions WHERE expires_at < now() OR absolute_expires_at < now();

-- name: InsertAudit :exec
INSERT INTO console_audit (account_id, action, target, ip) VALUES ($1, $2, $3, $4);

-- name: ListAudit :many
SELECT * FROM console_audit WHERE account_id = $1 ORDER BY created_at DESC LIMIT $2;

-- name: ListTenantsOfAccount :many
SELECT * FROM tenants WHERE account_id = $1 ORDER BY created_at;

-- name: GetTenantOfAccount :one
SELECT * FROM tenants WHERE id = $1 AND account_id = $2;

-- name: CountTenantsOfAccount :one
SELECT count(*) FROM tenants WHERE account_id = $1;

-- name: ListAllTenantsWithOwner :many
SELECT sqlc.embed(t), a.email AS owner_email
FROM tenants t LEFT JOIN accounts a ON a.id = t.account_id
ORDER BY t.created_at;

-- name: GetTenantWithOwner :one
SELECT sqlc.embed(t), a.email AS owner_email
FROM tenants t LEFT JOIN accounts a ON a.id = t.account_id
WHERE t.id = $1;

-- Usage of one tenant. Messages: the sum of the per channel counters of messages (cheap, no scan).
-- name: TenantUsage :one
SELECT
    (SELECT count(*) FROM users u WHERE u.tenant_id = sqlc.arg(for_tenant) AND u.deleted_at IS NULL)::bigint AS users,
    (SELECT count(*) FROM channels c WHERE c.tenant_id = sqlc.arg(for_tenant))::bigint AS channels,
    (SELECT coalesce(sum(c2.last_message_seq), 0) FROM channels c2 WHERE c2.tenant_id = sqlc.arg(for_tenant))::bigint AS messages;

-- name: PlatformUsage :one
SELECT
    (SELECT count(*) FROM accounts)::bigint AS accounts,
    (SELECT count(*) FROM tenants)::bigint AS projects,
    (SELECT count(*) FROM tenants WHERE disabled_at IS NULL)::bigint AS active_projects,
    (SELECT count(*) FROM users u WHERE u.deleted_at IS NULL)::bigint AS users,
    (SELECT count(*) FROM channels)::bigint AS channels,
    (SELECT coalesce(sum(last_message_seq), 0) FROM channels)::bigint AS messages;
