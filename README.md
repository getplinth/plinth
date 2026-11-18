# Plinth

Self-service project onboarding for platform teams. Admins connect the systems
they already run (Git hosting, container registry, Kubernetes clusters,
databases, secret stores) and bundle them into **blueprints**. Developers fill
in one form; Plinth provisions the repository, registry project, namespace
(with RBAC, quota and network policy) and database, then reports status.

> Status: the engine, API and web interface work and are tested end to end against real systems. Try the interface with `make demo` (see [docs/ui.md](docs/ui.md)). Not built yet: tenant members.

## Concepts

- **Integration**: a configured external system (e.g. a GitHub org, a cluster).
- **Provider**: the driver behind an integration (`github`, `gitlab`, `harbor`, `postgres`, ...).
- **Blueprint**: a versioned combination of integrations plus defaults and the request form schema.
- **Request**: a developer's onboarding order; expands into idempotent, resumable steps.

Plinth is product-neutral: GitHub or GitLab, any container registry, any Kubernetes distribution, Postgres or MySQL are all just drivers.

Docs: [architecture](docs/architecture.md) (Mermaid diagrams), [deploying on Kubernetes](docs/deploy.md), [web interface](docs/ui.md), [API and operation](docs/api.md), [onboarding flow](docs/onboarding.md), and one page per driver in [docs/drivers](docs/drivers).

## Develop

```bash
make demo        # try the web interface with simulated systems
make test        # Go and web unit tests
make itest-app   # whole application on a disposable PostgreSQL (needs Docker)
make itest       # all integration tests: application, Kubernetes, Postgres, Gitea, Harbor
make e2e         # one request through all real drivers (slow)
./bin/plinth genkey   # create a PLINTH_MASTER_KEY
```

Configuration is by `PLINTH_*` environment variables, listed in [docs/api.md](docs/api.md).

## Layout

```
cmd/plinth/            entrypoint
internal/api/          REST API
internal/provider/     provider interfaces + driver registry
internal/orchestrator/ step runner (retry, resume)
internal/store/        Postgres access + migrations
internal/config/       env configuration
web/                   React web interface (embedded in the binary)
deploy/helm/plinth/     Helm chart (see docs/deploy.md)
docs/                  architecture, legacy mapping
```
