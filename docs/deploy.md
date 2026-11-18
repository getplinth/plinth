# Deploying on Kubernetes

The Helm chart in [`deploy/helm/plinth`](../deploy/helm/plinth) runs Plinth as a small stateless Deployment. All state lives in PostgreSQL, so replicas are interchangeable and upgrades do not interrupt anyone.

```mermaid
flowchart LR
  U[Browsers] --> I[Ingress]
  I --> S[Service]
  S --> P1[Plinth pod]
  S --> P2[Plinth pod]
  P1 --> DB[(PostgreSQL)]
  P2 --> DB
  P1 -. discovery, token keys .-> IDP[Identity provider]
  U -. sign-in .-> IDP
  P1 --> X["Systems it manages<br/>Git, registry, clusters, databases"]
```

The chart does **not** run a database. Use a managed PostgreSQL or an operator, and back it up: it holds requests, blueprints and the encrypted integration settings.

## Before you install

1. **A PostgreSQL database** and a user that owns it. Plinth creates its tables at start.
2. **An OpenID Connect client** for Plinth at your identity provider:
   - public client, authorization code flow with PKCE (S256);
   - redirect URI `https://<your Plinth address>/callback`;
   - CORS allowed for that address on the discovery and token endpoints;
   - a `groups` claim in the ID token, and a group for administrators (default `plinth-admins`).
3. **Two Secrets** in the namespace you install into:

```bash
kubectl create namespace platform

# 32 random bytes that encrypt the integration settings. Keep a copy somewhere safe:
# without it, stored integrations cannot be read.
kubectl -n platform create secret generic plinth-master-key \
  --from-literal=master-key="$(docker run --rm ghcr.io/getplinth/plinth:0.1.0 genkey)"

kubectl -n platform create secret generic plinth-database \
  --from-literal=database-url='postgres://plinth:PASSWORD@db.example.com:5432/plinth?sslmode=verify-full'
```

## Install

```bash
helm install plinth deploy/helm/plinth --namespace platform \
  --set config.oidc.issuer=https://login.example.com/realms/platform \
  --set config.oidc.audience=plinth \
  --set database.existingSecret=plinth-database \
  --set masterKey.existingSecret=plinth-master-key \
  --set ingress.enabled=true --set ingress.className=nginx \
  --set 'ingress.hosts={plinth.example.com}' \
  --set 'ingress.tls[0].secretName=plinth-tls' --set 'ingress.tls[0].hosts={plinth.example.com}'

helm test plinth --namespace platform
```

`helm test` checks health, the database connection, the sign-in settings, and that the API refuses anonymous callers. For anything beyond a try-out put your settings in a values file (see [`ci/full-values.yaml`](../deploy/helm/plinth/ci/full-values.yaml)).

If a required value is missing the chart stops with a sentence that says what to set, instead of installing something that crash-loops.

## Upgrade, roll back, remove

```bash
helm upgrade plinth deploy/helm/plinth --namespace platform --reuse-values --set image.tag=0.2.0
helm rollback plinth 1 --namespace platform
helm uninstall plinth --namespace platform      # keeps your Secrets and the database
```

A new pod is started and ready before an old one stops (`maxUnavailable: 0`), a stopping pod keeps serving for a few seconds while the Service drops it (so no request lands on a closed port), a disruption budget keeps one replica through node drains, and a stopping pod hands running onboarding requests back to the queue so another replica continues them. Database migrations take a lock, so replicas starting together are safe. Plinth only moves **forward**: rolling back the chart does not undo database migrations, so only roll back to a version whose schema is compatible.

## Values

