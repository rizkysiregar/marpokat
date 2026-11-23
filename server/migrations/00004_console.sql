-- The accounts behind the Marpokat website (PLAN M9): people who sign up, own projects (tenants) and manage them.
-- Accounts are the website's own concern; the chat data (users, channels, messages) never references them.

-- +goose Up

CREATE TABLE accounts (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email             text NOT NULL CHECK (length(email) BETWEEN 3 AND 254),
    name              text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    -- argon2id in PHC string form
    password_hash     text NOT NULL,
    email_verified_at timestamptz,
    is_platform_admin boolean NOT NULL DEFAULT false,
    disabled_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
-- e-mail is the identity, whatever the case
CREATE UNIQUE INDEX accounts_email_lower ON accounts (lower(email));
CREATE TRIGGER accounts_updated_at BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Single use links sent by e-mail. Only the SHA-256 of the token is stored.
CREATE TABLE account_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    kind       text NOT NULL CHECK (kind IN ('verify_email', 'reset_password')),
    token_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX account_tokens_account ON account_tokens (account_id, kind);

-- Opaque session tokens (only the SHA-256 is stored). Sliding expiry with an absolute end.
CREATE TABLE console_sessions (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id          uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    token_hash          bytea NOT NULL UNIQUE,
    created_at          timestamptz NOT NULL DEFAULT now(),
    last_seen_at        timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NOT NULL,
    absolute_expires_at timestamptz NOT NULL,
    user_agent          text NOT NULL DEFAULT '',
    ip                  text NOT NULL DEFAULT ''
);
CREATE INDEX console_sessions_account ON console_sessions (account_id);
CREATE INDEX console_sessions_expiry ON console_sessions (expires_at);

-- Security events of an account, shown to its owner.
CREATE TABLE console_audit (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    action     text NOT NULL,
    target     text,
    ip         text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX console_audit_account ON console_audit (account_id, created_at DESC);

-- Who owns a tenant. Null for tenants made with the CLI.
ALTER TABLE tenants ADD COLUMN account_id uuid REFERENCES accounts (id);
CREATE INDEX tenants_account ON tenants (account_id) WHERE account_id IS NOT NULL;

-- +goose Down
DROP INDEX tenants_account;
ALTER TABLE tenants DROP COLUMN account_id;
DROP TABLE console_audit;
DROP TABLE console_sessions;
DROP TABLE account_tokens;
DROP TABLE accounts;
