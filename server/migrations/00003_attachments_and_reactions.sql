-- Attachments are linked to messages through a table instead of a JSON list: one attachment can belong to only one
-- message (a constraint, so two concurrent sends cannot both take it), the attachments of a message come back in
-- order with one join, and finding what nobody uses any more (for the cleaner) is a plain query.

-- +goose Up
DROP VIEW message_details;
ALTER TABLE messages DROP COLUMN attachments;

CREATE VIEW message_details AS
SELECT m.channel_id,
       m.seq,
       m.id,
       m.tenant_id,
       m.sender_id,
       m.client_msg_id,
       m.type,
       m.text,
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

CREATE TABLE message_attachments (
    message_id    uuid NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    attachment_id uuid NOT NULL UNIQUE REFERENCES attachments (id),
    position      smallint NOT NULL,
    PRIMARY KEY (message_id, attachment_id)
);
CREATE INDEX message_attachments_message_idx ON message_attachments (message_id, position);

-- the cleaner walks attachments from the oldest
CREATE INDEX attachments_created_idx ON attachments (created_at);

-- +goose Down
DROP INDEX attachments_created_idx;
DROP TABLE message_attachments;
DROP VIEW message_details;
ALTER TABLE messages ADD COLUMN attachments jsonb NOT NULL DEFAULT '[]'::jsonb;
CREATE VIEW message_details AS
SELECT m.channel_id, m.seq, m.id, m.tenant_id, m.sender_id, m.client_msg_id, m.type, m.text, m.attachments,
       m.reply_to_id, m.reaction_counts, m.metadata, m.rev_seq, m.edited_at, m.deleted_at, m.created_at,
       u.external_id AS sender_external_id, u.name AS sender_name, u.avatar_url AS sender_avatar_url,
       u.metadata AS sender_metadata, u.deleted_at AS sender_deleted_at
FROM messages m
LEFT JOIN users u ON u.id = m.sender_id;
