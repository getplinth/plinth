# Third-party notices

Plinth is licensed under the Apache License 2.0 ([LICENSE](LICENSE), [NOTICE](NOTICE)). It includes and
depends on the software below. `make licenses` re-checks the licences of every dependency, and CI fails if a
copyleft licence (GPL, AGPL, LGPL) appears among the dependencies that ship in the product.

## Bundled in the web interface (and so in the binary and the container image)

| Component | Licence | Copyright |
|---|---|---|
| [React](https://react.dev) and React DOM 18 | MIT | Copyright (c) Facebook, Inc. and its affiliates |
| [React Router](https://reactrouter.com) 7 | MIT | Copyright (c) React Training LLC 2015-2019, Remix Software Inc. 2020-2021 |
| [oidc-client-ts](https://github.com/authts/oidc-client-ts) | Apache-2.0 | Copyright authors of oidc-client-ts |
| Bricolage Grotesque (font) | SIL OFL 1.1 | Copyright 2022 The Bricolage Grotesque Project Authors (https://github.com/ateliertriay/bricolage) |
| IBM Plex Sans (font) | SIL OFL 1.1 | Copyright 2019 IBM Corp. |
| IBM Plex Mono (font) | SIL OFL 1.1 | Copyright 2017 IBM Corp. |

The fonts are shipped unmodified, as web fonts packaged by [Fontsource](https://fontsource.org). The licence texts are in
[third_party/fonts](third_party/fonts). The OFL permits bundling and redistribution, with the notice, and does not
cover the rest of Plinth, which stays under Apache-2.0.

## Linked into the Go binary

Go modules under Apache-2.0 (the Kubernetes client libraries and their dependencies, go-oidc, and others), BSD-3-Clause
(`golang.org/x/...`, the Go standard library's licence, protobuf, uuid), MIT (pgx, testify, ...) and ISC. Each module's
own licence applies; `go list -m all` lists them and `make licenses` summarises them.

## Build and test tools

Vite, Vitest, TypeScript, Testing Library and their dependencies are used to build and test Plinth and are not part of the
product. One of them (`lightningcss`, used by the build) is MPL-2.0; it is not distributed with Plinth.
