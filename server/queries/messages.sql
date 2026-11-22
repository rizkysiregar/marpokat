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
INSERT INTO messages (channel_id, seq, id, tenant_id, sender_id, client_msg_id, type, text, reply_to_id, metadata, rev_seq, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

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

-- name: GetMessageByID :one
SELECT * FROM message_details WHERE id = $1 AND tenant_id = $2;

-- name: BumpChannelRev :one
-- Takes the next value of the channel counter for a change to an existing message (edit, delete, reaction, an attachment
-- that became ready). It locks the channel row like BumpChannelSeq does, so changes commit in counter order.
UPDATE channels
SET last_seq = last_seq + 1
WHERE id = sqlc.arg('id') AND tenant_id = sqlc.arg('tenant_id') AND deleted_at IS NULL
  AND (NOT sqlc.arg('check_frozen')::boolean OR frozen_at IS NULL)
RETURNING last_seq;

-- name: ReviseMessageText :exec
UPDATE messages SET text = sqlc.arg('text'), edited_at = now(), rev_seq = sqlc.arg('rev_seq')
WHERE id = sqlc.arg('id') AND tenant_id = sqlc.arg('tenant_id') AND deleted_at IS NULL;

-- name: ReviseMessageDeleted :exec
-- A tombstone keeps the place of the message and nothing of its content.
UPDATE messages SET text = NULL, deleted_at = now(), reaction_counts = '{}'::jsonb, rev_seq = sqlc.arg('rev_seq')
WHERE id = sqlc.arg('id') AND tenant_id = sqlc.arg('tenant_id') AND deleted_at IS NULL;

-- name: ReviseMessage :exec
UPDATE messages SET rev_seq = sqlc.arg('rev_seq') WHERE id = sqlc.arg('id') AND tenant_id = sqlc.arg('tenant_id');

-- name: InsertReaction :execrows
INSERT INTO message_reactions (message_id, user_id, emoji) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: DeleteReaction :execrows
DELETE FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji = $3;

-- name: DeleteMessageReactions :exec
DELETE FROM message_reactions WHERE message_id = $1;

-- name: CountUserReactions :one
SELECT count(*)::int FROM message_reactions WHERE message_id = $1 AND user_id = $2;

-- name: AdjustReactionCount :exec
-- Keeps the per emoji counts on the message in step with message_reactions. One UPDATE reads and writes the count, so
-- two users reacting at the same moment cannot lose one.
UPDATE messages
SET reaction_counts = CASE
        WHEN coalesce((reaction_counts ->> sqlc.arg('emoji')::text)::int, 0) + sqlc.arg('delta')::int <= 0
            THEN reaction_counts - sqlc.arg('emoji')::text
        ELSE jsonb_set(reaction_counts, ARRAY[sqlc.arg('emoji')::text],
                       to_jsonb(coalesce((reaction_counts ->> sqlc.arg('emoji')::text)::int, 0) + sqlc.arg('delta')::int))
    END,
    rev_seq = sqlc.arg('rev_seq')
WHERE id = sqlc.arg('id') AND tenant_id = sqlc.arg('tenant_id') AND deleted_at IS NULL;

-- name: ListMyReactions :many
-- What one user reacted with on the given messages (comma separated ids).
SELECT message_id, emoji FROM message_reactions
WHERE user_id = sqlc.arg('user_id') AND message_id = ANY(string_to_array(sqlc.arg('message_ids')::text, ',')::uuid[]);

-- name: ListReactionsOfMessage :many
SELECT user_id, emoji FROM message_reactions WHERE message_id = $1;

-- name: ListMessagePreviews :many
-- The short form of the given messages (comma separated ids), for the "replying to" line of a reply.
SELECT md.id, md.seq, md.channel_id, md.sender_external_id, md.text, md.deleted_at,
       EXISTS (SELECT 1 FROM message_attachments ma WHERE ma.message_id = md.id) AS has_attachments
FROM message_details md
WHERE md.tenant_id = sqlc.arg('tenant_id') AND md.id = ANY(string_to_array(sqlc.arg('message_ids')::text, ',')::uuid[]);
