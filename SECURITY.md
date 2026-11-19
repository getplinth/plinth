# Security policy

Plinth holds administrative credentials for the systems it manages (Git hosts, container registries, Kubernetes clusters,
database servers) and creates credentials for projects. We take its security seriously and welcome reports.

## Reporting a vulnerability

**Please do not open a public issue or pull request for a security problem.** Report it privately with GitHub's private
vulnerability reporting: on the repository open the **Security** tab and choose **Report a vulnerability**.

Please include what you found, how to reproduce it (a version, a configuration, steps), what an attacker gains, and anything you
know about a fix. Do not include real credentials or other people's data.

What to expect from us:

| Step | Target |
|---|---|
| Acknowledge your report | within 3 working days |
| First assessment (is it valid, how severe) | within 7 days |
| Fix or mitigation for a confirmed high or critical issue | as fast as we can, aiming at 30 days |
| Public disclosure | after a fix is released, coordinated with you, and in any case within 90 days of the report unless we agree otherwise |

We credit reporters in the advisory and the changelog unless you prefer not to be named. We do not run a bounty programme.

## Supported versions

Plinth is before version 1.0. Security fixes go into the latest release and into `main`; please test against those before you
report. Once 1.0 is released this page will name the supported release lines.

## What counts

In scope, for example:

- authentication or authorisation bypass, including reaching another team's requests, projects, members or credentials, or
  acting as an administrator without being one;
- exposure of a secret: integration credentials, master keys, session tokens, passwords, or the credentials created for a
  project, through the API, logs, the audit log, notifications, errors or storage;
- flaws in how credentials are encrypted at rest, in key rotation, in sign-in (passwords, sessions, token verification, the
  handling of several identity providers) or in the invitation and sign-in checks that rely on verified e-mail addresses;
- injection (SQL, command, header, template), request forgery that makes Plinth contact addresses it should not (for example
  through a provider's configuration), and path traversal;
- the Helm chart or container image making a deployment less safe than documented.

Out of scope:

- anything that needs an administrator's access to Plinth, who is trusted with the systems it manages by design;
- `make demo` and `make stack`: they are for local testing, use generated throwaway passwords, plain http, and stand-in or
  unauthenticated services, and say so;
- findings in a dependency without a way to reach it through Plinth (report those upstream, or tell us if Plinth is affected);
- denial of service by sheer volume, missing hardening headers on pages that carry no secrets, and social engineering.

## How Plinth protects what it holds

This is the design you can expect, so a deviation from it is a bug worth reporting. See [docs/api.md](docs/api.md) and
[docs/architecture.md](docs/architecture.md) for detail.

- **Integration credentials are encrypted at rest** (envelope encryption: a data key per record, AES-GCM, bound to the record),
  with a master key that you supply, can rotate without downtime, and that is never stored in the database or printed.
- **Credentials created for a project** (a registry robot, a database role) are delivered to their destination once and are
  **not stored**: the request state, API responses, audit log and notifications do not contain them.
- **Sign-in:** passwords are stored as argon2id hashes, guessing is throttled and locks accounts, sessions are opaque tokens
  stored only as digests with idle and absolute limits, identity provider tokens are verified against the issuer named in
  the token, and a provider cannot produce the identity of a password account.
- **Least privilege:** integrations should use dedicated identities with limited rights. The Kubernetes driver's role
  (`deploy/kubernetes/plinth-rbac.yaml`) is tested to be unable to read Secrets or weaken tenant isolation.
- **Every administrative action is audit-logged**, without secret values.
- **The container and chart run hardened:** non-root, read-only root filesystem, no added capabilities, secrets only from
  Kubernetes Secrets.

## Known limits

- Password accounts have no second factor. For production, sign in through an identity provider that offers one and turn password
  sign-in off.
- Administrators are fully trusted. Plinth does not defend the systems it manages against its own administrators.
- Delivery of notifications is at least once, and e-mail content is not encrypted end to end.
