# Roadmap

What is wanted, in rough order of value. These are ideas and invitations, not promises or dates; pick one up by opening an
issue so work is not duplicated. Items marked **(good first)** are small and well bounded.

## Drivers

Plinth's core is neutral about products; each system it manages is a driver (see [docs/drivers](drivers)). Today there are
Kubernetes, PostgreSQL, Gitea and Harbor. Wanted:

- **Git hosts**: GitHub, GitLab, Bitbucket.
- **Registries**: Azure Container Registry, Amazon ECR, Google Artifact Registry, GitHub Container Registry.
- **Databases**: MySQL / MariaDB, and managed services' equivalents of "create a role and a database".
- **Secret stores**: HashiCorp Vault and the cloud secret managers, to deliver credentials somewhere other than a Kubernetes Secret.

## Product

- **Decommissioning**: remove a project and what was created for it, safely (with ownership checks, a dry run, and an audit trail).
  The drivers already know how to delete; nothing exposes it yet.
- **Long-lived API tokens** for scripts and CI, so they do not need a password session.
- **Importing existing projects** that were created by other means, so teams and projects appear in Plinth.
- **Metrics** (`/metrics`) and a `ServiceMonitor` in the chart.
- **Multi-factor authentication** for password accounts.
- **More notification channels** (chat applications), per-person preferences and digests, and e-mail to team members about their
  team's requests.
- **GitOps mode**: commit cluster manifests to a repository instead of applying them directly.

## Chart and operations

- A published container image and chart (see [docs/releasing.md](releasing.md)).
- Optional bundled PostgreSQL for try-outs, autoscaling, and examples for common ingress controllers (**good first**).
- Running the cluster test and the local stack in CI.

## Documentation

- Screenshots of the interface, a getting-started tutorial, and a guide to writing a driver with a worked example (**good first**).
- Translations of the interface.
