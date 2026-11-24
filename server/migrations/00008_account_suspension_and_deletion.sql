-- PLAN D80: the platform admin suspends an account (disabled_at, with a reason) and with it every project of the
-- account; an account can be deleted (delete_after: by its owner after the grace period, by the admin after it or
-- now). A project remembers why it is suspended, so lifting an account's suspension does not lift a suspension the
-- admin put on the project itself.

-- +goose Up
ALTER TABLE accounts
    ADD COLUMN suspended_reason text CHECK (length(suspended_reason) <= 500),
    ADD COLUMN delete_after timestamptz;
CREATE INDEX accounts_delete_after ON accounts (delete_after) WHERE delete_after IS NOT NULL;

ALTER TABLE tenants ADD COLUMN suspension_source text CHECK (suspension_source IN ('project', 'account'));
UPDATE tenants SET suspension_source = 'project' WHERE suspended_at IS NOT NULL;

-- +goose Down
ALTER TABLE tenants DROP COLUMN suspension_source;
DROP INDEX accounts_delete_after;
ALTER TABLE accounts DROP COLUMN delete_after, DROP COLUMN suspended_reason;
