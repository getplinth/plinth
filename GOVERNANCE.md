# Governance

Plinth is a maintainer-led project. This page says who decides what, so that contributors know what to expect. It is
deliberately small and will grow when the project does.

## Roles

- **Contributors** are everyone who files an issue, reviews, improves the docs or sends a pull request. Anyone can be one.
- **Maintainers** ([MAINTAINERS.md](MAINTAINERS.md)) review and merge changes, triage issues, cut releases and keep the
  project healthy. They can approve pull requests and have write access.
- **The project owner** is the maintainer who started the project. They hold the GitHub organisation and the project's
  accounts, break ties when maintainers disagree, and act on conduct and security reports about a maintainer's behaviour
  together with another maintainer where there is one.

## How decisions are made

- **Most changes** are decided in pull requests by lazy consensus: a maintainer approves, nobody with standing objects, it merges.
- **Design decisions** (a new driver interface, a change to the data model or the API, a new dependency, a change to security
  behaviour) are discussed in an issue first. A maintainer states a proposed decision, leaves it open for at least a week so that
  others can respond, and decides in light of the discussion, writing the reasoning down.
- **Breaking changes** (configuration, API, schema, behaviour that people rely on) need such an issue, a note in the
  [changelog](CHANGELOG.md) and, for anything that touches stored data, a migration that works on a database that holds data.
  Before 1.0 they are allowed with that care; after 1.0 they follow [semantic versioning](https://semver.org).
- **Security** issues are handled privately by the maintainers as described in [SECURITY.md](SECURITY.md), and the decision and
  its timing are theirs.
- **Disagreement** is settled by discussion first. If maintainers cannot agree, the project owner decides.

## Becoming a maintainer

Sustained, good contributions (code, reviews, docs, helping people) and a demonstrated understanding of the project's rules
in [CONTRIBUTING.md](CONTRIBUTING.md), especially about secrets, idempotency and testing. An existing maintainer nominates
the person in an issue or privately; if no maintainer objects within a week, they are invited. A maintainer who is inactive
for a year, or who asks to, is moved to emeritus with thanks, and can come back by the same route.

## Conduct

Everyone follows the [Code of Conduct](CODE_OF_CONDUCT.md). Maintainers are held to it too.

## Changing this page

By pull request, discussed like a design decision.
