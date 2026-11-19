import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { getOptions, loginLocal, loginWithProvider, returnPath } from '../auth'
import { Banner, Field, Mark } from '../components'
import { sentence } from '../lib/format'
import type { SignInOptions } from '../types'

export function LoginPage() {
  const navigate = useNavigate()
  const [options, setOptions] = useState<SignInOptions>()
  const [loadError, setLoadError] = useState<string>()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const redirected = useRef(false)

  useEffect(() => {
    getOptions().then(setOptions, (e: unknown) => setLoadError(e instanceof Error ? e.message : 'Could not load the sign-in settings.'))
  }, [])

  // One identity provider and no password sign-in: nothing to choose.
  useEffect(() => {
    if (!options || redirected.current || options.local_login || options.providers.length !== 1) return
    redirected.current = true
    void loginWithProvider(options.providers[0]!).catch((e: unknown) => setError(e instanceof Error ? e.message : 'Could not start the sign-in.'))
  }, [options])

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (!username.trim() || !password) return
    setBusy(true)
    setError(undefined)
    try {
      await loginLocal(username.trim(), password)
      navigate(returnPath(), { replace: true })
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not sign in.')
      setPassword('')
    } finally {
      setBusy(false)
    }
  }

  const nobody = options && !options.local_login && options.providers.length === 0
  return (
    <div className="login-card">
      <Mark size={44} />
      <h1>Sign in to Plinth</h1>
      {loadError && <Banner title="Can't sign in.">{' '}{loadError}</Banner>}
      {!options && !loadError && <p className="muted" role="status">Loading…</p>}
      {error && <Banner title={sentence(error)} />}
      {nobody && (
        <Banner kind="warn" title="Nobody can sign in yet.">
          {' '}Whoever runs Plinth can create the first administrator with <code>plinth bootstrap-admin</code>, or set <code>PLINTH_BOOTSTRAP_ADMIN_PASSWORD</code> and restart.
        </Banner>
      )}
      {options && options.providers.length > 0 && (
        <div className="provider-list">
          {options.providers.map((p) => (
            <button key={p.id} className="btn primary" onClick={() => void loginWithProvider(p).catch((e: unknown) => setError(e instanceof Error ? e.message : 'Could not start the sign-in.'))}>
              Continue with {p.display_name}
            </button>
          ))}
        </div>
      )}
      {options && options.local_login && options.providers.length > 0 && <p className="login-or" aria-hidden="true"><span>or with a password</span></p>}
      {options?.local_login && (
        <form onSubmit={submit} noValidate>
          <Field id="login-username" label="Username">
            <input id="login-username" type="text" autoComplete="username" autoCapitalize="none" spellCheck={false} value={username} onChange={(e) => setUsername(e.target.value)} />
          </Field>
          <Field id="login-password" label="Password">
            <input id="login-password" type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
          </Field>
          <button className="btn primary" disabled={busy || !username.trim() || !password}>{busy ? 'Signing in…' : 'Sign in'}</button>
        </form>
      )}
    </div>
  )
}
