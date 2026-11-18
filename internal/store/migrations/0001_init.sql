-- Plinth schema (pre-release: edited in place until the first tag, then
-- changed only by new numbered files).
-- Secret VALUES are never stored in clear text: integration credentials are
-- envelope-encrypted, and provisioned credentials are not stored at all.

CREATE TABLE users (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  oidc_sub    text UNIQUE NOT NULL,
  email       text NOT NULL,
  role        text NOT NULL CHECK (role IN ('admin','user')),  -- refreshed from the token on each login
  created_at  timestamptz NOT NULL DEFAULT now(),
  last_seen   timestamptz NOT NULL DEFAULT now()
);

-- A configured external system (git host, cluster, registry, DB server, ...).
CREATE TABLE integrations (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind        text NOT NULL CHECK (kind IN ('gitsource','registry','cluster','database','secrets')),
  driver      text NOT NULL,            -- gitea | harbor | kubernetes | postgres | ...
  name        text UNIQUE NOT NULL,
  config_enc  bytea NOT NULL,           -- envelope-encrypted JSON, bound to this row's id
  config_keys text[] NOT NULL,          -- key NAMES only, so the UI can show what is set
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Namespace resource tiers (bronze/silver/gold ...). Seeded from internal/quota.
CREATE TABLE quota_tiers (
  name        text PRIMARY KEY,
  spec        jsonb NOT NULL            -- quota.Tier
);

-- A platform combination: which integrations a project gets, plus defaults.
CREATE TABLE blueprints (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name           text UNIQUE NOT NULL,
  gitsource_id   uuid NOT NULL REFERENCES integrations(id),
  registry_id    uuid REFERENCES integrations(id),
  git_parent     text NOT NULL DEFAULT '',          -- org / parent group for tenants
  template_repo  text NOT NULL DEFAULT '',          -- golden template
  template_vars  jsonb NOT NULL DEFAULT '{}',       -- {"PROJECT_NAME": "{project}"}
  extra_branches text[] NOT NULL DEFAULT '{}',
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now()
);

-- Environments of a blueprint (e.g. dev, preprod, prod).
CREATE TABLE blueprint_environments (
  blueprint_id    uuid NOT NULL REFERENCES blueprints(id) ON DELETE CASCADE,
  env             text NOT NULL,
  position        int  NOT NULL,                     -- provisioning order
  cluster_id      uuid NOT NULL REFERENCES integrations(id),
  database_id     uuid REFERENCES integrations(id),
  access_prefix   text NOT NULL DEFAULT '',          -- OIDC group prefix on that cluster
  ingress_ns      text NOT NULL DEFAULT '',          -- where the ingress controller runs
  PRIMARY KEY (blueprint_id, env)
);

-- Tenant = team. Created on the first request; the requester becomes its owner.
CREATE TABLE tenants (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name        text UNIQUE NOT NULL,                  -- normalized
  owner_id    uuid NOT NULL REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE requests (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  blueprint_id  uuid NOT NULL REFERENCES blueprints(id),
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  requested_by  uuid NOT NULL REFERENCES users(id),
  payload       jsonb NOT NULL,                      -- validated request.Request
  status        text NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','running','succeeded','failed')),
  error         text NOT NULL DEFAULT '',
  state         jsonb NOT NULL DEFAULT '{"steps":{},"outputs":{}}',  -- orchestrator.State
  claimed_by    text,
  claimed_at    timestamptz,                         -- worker lease; refreshed on every state save
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX requests_queue ON requests (status, claimed_at);

CREATE TABLE projects (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  name        text NOT NULL,                         -- normalized
  -- naming.Namespace(tenant, project), also the registry project and git path
  -- component. Globally unique: tenant "a-b" + project "c" and tenant "a" +
  -- project "b-c" would otherwise share a namespace, registry project and
  -- credentials (the legacy naming has this collision).
  namespace   text NOT NULL UNIQUE,
  request_id  uuid NOT NULL REFERENCES requests(id),
  spec        jsonb NOT NULL,                        -- request.Project
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);

CREATE TABLE audit_log (
  id         bigserial PRIMARY KEY,
  at         timestamptz NOT NULL DEFAULT now(),
  actor      text NOT NULL,                          -- user email, or "system"
  action     text NOT NULL,
  target     text NOT NULL,
  detail     jsonb
);
