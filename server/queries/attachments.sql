-- name: InsertAttachment :one
INSERT INTO attachments (id, tenant_id, uploader_id, object_key, mime, size, file_name)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetAttachment :one
SELECT * FROM attachments WHERE id = $1 AND tenant_id = $2;

-- name: LockAttachment :one
-- Serialises the steps of one upload (complete, process) so that none runs twice at once.
SELECT * FROM attachments WHERE id = $1 AND tenant_id = $2 FOR UPDATE;

-- name: GetAttachmentsByIDs :many
-- ids is a comma separated list: the driver cannot encode a uuid array in the simple protocol.
SELECT * FROM attachments
WHERE tenant_id = sqlc.arg('tenant_id') AND id = ANY(string_to_array(sqlc.arg('ids')::text, ',')::uuid[]);

-- name: SetAttachmentStatus :exec
UPDATE attachments SET status = $3 WHERE id = $1 AND tenant_id = $2;

-- name: FinishAttachment :exec
UPDATE attachments
SET status = sqlc.arg('status'), width = sqlc.arg('width'), height = sqlc.arg('height'),
    duration_ms = sqlc.arg('duration_ms'), thumb_key = sqlc.arg('thumb_key'), blurhash = sqlc.arg('blurhash')
WHERE id = sqlc.arg('id') AND tenant_id = sqlc.arg('tenant_id');

-- name: LinkAttachment :exec
INSERT INTO message_attachments (message_id, attachment_id, position) VALUES ($1, $2, $3);

-- name: UnlinkMessageAttachments :exec
DELETE FROM message_attachments WHERE message_id = $1;

-- name: ListMessageAttachments :many
-- The attachments of the given messages (comma separated ids), in the order they were sent.
SELECT ma.message_id, ma.position, a.*
FROM message_attachments ma
JOIN attachments a ON a.id = ma.attachment_id
WHERE ma.message_id = ANY(string_to_array(sqlc.arg('message_ids')::text, ',')::uuid[])
ORDER BY ma.message_id, ma.position;

-- name: ListMessagesUsingAttachment :many
SELECT m.id, m.channel_id, m.seq
FROM message_attachments ma
JOIN messages m ON m.id = ma.message_id
WHERE ma.attachment_id = $1 AND m.tenant_id = $2;

-- name: ListUnusedAttachments :many
-- Attachments no message uses, older than the cutoff: never sent, or their message was deleted.
SELECT a.* FROM attachments a
WHERE a.created_at < $1
  AND NOT EXISTS (SELECT 1 FROM message_attachments ma WHERE ma.attachment_id = a.id)
ORDER BY a.created_at
LIMIT $2;

-- name: DeleteUnusedAttachment :execrows
DELETE FROM attachments a
WHERE a.id = $1 AND NOT EXISTS (SELECT 1 FROM message_attachments ma WHERE ma.attachment_id = a.id);
