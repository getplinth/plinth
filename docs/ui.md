# Web interface

A single-page app (React, TypeScript, Vite) compiled into the Plinth binary, so there is nothing else to deploy: the same server answers `/api/v1/...` and serves the app for every other path.

## Try it

```bash
make demo        # builds the UI, starts PostgreSQL in Docker, runs Plinth with simulated systems
```

Open <http://127.0.0.1:8080> and sign in as **Alice** or **Bob** (developers) or **Root** (administrator). The demo uses a stand-in identity provider with no passwords, simulated Git/registry/cluster/database drivers, and binds only to `127.0.0.1`. It talks to no real system. A team name containing `fail` fails once per project so the Retry flow can be tried.

For UI development run `make demo` in one terminal and `cd web && npm run dev` in another (the dev server proxies `/api` to the demo).

## What people can do

| Page | Who | Purpose |
|---|---|---|
| Requests | everyone | your requests with progress and status; admins can switch to everyone's |
| Request detail | owner, admins | every step as a course of stone, live while it runs; the reason for a failure; **Retry failed steps** |
| New request | everyone | choose a blueprint, name the team, add projects; shows the exact names that will be created before you submit |
| Integrations | admins | add, check, replace and delete connections to Git, registry, clusters and databases |
| Blueprints | admins | combine integrations into what developers can request |
| Audit log | admins | who changed what |

## Sign-in

OpenID Connect authorization code flow with PKCE, performed in the browser by `oidc-client-ts`. The identity provider's address and client id come from the server (`GET /api/v1/config`). The ID token is sent to the API as a bearer token and kept in `sessionStorage`, so it disappears with the tab. When the API answers 401 the app signs out and sends the user through login again, returning to the page they were on (only same-site paths are accepted as the return address).

Your identity provider must allow the app as a **public client** with redirect URI `https://<your plinth host>/callback`, PKCE (S256), and CORS for the discovery and token endpoints from that origin. Add the `groups` claim to the ID token if you use group-based administrators.

## Security

- **Strict content security policy** on the app: scripts, styles and fonts only from this origin, no inline script or style, no framing, `connect-src` limited to this origin and the identity provider. The API keeps its own, stricter policy.
- Fonts are self-hosted (bundled), and there are no third-party scripts or analytics.
- Dependencies are audited (`npm audit` reports none); the router was upgraded to fix an open-redirect advisory.
- Stored integration settings are never shown, even to admins: "Replace settings" asks for the complete set again.
- The UI hides what a user cannot do, but the server enforces everything: hiding is a convenience, not a control.

## Design

Plinth is the base a statue stands on, so work is shown as **courses of stone**: each step is a slab, filled as it completes, with a shimmer on the one in progress, a dashed outline for work still waiting, and a red edge where it stopped. The same idea is the logo and the progress glyph in lists. Neutrals are cool limestone greys with deep ink; the single accent is verdigris, the green of aged bronze. Type is Bricolage Grotesque for titles and IBM Plex for text and data. Light and dark follow the system setting, motion respects *reduce motion*, every control is keyboard reachable with a visible focus ring, and tables collapse into stacked cards on phones.

## Building and testing

| Command | What it does |
|---|---|
| `make web` | install and build the app into `web/dist`, which the next `go build` embeds |
| `make test` | Go unit tests, then the web unit and component tests |
| `cd web && npm run typecheck` | TypeScript checks |

Without a build the server still starts and shows a short notice at `/`. The pure logic (naming rules shared with the server, step ordering, form validation, safe redirects) and the step view are unit tested; the naming rules use the same test cases as the Go implementation so the form cannot drift from the server.
