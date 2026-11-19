# Web interface

A single-page app (React, TypeScript, Vite) compiled into the Plinth binary, so there is nothing else to deploy: the same server answers `/api/v1/...` and serves the app for every other path.

## Try it

```bash
make demo        # builds the UI, starts PostgreSQL in Docker, runs Plinth with simulated systems
```

Open <http://127.0.0.1:8080> and sign in as **Alice** or **Bob** (developers) or **Root** or **Ada** (administrators). The demo uses a stand-in identity provider with no passwords, simulated Git/registry/cluster/database drivers, and binds only to `127.0.0.1`. It talks to no real system. A team name containing `fail` fails once per project so the Retry flow can be tried.

Things to try:
- **Approval.** The `regulated` blueprint needs approval: ask for a project as Alice, then approve it as Root. Root cannot approve Root's own request; Ada can.
- **Teams.** As Alice, request a project for a new team, add `bob@example.test` on the team page, then sign in as Bob and open Requests: the team's requests are there, and Bob can request for the team too. Add `newcomer@example.test` instead and the page says they are invited; they join when they first sign in with that address, and an owner can revoke the invitation before then.
- **Notifications.** `make demo` also starts Mailpit. Open <http://127.0.0.1:8025> while you try the two flows above: administrators get *Approval needed*, the requester gets *Approved* or *Rejected* and then *Ready*, and the invited address gets its invitation. As Root, the Notifications page lists what was sent. (Sign each person in once first: administrators are only e-mailed after they have signed in.)

For UI development run `make demo` in one terminal and `cd web && npm run dev` in another (the dev server proxies `/api` to the demo).

## What people can do

| Page | Who | Purpose |
|---|---|---|
| Requests | everyone | your requests and your teams' with progress and status; filter by team; admins can switch to everyone's |
| Teams | everyone | the teams you belong to (administrators can show every team); a team page lists its people and projects, and lets owners add people by e-mail, change roles, remove people, with **Leave team** for yourself |
| Request detail | owner, admins | every step as a course of stone, live while it runs; the reason for a failure; **Retry failed steps** |
| New request | everyone | choose a blueprint (the form says when one needs approval), name the team, add projects; shows the exact names that will be created before you submit |
| Approvals | admins | requests waiting for a decision, what each would create, with Approve and Reject (a reason is required to reject); the navigation shows how many wait for you |
| Integrations | admins | add, check, replace and delete connections to Git, registry, clusters and databases |
| Blueprints | admins | combine integrations into what developers can request |
| Notifications | admins | which channels are on, a **Send a test message** button, and the latest e-mails and webhook calls with their outcome and reason |
| Quota tiers | admins | the sizes developers choose: namespace totals and per-container defaults |
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
