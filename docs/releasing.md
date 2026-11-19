# Releasing

How a release is made, and what to do once before the repository becomes public. Releases are made by maintainers.

## Versions

Plinth follows [semantic versioning](https://semver.org). Before 1.0 a minor release (0.x) may contain breaking changes; they are
listed under **Changed** in the [changelog](../CHANGELOG.md) with how to move over, and a change to stored data always comes
with a migration. A patch release contains fixes only.

The container image, the Helm chart and the binaries share one version. The chart's `version` and `appVersion` are set from the
tag when the release is built; the values in `Chart.yaml` in the repository are a placeholder for development.

## Making a release

1. Make sure `main` is green in CI, and run `make e2e` and `make helm-e2e` if the drivers or the chart changed.
2. In [CHANGELOG.md](../CHANGELOG.md) rename **Unreleased** to the new version and date, add a fresh **Unreleased**, and fix the
   comparison links at the bottom. Merge that by pull request.
3. Tag the commit and push the tag:
   ```bash
   git tag -a v0.1.0 -m "v0.1.0"      # add -s to sign it with your GPG or SSH key
   git push origin v0.1.0
   ```
4. The `release` workflow then, for the tag:
   - builds the container image for `linux/amd64` and `linux/arm64` and pushes it to `ghcr.io/<owner>/plinth` with the version
     tag, with an SBOM and build provenance;
   - packages the Helm chart with the version and pushes it as an OCI artifact to `ghcr.io/<owner>/charts`;
   - builds the `plinth` binary (with the web interface) for Linux and macOS on amd64 and arm64, with a `checksums.txt`;
   - **signs** the image and the chart keylessly with [cosign](https://docs.sigstore.dev/cosign/overview/) (the signature names
     this workflow and the tag as the signer);
   - creates the GitHub release with the changelog section for the version and the binaries attached.
5. Check the release page, pull the image, and try `make stack` against it if it was a larger change.

### Verifying a release

```bash
cosign verify ghcr.io/getplinth/plinth:0.1.0 \
  --certificate-identity-regexp '^https://github.com/getplinth/plinth/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check checksums.txt
```

The release workflow has not run yet, so expect to fix a step on the first tag. Move or delete a tag only if nothing was
published for it; a published version is never reused: release the next patch instead.

## Security fixes

A fix for a reported vulnerability is developed in the private advisory's temporary fork, released as a patch version, and then
published together with the advisory. See [SECURITY.md](../SECURITY.md).

## Repository settings

These are configured on `getplinth/plinth` (Settings), and are written down here so they can be checked and restored:

- **Public**, in the `getplinth` organisation, licence detected as Apache-2.0, with a description and topics.
- **Private vulnerability reporting**, **secret scanning with push protection**, and **Dependabot** alerts and security updates are on.
  [SECURITY.md](../SECURITY.md) depends on the first.
- **Ruleset "protect main"** on the default branch: no deletion, no force-push, changes only through a pull request with one approval and
  resolved review threads, and the checks `go`, `web`, `helm`, `integration`, `repo` and `dco` must pass. Repository admins can bypass it, so
  the owner can push fixes directly; use that sparingly, and prefer a pull request so CI runs first.
- **Merging**: squash only, the branch is deleted after merge, and the squash keeps the commit messages (so sign-offs survive).
- **Pull requests from outside contributors** do not run workflows until a maintainer approves the run. Workflows get a read-only token by default.
- **Discussions** are on (the issue templates link to them) and the labels `driver`, `dependencies` and `security` exist next to GitHub's defaults.

## Still to do by hand

- [ ] Install the **Renovate** app on the `getplinth` organisation for this repository (its configuration is [renovate.json](../renovate.json)).
- [ ] After the first release, make the **packages** on `ghcr.io` (the image and the chart) public so they can be pulled without signing in.
- [ ] Put a real address or channel for **code of conduct** reports in [CODE_OF_CONDUCT.md](../CODE_OF_CONDUCT.md); today it uses private reporting.
- [ ] Add a second maintainer when there is one, and then require review from code owners in the ruleset.

The author name and e-mail address on the commits became public with the repository. Rewriting the history now would not remove copies that
others have already fetched, so only do it if you must, and expect a force-push to need the admin bypass.
