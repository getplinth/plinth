# Contributing to Plinth

Thank you for helping. Plinth is a self-service platform that creates repositories, registry projects, namespaces and
databases for development teams, and it holds administrative credentials for the systems it manages. That shapes what
we ask of changes: they must be correct, tested against the real systems where it matters, and careful with secrets.

By taking part you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md). Vulnerabilities are not reported in public
issues: see [SECURITY.md](SECURITY.md).

## Ways to help

- **Report a bug** with the [bug template](https://github.com/getplinth/plinth/issues/new/choose). Say what you ran, what you
  expected and what happened, and include the Plinth version and the relevant log lines, **without secrets**.
- **Add a driver** for another Git host, registry, cluster type or database. This is the most useful kind of contribution:
  Plinth's core is deliberately neutral about products and only talks to interfaces. See [docs/drivers](docs/drivers) and
  [Writing a driver](#writing-a-driver). [docs/roadmap.md](docs/roadmap.md) lists what is wanted.
- **Improve the documentation**: anything you had to guess, or found wrong, is worth a fix.
- **Pick up an issue** labelled `good first issue` or `help wanted`.

For anything bigger than a small fix, **open an issue first** and describe the problem (not only the solution) so we can agree
on the approach before you spend time on it.

## Set up

You need Go (the version in `go.mod`), Node 22 for the web interface, and Docker for the integration tests. `kubectl`, `k3d`
and `helm` are needed for the Kubernetes and chart tests.

```bash
git clone https://github.com/getplinth/plinth && cd plinth
make test          # Go and web unit tests: fast, no Docker
make demo          # the interface with simulated systems, to look around
make stack         # a complete local stack on real systems (see docs/stack.md)
```

What CI runs, and so what to run before you open a pull request:

| Command | What it checks |
|---|---|
| `make lint` | `go vet` and `gofmt` |
| `make test` | Go and web unit tests |
| `make helm-test` | the Helm chart: lint and render tests for many value sets (needs `helm`) |
| `make itest-app` | the whole application on a disposable PostgreSQL (needs Docker) |
| `make itest` | all integration tests: application, Kubernetes, PostgreSQL, Gitea, Harbor (Docker; Harbor is slow) |
| `make licenses` | the licences of everything that ships; fails on copyleft |
| `make check-scripts` | the tests of the repository's own scripts (DCO check, licence check, links in the docs) |

`make e2e` runs one request through every real driver and `make helm-e2e` installs the chart on a throwaway cluster; they are
slow and run on request, but run them if you touch the drivers or the chart.

The web interface lives in `web/` (`cd web && npm ci && npm run dev` proxies to `make demo`).

## What we look for in a change

- **A test that fails without it.** Behaviour is tested, not only built. Code that talks to a real system (a driver) is
  tested against a disposable instance of that system, not only against a mock: see the `itest-*` targets.
- **The neutral core stays neutral.** Product names belong in drivers and their docs, not in `internal/onboarding`, the API or
  the store.
- **Steps are idempotent and resumable.** Running a step twice, or after a crash halfway, must give the same result. Anything
  that issues a credential must cope with an earlier attempt having issued it without delivering it.
- **Secrets never leak.** Not into logs, error messages, the audit log, the API's responses, the request state or the
  database in clear text. Credentials created for a project are delivered and not stored. Tests scan for this; keep them
  passing and add to them.
- **Say what went wrong and what to do.** Error messages are sentences for the person who sees them, written without
  blame, never vague.
- **Document it.** User-visible behaviour, configuration and API changes update the relevant page in `docs/` in the same pull
  request, and diagrams are Mermaid so they stay reviewable.
- **A schema change is a new migration**, never an edit of an old one, and works on a database that already holds data.
- **No new dependency without a reason.** Say why in the pull request; its licence must pass `make licenses`.

## Writing a driver

A driver implements one of the interfaces in `internal/provider` (`GitSource`, `Registry`, `Cluster`, `Database`) and registers
itself under a name. Read an existing one first (`internal/provider/gitea` is a small example) and the page for it in
[docs/drivers](docs/drivers). A new driver comes with:

1. the implementation, with its configuration keys validated up front and `Validate` checking the connection;
2. **an integration test against a real, disposable instance** run by a `make itest-...` target (and added to CI if it is
   reasonably quick), plus unit tests for what needs no instance;
3. a documentation page like the existing ones: configuration keys, how Plinth's concepts map onto the system's, the
   permissions the account needs, and what is not supported;
4. idempotency and ownership handled as in the other drivers: it only takes over what it created unless the administrator
   asked it to adopt existing objects.

## Commits and pull requests

- **Sign off your commits** (`git commit -s`). This adds a `Signed-off-by: Your Name <you@example.com>` line, by which you
  certify the [Developer Certificate of Origin](DCO): that you wrote the change or have the right to submit it under the
  project's licence. CI checks every commit in a pull request. If you forgot: `git commit --amend -s --no-edit` for the last
  one, or `git rebase --signoff main` for several, then push again with `--force-with-lease`.
- Use your real name and an address you control. The sign-off is a public, permanent record.
- Write the subject in the imperative, at most about 70 characters ("Add a webhook retry limit"), and explain in the body why
  the change is needed and how you checked it.
- Keep a pull request to one logical change, and fill in the template. Reviews are about the change, with the aim of
  getting it merged; we may ask for tests or documentation.
- Maintainers merge when CI is green and a maintainer has approved. Pull requests are squash-merged, and the sign-offs are kept.
- By contributing you license your work under the [Apache License 2.0](LICENSE), the same as the rest of the project.

### Using AI tools

You may use them, and you are responsible for what you submit: you must understand it, have the right to contribute it, have
run it, and sign it off. Generated text or code that nobody has checked will be asked to be redone.

## Getting help

Open a [discussion](https://github.com/getplinth/plinth/discussions) for questions and ideas, and an issue for bugs and
concrete proposals. For how decisions are made, see [GOVERNANCE.md](GOVERNANCE.md).
