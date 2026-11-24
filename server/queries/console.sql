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

-- name: SetAccountPlatformAdmin :one
UPDATE accounts SET is_platform_admin = sqlc.arg(admin)::boolean WHERE lower(email) = lower(sqlc.arg(email)) RETURNING id;

-- Accounts in id order, optionally only those whose e-mail or name contains the search (an ILIKE pattern, escaped).
-- name: SetAccountSuspended :one
UPDATE accounts
SET disabled_at = CASE WHEN sqlc.arg(suspended)::boolean THEN coalesce(disabled_at, now()) ELSE NULL END,
    suspended_reason = CASE WHEN sqlc.arg(suspended)::boolean THEN sqlc.narg(reason)::text ELSE NULL END
WHERE id = sqlc.arg(id)
RETURNING *;

-- An account to delete; one already scheduled keeps its date.
-- name: ScheduleAccountDeletion :one
UPDATE accounts SET delete_after = coalesce(delete_after, sqlc.arg(delete_after)::timestamptz) WHERE id = sqlc.arg(id) RETURNING *;

-- name: SetAccountDeleteNow :one
UPDATE accounts SET delete_after = now() WHERE id = $1 RETURNING *;

-- name: CancelAccountDeletion :one
UPDATE accounts SET delete_after = NULL WHERE id = $1 RETURNING *;

-- The next account whose grace period is over, locked so that two workers never purge the same one.
-- name: LockDueAccountDeletion :one
SELECT id, email FROM accounts
WHERE delete_after IS NOT NULL AND delete_after <= now()
ORDER BY delete_after
LIMIT 1
FOR UPDATE SKIP LOCKED;

-- name: ListTenantIDsOfAccount :many
SELECT id, slug FROM tenants WHERE account_id = $1;

-- Sessions, tokens and the security log go with it (ON DELETE CASCADE).
-- name: DeleteAccountRow :exec
DELETE FROM accounts WHERE id = $1;

-- name: ListAccounts :many
SELECT * FROM accounts
WHERE id > sqlc.arg(after)::uuid
  AND (sqlc.arg(pattern)::text = '' OR email ILIKE sqlc.arg(pattern)::text OR name ILIKE sqlc.arg(pattern)::text)
ORDER BY id
LIMIT sqlc.arg(max_rows);

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
INSERT INTO console_sessions (id, account_id, token_hash, expires_at, absolute_expires_at, user_agent, ip, scope)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: GetSessionByHash :one
SELECT * FROM console_sessions WHERE token_hash = $1 AND expires_at > now() AND absolute_expires_at > now();

-- name: TouchSession :exec
UPDATE console_sessions SET last_seen_at = now(), expires_at = $2 WHERE id = $1;

-- name: DeleteSession :exec
DELETE FROM console_sessions WHERE id = $1;

-- name: DeleteAccountSessions :exec
DELETE FROM console_sessions WHERE account_id = $1;

-- name: DeleteSessionOfAccount :execrows
DELETE FROM console_sessions WHERE id = $1 AND account_id = $2;

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

-- A project waiting to be deleted does not count against the limit.
-- name: CountTenantsOfAccount :one
SELECT count(*) FROM tenants WHERE account_id = $1 AND delete_after IS NULL;

-- Switches the project off and sets when it goes for good. A project already scheduled keeps its date.
-- name: ScheduleTenantDeletion :one
UPDATE tenants
SET delete_after = coalesce(delete_after, sqlc.arg(delete_after)::timestamptz), disabled_at = coalesce(disabled_at, now())
WHERE id = sqlc.arg(id)
RETURNING *;

-- The platform admin's "delete now": the date is moved to now even when the project was already scheduled.
-- name: SetTenantDeleteNow :one
UPDATE tenants SET delete_after = now(), disabled_at = coalesce(disabled_at, now()) WHERE id = $1 RETURNING *;

-- Restoring keeps the project switched off: the owner switches it on when ready.
-- name: CancelTenantDeletion :one
UPDATE tenants SET delete_after = NULL WHERE id = $1 RETURNING *;

