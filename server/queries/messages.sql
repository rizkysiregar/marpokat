-- name: BumpChannelSeq :one
-- Takes the next value of the channel counter for a new message (PLAN D8). The row lock it takes is held until the
-- transaction ends, so sends to one channel commit in seq order and a reader never sees seq N+1 without N.
-- With check_frozen, a frozen channel matches nothing.
UPDATE channels
SET last_seq = last_seq + 1, last_message_seq = last_seq + 1, last_message_at = now()
WHERE id = sqlc.arg('id') AND tenant_id = sqlc.arg('tenant_id') AND deleted_at IS NULL
  AND (NOT sqlc.arg('check_frozen')::boolean OR frozen_at IS NULL)
RETURNING last_seq, last_message_at;

-- name: InsertMessage :exec
INSERT INTO messages (channel_id, seq, id, tenant_id, sender_id, client_msg_id, type, text, metadata, rev_seq, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: GetMessageBySeq :one
SELECT * FROM message_details WHERE channel_id = $1 AND tenant_id = $2 AND seq = $3;

-- name: GetMessageByClientID :one
SELECT * FROM message_details
WHERE channel_id = $1 AND tenant_id = $2 AND sender_id = $3 AND client_msg_id = $4;

-- name: ListMessages :many
-- History, newest first. before_seq 0 means from the latest.
SELECT * FROM message_details
WHERE channel_id = sqlc.arg('channel_id') AND tenant_id = sqlc.arg('tenant_id')
  AND (sqlc.arg('before_seq')::bigint = 0 OR seq < sqlc.arg('before_seq')::bigint)
ORDER BY seq DESC
LIMIT sqlc.arg('lim');

-- name: SyncMessages :many
-- Everything that changed after the cursor, oldest change first (PLAN D8).
SELECT * FROM message_details
WHERE channel_id = $1 AND tenant_id = $2 AND rev_seq > $3
ORDER BY rev_seq
LIMIT $4;

-- name: ListLastMessages :many
-- channel_ids is a comma separated list, see ListDMPeers.
SELECT md.* FROM channels c
JOIN message_details md ON md.channel_id = c.id AND md.seq = c.last_message_seq
WHERE c.id = ANY(string_to_array(sqlc.arg('channel_ids')::text, ',')::uuid[]) AND c.tenant_id = sqlc.arg('tenant_id');

-- name: MarkRead :one
-- Moves the read marker forward only, and never past the end of the channel.
UPDATE channel_members cm
SET last_read_seq = GREATEST(cm.last_read_seq, LEAST(sqlc.arg('seq')::bigint, c.last_seq))
FROM channels c
WHERE c.id = cm.channel_id AND cm.channel_id = sqlc.arg('channel_id') AND cm.user_id = sqlc.arg('user_id')
  AND cm.tenant_id = sqlc.arg('tenant_id') AND cm.left_at IS NULL
RETURNING cm.last_read_seq;

-- name: InsertOutbox :exec
INSERT INTO outbox (tenant_id, topic, payload) VALUES ($1, $2, $3);

-- name: ListPushRecipients :many
-- Who could be sent a push for a message: active members other than the sender who are not muted and have not read it
-- yet, with the unread count to show. Whether they are online is decided later, from presence.
SELECT cm.user_id,
       (SELECT count(*) FROM (
            SELECT 1 FROM messages m
            WHERE m.channel_id = cm.channel_id AND m.seq > cm.last_read_seq AND m.type <> 'system'
              AND m.deleted_at IS NULL AND m.sender_id IS DISTINCT FROM cm.user_id
            LIMIT 100) unread)::int AS unread_count
FROM channel_members cm
JOIN users u ON u.id = cm.user_id
WHERE cm.channel_id = sqlc.arg('channel_id') AND cm.tenant_id = sqlc.arg('tenant_id') AND cm.left_at IS NULL
  AND cm.user_id <> sqlc.arg('sender_id') AND u.deleted_at IS NULL
  AND cm.last_read_seq < sqlc.arg('seq')::bigint
  AND (cm.muted_until IS NULL OR cm.muted_until <= now());
