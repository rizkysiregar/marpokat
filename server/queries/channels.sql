-- Every query takes tenant_id (PLAN D1).

-- name: InsertChannel :one
-- For a DM that already exists nothing is inserted and no row comes back (the caller loads it by dm_key).
INSERT INTO channels (id, tenant_id, type, name, avatar_url, dm_key, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (dm_key) DO NOTHING
RETURNING *;

-- name: GetChannelByDMKey :one
SELECT * FROM channels WHERE dm_key = $1 AND tenant_id = $2 AND deleted_at IS NULL;

-- name: GetChannel :one
SELECT * FROM channels WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL;

-- name: LockChannel :one
-- Serialises changes to one channel (members, name, sends) until the transaction ends.
SELECT * FROM channels WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL FOR UPDATE;

-- name: UpdateChannelProfile :one
UPDATE channels
SET name = coalesce(sqlc.narg('name'), name),
    avatar_url = CASE WHEN sqlc.arg('set_avatar')::boolean THEN sqlc.narg('avatar_url') ELSE avatar_url END
WHERE id = sqlc.arg('id') AND tenant_id = sqlc.arg('tenant_id') AND deleted_at IS NULL
RETURNING *;

-- name: SetChannelFrozen :execrows
UPDATE channels
SET frozen_at = CASE WHEN sqlc.arg('frozen')::boolean THEN coalesce(frozen_at, now()) ELSE NULL END
WHERE id = sqlc.arg('id') AND tenant_id = sqlc.arg('tenant_id') AND deleted_at IS NULL;

-- name: SoftDeleteChannel :exec
UPDATE channels SET deleted_at = now() WHERE id = $1 AND tenant_id = $2;

-- name: AddMember :execrows
-- Adds the user, or brings back one who left. 0 rows: already an active member.
INSERT INTO channel_members (channel_id, user_id, tenant_id, role, last_read_seq)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (channel_id, user_id) DO UPDATE
SET left_at = NULL, role = EXCLUDED.role, joined_at = now(), last_read_seq = EXCLUDED.last_read_seq
WHERE channel_members.left_at IS NOT NULL;

-- name: GetMember :one
SELECT cm.channel_id, cm.user_id, cm.tenant_id, cm.role, cm.last_read_seq, cm.muted_until, cm.joined_at, cm.left_at,
       c.type AS channel_type, c.frozen_at AS channel_frozen_at
FROM channel_members cm
JOIN channels c ON c.id = cm.channel_id
WHERE cm.channel_id = $1 AND cm.user_id = $2 AND cm.tenant_id = $3 AND c.tenant_id = $3 AND c.deleted_at IS NULL;

-- name: RemoveMember :execrows
UPDATE channel_members SET left_at = now()
WHERE channel_id = $1 AND user_id = $2 AND tenant_id = $3 AND left_at IS NULL;

-- name: SetMemberRole :exec
UPDATE channel_members SET role = $4
WHERE channel_id = $1 AND user_id = $2 AND tenant_id = $3 AND left_at IS NULL;

-- name: PickNextOwner :one
-- Oldest admin, else the oldest member.
SELECT user_id FROM channel_members
WHERE channel_id = $1 AND tenant_id = $2 AND left_at IS NULL AND user_id <> $3
ORDER BY (role = 'admin') DESC, joined_at, user_id
LIMIT 1;

-- name: CountActiveMembers :one
SELECT count(*)::int FROM channel_members WHERE channel_id = $1 AND tenant_id = $2 AND left_at IS NULL;

-- name: ListMembers :many
SELECT cm.role, cm.last_read_seq, cm.joined_at,
       u.id AS user_id, u.external_id, u.name, u.avatar_url, u.metadata, u.deleted_at AS user_deleted_at
FROM channel_members cm
JOIN users u ON u.id = cm.user_id
WHERE cm.channel_id = sqlc.arg('channel_id') AND cm.tenant_id = sqlc.arg('tenant_id') AND cm.left_at IS NULL
  AND (sqlc.narg('cursor_at')::timestamptz IS NULL
       OR (cm.joined_at, cm.user_id) > (sqlc.narg('cursor_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY cm.joined_at, cm.user_id
LIMIT sqlc.arg('lim');

-- name: ListMemberExternalIDs :many
SELECT u.external_id FROM channel_members cm JOIN users u ON u.id = cm.user_id
WHERE cm.channel_id = $1 AND cm.tenant_id = $2 AND cm.left_at IS NULL
ORDER BY cm.joined_at, cm.user_id;

-- name: ListUserChannels :many
-- The channels of one user, newest activity first. Without since only active memberships; with since the channels
-- changed after it, including those the user left (the client drops them from its cache). channel_id narrows it to one.
SELECT c.id, c.tenant_id, c.type, c.name, c.avatar_url, c.dm_key, c.last_seq, c.last_message_seq,
       c.last_message_at, c.created_by, c.created_at, c.updated_at, c.frozen_at, c.deleted_at,
       cm.role, cm.last_read_seq, cm.muted_until, cm.joined_at, cm.left_at,
       (SELECT count(*) FROM (
            SELECT 1 FROM messages m
            WHERE m.channel_id = c.id AND m.seq > cm.last_read_seq AND m.type <> 'system'
              AND m.deleted_at IS NULL AND m.sender_id IS DISTINCT FROM cm.user_id
            LIMIT 100) unread)::int AS unread_count,
       (SELECT count(*) FROM channel_members x
        WHERE x.channel_id = c.id AND x.left_at IS NULL)::int AS member_count,
       coalesce(c.last_message_at, c.created_at)::timestamptz AS sort_at
FROM channel_members cm
JOIN channels c ON c.id = cm.channel_id
WHERE cm.user_id = sqlc.arg('user_id') AND cm.tenant_id = sqlc.arg('tenant_id') AND c.tenant_id = sqlc.arg('tenant_id')
  AND c.deleted_at IS NULL
  AND (sqlc.narg('channel_id')::uuid IS NULL OR c.id = sqlc.narg('channel_id')::uuid)
  AND (CASE WHEN sqlc.narg('since')::timestamptz IS NULL THEN cm.left_at IS NULL
            ELSE (c.updated_at > sqlc.narg('since')::timestamptz OR cm.left_at > sqlc.narg('since')::timestamptz) END)
  AND (sqlc.narg('cursor_at')::timestamptz IS NULL
       OR (coalesce(c.last_message_at, c.created_at), c.id) < (sqlc.narg('cursor_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY sort_at DESC, c.id DESC
LIMIT sqlc.arg('lim');

-- name: ListDMPeers :many
-- The other member of each given DM, as seen by user_id. channel_ids is a comma separated list: with the simple
-- protocol (pgbouncer) the driver cannot encode a uuid array, and a string it can.
SELECT cm.channel_id, u.id, u.external_id, u.name, u.avatar_url, u.metadata, u.deleted_at
FROM channel_members cm
JOIN users u ON u.id = cm.user_id
WHERE cm.channel_id = ANY(string_to_array(sqlc.arg('channel_ids')::text, ',')::uuid[]) AND cm.tenant_id = sqlc.arg('tenant_id')
  AND cm.user_id <> sqlc.arg('user_id');