-- The next project whose grace period is over, locked so that two workers never purge the same one.
-- name: LockDueTenantDeletion :one
SELECT id, slug, account_id FROM tenants
WHERE delete_after IS NOT NULL AND delete_after <= now()
ORDER BY delete_after
LIMIT 1
FOR UPDATE SKIP LOCKED;

-- Everything of a tenant, in an order the foreign keys accept. Channels take their members, messages, reactions and
-- message attachments with them (ON DELETE CASCADE).
-- name: PurgeTenantChannels :exec
DELETE FROM channels WHERE tenant_id = $1;

-- name: PurgeTenantAttachments :exec
DELETE FROM attachments WHERE tenant_id = $1;

-- name: PurgeTenantDevices :exec
DELETE FROM devices WHERE tenant_id = $1;

-- name: PurgeTenantOutbox :exec
DELETE FROM outbox WHERE tenant_id = $1;

-- name: PurgeTenantUsers :exec
DELETE FROM users WHERE tenant_id = $1;

-- name: DeleteTenantRow :exec
DELETE FROM tenants WHERE id = $1;

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

-- The whole platform's audit log, newest first, with the e-mail of each account. Optional filters; the cursor is the id
-- of the last event of the previous page.
-- name: ListPlatformAudit :many
SELECT sqlc.embed(e), a.email AS account_email
FROM console_audit e JOIN accounts a ON a.id = e.account_id
WHERE (sqlc.narg(action)::text IS NULL OR e.action = sqlc.narg(action)::text)
  AND (sqlc.narg(account_id)::uuid IS NULL OR e.account_id = sqlc.narg(account_id)::uuid)
  AND (sqlc.narg(before_id)::uuid IS NULL OR (e.created_at, e.id) < (
        SELECT b.created_at, b.id FROM console_audit b WHERE b.id = sqlc.narg(before_id)::uuid))
ORDER BY e.created_at DESC, e.id DESC
LIMIT sqlc.arg(max_rows);

-- Per day (UTC) over the last N days, today included: new accounts, new projects, messages sent and people who sent
-- one (system messages are not counted).
-- name: PlatformDailySeries :many
WITH days AS (
    SELECT generate_series(
        date_trunc('day', now() AT TIME ZONE 'UTC') - (sqlc.arg(days)::int - 1) * interval '1 day',
        date_trunc('day', now() AT TIME ZONE 'UTC'),
        interval '1 day') AS day
), since AS (SELECT min(day) AT TIME ZONE 'UTC' AS t FROM days),
acc AS (
    SELECT date_trunc('day', created_at AT TIME ZONE 'UTC') AS day, count(*) AS n
    FROM accounts WHERE created_at >= (SELECT t FROM since) GROUP BY 1
), prj AS (
    SELECT date_trunc('day', created_at AT TIME ZONE 'UTC') AS day, count(*) AS n
    FROM tenants WHERE created_at >= (SELECT t FROM since) GROUP BY 1
), msg AS (
    SELECT date_trunc('day', created_at AT TIME ZONE 'UTC') AS day, count(*) AS n, count(DISTINCT sender_id) AS senders
    FROM messages WHERE created_at >= (SELECT t FROM since) AND type <> 'system' GROUP BY 1
)
SELECT to_char(days.day, 'YYYY-MM-DD') AS day,
       coalesce(acc.n, 0)::bigint AS accounts,
       coalesce(prj.n, 0)::bigint AS projects,
       coalesce(msg.n, 0)::bigint AS messages,
       coalesce(msg.senders, 0)::bigint AS active_users
FROM days
LEFT JOIN acc ON acc.day = days.day
LEFT JOIN prj ON prj.day = days.day
LEFT JOIN msg ON msg.day = days.day
ORDER BY days.day;

-- The projects that sent the most messages since a time.
-- name: TopTenantsByMessages :many
SELECT t.id, t.name, t.slug, count(*)::bigint AS messages
FROM messages m JOIN tenants t ON t.id = m.tenant_id
WHERE m.created_at >= sqlc.arg(since)::timestamptz AND m.type <> 'system'
GROUP BY t.id, t.name, t.slug
ORDER BY messages DESC
LIMIT sqlc.arg(max_rows);
