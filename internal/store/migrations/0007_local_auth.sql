-- Sign-in without an external identity provider, and identity providers that an
-- administrator configures in the control panel instead of at install time.

-- Where an account comes from. OIDC users are created on their first login;
-- local users are created by an administrator (or the bootstrap) and sign in
-- with a password. Their oidc_sub is "local:<id>".
ALTER TABLE users ADD COLUMN auth_source text NOT NULL DEFAULT 'oidc' CHECK (auth_source IN ('oidc', 'local'));

CREATE TABLE local_accounts (
  user_id             uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  username            text NOT NULL UNIQUE CHECK (username = lower(username)),
  password_hash       text NOT NULL,                 -- argon2id, never the password
  must_change         boolean NOT NULL DEFAULT false, -- a password someone else chose must be replaced
  disabled            boolean NOT NULL DEFAULT false,
  failed_attempts     int NOT NULL DEFAULT 0,
  locked_until        timestamptz,
  password_changed_at timestamptz NOT NULL DEFAULT now(),
  last_login          timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now()
);

-- Sessions for local sign-in. The token is shown once; only its SHA-256 is stored.
CREATE TABLE sessions (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  token_hash          bytea NOT NULL UNIQUE,
  user_id             uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at          timestamptz NOT NULL DEFAULT now(),
  last_used_at        timestamptz NOT NULL DEFAULT now(),
  expires_at          timestamptz NOT NULL,           -- idle timeout, moves forward while in use
  absolute_expires_at timestamptz NOT NULL            -- never extended
);
CREATE INDEX sessions_user ON sessions (user_id);
CREATE INDEX sessions_expiry ON sessions (absolute_expires_at);

-- OpenID Connect providers configured by administrators. The browser signs in
-- as a public client (authorization code with PKCE), so no client secret exists.
CREATE TABLE oidc_providers (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name                   text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,30}$'),
  display_name           text NOT NULL,
  issuer                 text NOT NULL,
  client_id              text NOT NULL,
  scopes                 text NOT NULL DEFAULT 'openid profile email groups',
  groups_claim           text NOT NULL DEFAULT 'groups',
  admin_group            text NOT NULL DEFAULT '',
  trust_unverified_email boolean NOT NULL DEFAULT false,
  enabled                boolean NOT NULL DEFAULT true,
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  UNIQUE (issuer, client_id)
);

CREATE TABLE auth_settings (
  only_row            boolean PRIMARY KEY DEFAULT true CHECK (only_row),
  local_login_enabled boolean NOT NULL DEFAULT true
);
INSERT INTO auth_settings DEFAULT VALUES;
