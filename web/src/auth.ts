import { UserManager, WebStorageStateStore } from 'oidc-client-ts'
import type { SignInOptions, SignInProvider } from './types'

// People sign in with a password (a Plinth session token) or through an
// identity provider (OpenID Connect, authorization code with PKCE, done in the
// browser by oidc-client-ts). Which providers exist comes from the server
// (GET /api/v1/config) and can change, so they are looked up, not built in.
// Either way the API receives a bearer token, kept in sessionStorage so it
// disappears with the tab. A strict content security policy (no inline or
// third-party scripts) is the main defence against it being read by injected code.

const LOCAL_KEY = 'plinth.local' // {token, expires_at}
const PROVIDER_KEY = 'plinth.provider' // id of the provider used to sign in
const RETURN_KEY = 'plinth.return' // where to go after signing in

function read(key: string): string | null {
  try {
    return window.sessionStorage.getItem(key)
  } catch {
    return null
  }
}
function write(key: string, value: string | null) {
  try {
    if (value === null) window.sessionStorage.removeItem(key)
    else window.sessionStorage.setItem(key, value)
  } catch {
    /* storage unavailable: sign-in then lasts until the page reloads */
  }
}

/** What the sign-in page offers. */
export async function getOptions(): Promise<SignInOptions> {
  const res = await fetch('/api/v1/config')
  if (!res.ok) throw new Error('Could not load the sign-in settings from the server.')
  return (await res.json()) as SignInOptions
}

const managers = new Map<string, UserManager>()

function managerFor(p: SignInProvider): UserManager {
  const key = `${p.id}|${p.issuer}|${p.client_id}|${p.scopes}`
  let m = managers.get(key)
  if (!m) {
    m = new UserManager({
      authority: p.issuer,
      client_id: p.client_id,
      redirect_uri: `${window.location.origin}/callback`,
      response_type: 'code',
      scope: p.scopes || 'openid profile email groups',
      userStore: new WebStorageStateStore({ store: window.sessionStorage }),
      automaticSilentRenew: false,
      loadUserInfo: false,
    })
    managers.set(key, m)
  }
  return m
}

async function providerById(id: string): Promise<SignInProvider> {
  const p = (await getOptions()).providers.find((x) => x.id === id)
  if (!p) throw new Error('That sign-in provider is no longer available. Sign in again.')
  return p
}

/** The current bearer token, or null when signed out or expired. */
export async function getToken(): Promise<string | null> {
  const local = read(LOCAL_KEY)
  if (local) {
    try {
      const s = JSON.parse(local) as { token: string; expires_at: string }
      if (s.token && Date.parse(s.expires_at) > Date.now()) return s.token
    } catch {
      /* fall through */
    }
    write(LOCAL_KEY, null)
    return null
  }
  const id = read(PROVIDER_KEY)
  if (!id) return null
  try {
    const user = await managerFor(await providerById(id)).getUser()
    return user && !user.expired && user.id_token ? user.id_token : null
  } catch {
    return null
  }
}

/** Show the sign-in page, remembering where to come back to. */
export async function startLogin(returnTo: string): Promise<void> {
  write(RETURN_KEY, returnTo)
  window.location.assign('/login')
}

/** Where to go after signing in. */
export function returnPath(): string {
  return safeReturnPath(read(RETURN_KEY))
}

/** Sign in with a username and password. Resolves to whether the password must be changed now. */
export async function loginLocal(username: string, password: string): Promise<{ mustChange: boolean }> {
  const res = await fetch('/api/v1/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  })
  const body = (await res.json().catch(() => ({}))) as { token?: string; expires_at?: string; must_change_password?: boolean; error?: string }
  if (!res.ok || !body.token || !body.expires_at) throw new Error(body.error ?? 'Could not sign in.')
  write(PROVIDER_KEY, null)
  write(LOCAL_KEY, JSON.stringify({ token: body.token, expires_at: body.expires_at }))
  return { mustChange: !!body.must_change_password }
}

/** Send the browser to an identity provider; it comes back to /callback. */
export async function loginWithProvider(p: SignInProvider): Promise<void> {
  write(LOCAL_KEY, null)
  write(PROVIDER_KEY, p.id)
  await managerFor(p).signinRedirect({ state: read(RETURN_KEY) ?? '/' })
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
    const id = read(PROVIDER_KEY)
    if (!id) throw new Error('The sign-in was not started from this page. Try again.')
    const user = await managerFor(await providerById(id)).signinRedirectCallback()
    return safeReturnPath(user.state)
  })()
  return callback
}

/** End the session: on the server for password sessions, locally for both. */
export async function signOut(): Promise<void> {
  const local = read(LOCAL_KEY)
  if (local) {
    try {
      const { token } = JSON.parse(local) as { token: string }
      await fetch('/api/v1/auth/logout', { method: 'POST', headers: { Authorization: `Bearer ${token}` } })
    } catch {
      /* the session ends on its own; the token is dropped below either way */
    }
    write(LOCAL_KEY, null)
  }
  const id = read(PROVIDER_KEY)
  if (id) {
    try {
      await managerFor(await providerById(id)).removeUser()
    } catch {
      /* provider gone: nothing more to clear */
    }
    write(PROVIDER_KEY, null)
  }
}
