-- name: LockOutboxBatch :many
-- The oldest unpublished events, locked for this transaction. Another worker skips them instead of waiting, so any
-- number of workers can run (PLAN D7). Only the partial index on unpublished rows is scanned.
SELECT id, event_id, tenant_id, topic, payload, created_at
FROM outbox
WHERE published_at IS NULL
ORDER BY id
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: MarkOutboxPublished :exec
-- ids is a comma separated list: the driver cannot encode an array in the simple protocol (see queries/channels.sql).
UPDATE outbox SET published_at = now(), attempts = attempts + 1, last_error = NULL
WHERE id = ANY(string_to_array(sqlc.arg('ids')::text, ',')::bigint[]);

-- name: MarkOutboxFailed :exec
UPDATE outbox SET attempts = attempts + 1, last_error = sqlc.arg('error')
WHERE id = ANY(string_to_array(sqlc.arg('ids')::text, ',')::bigint[]);

-- name: DeleteOutboxPublishedBefore :execrows
-- A bounded batch per call: the cleaner repeats until nothing is left, so one call never holds a long lock.
DELETE FROM outbox
WHERE id IN (SELECT o.id FROM outbox o WHERE o.published_at IS NOT NULL AND o.published_at < $1 LIMIT 5000);

-- name: CountOutboxPending :one
SELECT count(*) FROM outbox WHERE published_at IS NULL;
