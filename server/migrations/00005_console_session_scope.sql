-- Where a console session was signed in (PLAN D71): the portal, or the admin app. Only an admin session can use the
-- platform admin endpoints, so a portal session of an administrator that leaks is not enough to manage the platform.

-- +goose Up
ALTER TABLE console_sessions
    ADD COLUMN scope text NOT NULL DEFAULT 'portal' CHECK (scope IN ('portal', 'admin'));

-- +goose Down
ALTER TABLE console_sessions DROP COLUMN scope;
