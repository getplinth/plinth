# Changelog

All notable changes are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0, a minor release may contain breaking
changes; they are called out under **Changed** with how to move over.

## [Unreleased]

First public development version. Plinth lets developers request a project and creates the repository, registry project,
namespace (with RBAC, quota and network isolation) and database, delivers the credentials once, and reports progress.

### Added

- **Engine**: requests expand into idempotent, resumable steps with retry; the state is saved before each attempt, a request is
  claimed by a worker with a lease (so replicas share the queue), and credentials created for a project are redacted from
  everything stored and rotated on any retry.
- **Drivers**: Kubernetes (namespace, ResourceQuota, LimitRange, RBAC, NetworkPolicy, image pull secret), PostgreSQL
  (database and role), Gitea (organisation, repository from a template, branches, variables) and Harbor (project, robot
  account). Product-neutral interfaces for Git hosts, registries, clusters and databases.
- **REST API and web interface**: integrations and blueprints (admin), requests with live progress, retry, audit log.
- **Approval** for sensitive blueprints, with four-eyes: nobody approves their own request.
- **Teams** with owners and members, invitations for people who have not signed in yet (claimed only by an e-mail address the
  identity provider verified), and a team name that cannot be used by two teams.
- **Quota tiers** that administrators manage, validated so the cluster never receives a tier it would refuse.
- **Releasing the names** of a failed request, with a record of what was left behind.
- **Notifications** by e-mail (SMTP, STARTTLS verified) and webhook (signed), queued in the database with the change they
  report, delivered at least once with retries.
- **Sign-in** with a password out of the box (argon2id, throttled, lockout, revocable sessions), and any number of OpenID
  Connect identity providers configured in the control panel, with safeguards against locking yourself out. The first
  administrator is created at install.
- **Master key rotation** without downtime (`plinth keys`, `plinth rekey`).
- **Helm chart**: hardened, rolling upgrades without failed requests, tested on a real cluster.
- **`make demo`** (simulated systems) and **`make stack`** (a complete local stack on real Gitea, Kubernetes, PostgreSQL, Mailpit,
  and optionally Harbor and Keycloak).
- Open-source project files: Apache-2.0 licence, contributing guide with DCO sign-off, code of conduct, security policy,
  governance, issue and pull-request templates, and release automation.

[Unreleased]: https://github.com/getplinth/plinth/commits/main
