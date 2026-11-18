-- Invitations let a team owner add someone by e-mail address before that person
-- has ever signed in. The invitation is claimed when they sign in with an
-- address their identity provider has verified.

-- Whether the identity provider vouched for the address on the user's last
-- login. Granting anything by e-mail address requires this to be true.
ALTER TABLE users ADD COLUMN email_verified boolean NOT NULL DEFAULT false;

CREATE TABLE tenant_invitations (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  email       text NOT NULL CHECK (email = lower(email)),
  role        text NOT NULL CHECK (role IN ('owner','member')),
  invited_by  uuid REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  UNIQUE (tenant_id, email)
);
CREATE INDEX tenant_invitations_email ON tenant_invitations (email);
