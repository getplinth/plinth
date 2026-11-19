## What and why

<!-- What does this change, and why is it needed? Link the issue it closes ("Closes #123"). -->

## How I checked it

<!-- The commands you ran and what you saw: tests added, `make test`, `make itest-app`, `make helm-test`, trying it in `make stack`. -->

## Checklist

- [ ] My commits are signed off (`git commit -s`): I certify the [Developer Certificate of Origin](../blob/main/DCO)
- [ ] There is a test that fails without this change (for a driver, one against a real disposable instance)
- [ ] I updated the documentation in `docs/` and the [changelog](../blob/main/CHANGELOG.md) for anything a user will notice
- [ ] No secret can reach a log, an error, the audit log, a response or the stored state because of this
- [ ] A schema change is a new migration that works on a database that holds data
- [ ] A new dependency is explained here and passes `make licenses`

**Breaking change?** <!-- If yes: what breaks, and how do people move over? -->
