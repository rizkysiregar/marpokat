-- A message together with its sender's public profile, so every read path gets the same shape from one query.
-- The sender is optional (system messages), hence the LEFT JOIN and the nullable sender_* columns.

-- +goose Up
CREATE VIEW message_details AS
SELECT m.channel_id,
       m.seq,
       m.id,
       m.tenant_id,
       m.sender_id,
       m.client_msg_id,
       m.type,
       m.text,
       m.attachments,
       m.reply_to_id,
       m.reaction_counts,
       m.metadata,
       m.rev_seq,
       m.edited_at,
       m.deleted_at,
       m.created_at,
       u.external_id AS sender_external_id,
       u.name        AS sender_name,
       u.avatar_url  AS sender_avatar_url,
       u.metadata    AS sender_metadata,
       u.deleted_at  AS sender_deleted_at
FROM messages m
LEFT JOIN users u ON u.id = m.sender_id;

-- +goose Down
DROP VIEW message_details;