| Value | Default | Meaning |
|---|---|---|
| `config.oidc.issuer`, `config.oidc.audience` | required | identity provider and client id |
| `config.oidc.adminGroup` / `groupsClaim` | `plinth-admins` / `groups` | who is an administrator |
| `config.oidc.allowHttp` | `false` | accept an `http://` issuer (testing only) |
| `config.oidc.trustUnverifiedEmail` | `false` | accept e-mail addresses the provider did not verify, for invitations and add-by-email (see [the API notes](api.md#teams)) |
| `config.workers` | `2` | onboarding requests processed at once, per replica |
| `database.existingSecret` (+ `existingSecretKey`) | required | Secret holding the PostgreSQL URL |
| `database.url` | | alternative: the chart creates the Secret (the URL is then visible in `helm get values`) |
| `masterKey.existingSecret` (+ `existingSecretKey`) | required | Secret holding the master key |
| `masterKey.value` | | alternative: inline (visible in `helm get values`) |
| `replicaCount` | `2` | replicas; they share the work through the database |
| `image.repository`, `tag`, `digest` | `ghcr.io/getplinth/plinth`, appVersion | pin a `digest` for reproducible installs |
| `ingress.*` | off | class, annotations, `hosts`, `tls` |
| `networkPolicy.enabled`, `ingressFrom` | off | who may reach Plinth (egress stays open) |
| `podDisruptionBudget.*` | `minAvailable: 1`, only with 2+ replicas | node drains |
| `shutdownDelaySeconds` | `5` | a stopping pod keeps serving this long while the Service stops sending it traffic; prevents failed requests during upgrades and drains (0 disables) |
| `resources` | 100m / 128Mi, memory limit 512Mi | |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpread`, `priorityClassName` | | scheduling |
| `extraEnv`, `podAnnotations`, `podLabels`, `serviceAccount.*` | | extras |

The full list, with comments, is in [`values.yaml`](../deploy/helm/plinth/values.yaml); values are type-checked by [`values.schema.json`](../deploy/helm/plinth/values.schema.json).

## What the chart does for security

- Runs as a non-root user with a read-only root filesystem, all capabilities dropped, no privilege escalation and the runtime-default seccomp profile. Only `/tmp` is writable (git copies template repositories there).
- Mounts no Kubernetes API token and creates no cluster roles or role bindings: Plinth manages other systems with the credentials in its integrations and never calls this cluster's API as itself.
- The database URL and master key reach the pod only as `secretKeyRef` environment variables. They are never written into the pod spec, and the cluster test checks that they are absent from the pod specs and logs.
- A chart-managed Secret changes the pod template when it changes, so pods roll to pick it up.
- Every object names its namespace, so the chart is safe to render with Argo CD or Flux.
- Optional NetworkPolicy limits who can reach Plinth. Outgoing traffic is deliberately not restricted: Plinth must reach your identity provider, database, Git host, registry and clusters.

## Operating it

| | |
|---|---|
| Health | `/healthz` (process up), `/readyz` (database answers). A replica that loses the database leaves the Service and is not restarted. |
| Logs | JSON on stdout. They never contain tokens, bodies, query strings or secrets. |
| Back up | the PostgreSQL database **and** the master key, separately. Either alone is useless for restoring integrations. |
| Master key | there is no rotation yet. Treat the key as long-lived and protect it. |
| Scaling | add replicas for availability; raise `config.workers` for more simultaneous requests. |

## Tests

| Command | What it proves |
|---|---|
| `make helm-test` | `helm lint`, then the chart is rendered with many value sets and checked as typed Kubernetes objects: what is created, hardening, how values flow through, and that every misconfiguration is refused with a clear message |
| `make helm-e2e` | installs the chart on a throwaway k3d cluster with a real database and an identity provider, then checks: both replicas ready and migrated safely, `helm test`, sign-in and roles, settings encrypted in the database, hardening inside the running pods, an upgrade with zero failed requests while a client calls the Service ten times a second (scaling to 3 and changing configuration), rollback, losing a replica, and uninstall keeping your Secrets and data |

## Not included

A bundled database, a metrics endpoint with a `ServiceMonitor`, autoscaling (replicas are for availability, not load), master key rotation, and a published image: set `image.repository` to where you push yours.
