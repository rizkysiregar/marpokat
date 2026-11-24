-- PLAN D78, D79: a project can carry a label (development, staging, production) that changes nothing but how it is
-- shown, and the platform admin can suspend it. A suspension is separate from the owner's switch off (disabled_at):
-- the owner cannot lift it.

-- +goose Up
ALTER TABLE tenants
    ADD COLUMN environment text CHECK (environment IN ('development', 'staging', 'production')),
    ADD COLUMN suspended_at timestamptz,
    ADD COLUMN suspended_reason text CHECK (length(suspended_reason) <= 500);

-- +goose Down
ALTER TABLE tenants DROP COLUMN suspended_reason, DROP COLUMN suspended_at, DROP COLUMN environment;
