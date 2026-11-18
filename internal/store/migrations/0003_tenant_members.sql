-- Teams have members, not a single owner. Owners manage the team and its
-- members; members can request projects, see the team's requests and retry
-- them. Administrators can do everything for every team.

CREATE TABLE tenant_members (
  tenant_id  uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  user_id    uuid NOT NULL REFERENCES users(id),
  role       text NOT NULL CHECK (role IN ('owner','member')),
  added_by   uuid REFERENCES users(id),
  added_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX tenant_members_user ON tenant_members (user_id);

-- Everyone who owned a tenant before this migration stays its owner.
INSERT INTO tenant_members (tenant_id, user_id, role, added_by)
  SELECT id, owner_id, 'owner', owner_id FROM tenants;

COMMENT ON COLUMN tenants.owner_id IS 'who created the tenant; tenant_members is authoritative for access';

-- Members are added by e-mail address, compared without regard to case.
CREATE INDEX users_email_lower ON users (lower(email));
