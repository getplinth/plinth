import { UserManager, WebStorageStateStore } from 'oidc-client-ts'

// Sign-in is the OpenID Connect authorization code flow with PKCE, done in the
// browser by oidc-client-ts. The identity provider's address comes from the
// server (GET /api/v1/config). The ID token is sent to the API as a bearer
// token and kept in sessionStorage, so it disappears with the tab. A strict
// content security policy (no inline or third-party scripts) is the main
// defence against it being read by injected code.

let manager: Promise<UserManager> | null = null

async function getManager(): Promise<UserManager> {
  manager ??= (async () => {
    const res = await fetch('/api/v1/config')
    if (!res.ok) throw new Error('Could not load the sign-in settings from the server.')
    const cfg = (await res.json()) as { issuer: string; client_id: string }
    if (!cfg.issuer || !cfg.client_id) throw new Error('The server has no sign-in provider configured.')
    return new UserManager({
      authority: cfg.issuer,
      client_id: cfg.client_id,
      redirect_uri: `${window.location.origin}/callback`,
      response_type: 'code',
      scope: 'openid profile email groups',
      userStore: new WebStorageStateStore({ store: window.sessionStorage }),
      automaticSilentRenew: false,
      loadUserInfo: false,
    })
  })()
  return manager
}

/** The current ID token, or null when signed out or expired. */
export async function getToken(): Promise<string | null> {
  const m = await getManager()
  const user = await m.getUser()
  if (!user || user.expired || !user.id_token) return null
  return user.id_token
}

/** Send the browser to the identity provider; comes back to /callback. */
export async function startLogin(returnTo: string): Promise<void> {
  const m = await getManager()
  await m.signinRedirect({ state: returnTo })
}

/**
 * Where to go after login. Only same-site absolute paths are accepted: not
 * "//host", "/\\host" or anything with a scheme, which browsers can treat as
 * another site.
 */
export function safeReturnPath(state: unknown): string {
  if (typeof state !== 'string') return '/'
  if (!/^\/(?![/\\])/.test(state) || /[\\\u0000-\u001f]/.test(state)) return '/'
  return state
}

let callback: Promise<string> | null = null

/** Finish the sign-in on /callback. Safe to call twice (React strict mode). */
export function finishLogin(): Promise<string> {
  callback ??= (async () => {
    const m = await getManager()
    const user = await m.signinRedirectCallback()
    return safeReturnPath(user.state)
  })()
  return callback
}

export async function signOut(): Promise<void> {
  const m = await getManager()
  await m.removeUser()
}
