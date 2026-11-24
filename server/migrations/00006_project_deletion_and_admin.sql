-- PLAN M9, the console PR after the Vue portal:
-- - a project can be deleted with a grace period (D75): delete_after is when the worker removes it and all its data;
--   until then it is switched off and can be restored
-- - the platform admin reads the whole audit log, newest first, and charts messages per day

-- +goose Up
ALTER TABLE tenants ADD COLUMN delete_after timestamptz;
CREATE INDEX tenants_delete_after ON tenants (delete_after) WHERE delete_after IS NOT NULL;

CREATE INDEX console_audit_created ON console_audit (created_at DESC, id DESC);

-- messages are written in time order, so a BRIN index on created_at is tiny and enough for "per day" counts
CREATE INDEX messages_created_brin ON messages USING brin (created_at);

-- +goose Down
DROP INDEX messages_created_brin;
DROP INDEX console_audit_created;
DROP INDEX tenants_delete_after;
ALTER TABLE tenants DROP COLUMN delete_after;
