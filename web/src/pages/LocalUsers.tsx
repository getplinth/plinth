import { useCallback, useState, type FormEvent } from 'react'
import { api } from '../api'
import { Banner, ConfirmButton, ErrorBanner, Field } from '../components'
import { useAction, useLoad } from '../hooks'
import { fullTime, relativeTime, sentence } from '../lib/format'
import { useMe } from '../session'
import type { LocalAccount, Role } from '../types'

/** A password shown once. Plinth keeps only a hash, so it cannot be shown again. */
function OneTimePassword({ username, password, onClose }: { username: string; password: string; onClose: () => void }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(password)
      setCopied(true)
    } catch {
      /* select-all on the text still works */
    }
  }
  return (
    <Banner kind="warn" title={`Password for ${username}`}>
      <p>Give this to them now. It is shown only once, and they must choose their own at first sign-in.</p>
      <div className="secret-box">
        <code>{password}</code>
        <button type="button" className="btn sm" onClick={() => void copy()}>{copied ? 'Copied' : 'Copy'}</button>
        <button type="button" className="btn sm quiet" onClick={onClose}>Done</button>
      </div>
    </Banner>
  )
}

function CreateForm({ onDone, onCancel }: { onDone: (a: LocalAccount) => void; onCancel: () => void }) {
  const [username, setUsername] = useState('')
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<Role>('user')
  const [password, setPassword] = useState('')
  const [tried, setTried] = useState(false)
  const sendFn = useCallback(async () => {
    const a = await api<LocalAccount>('/local-users', {
      method: 'POST',
      body: { username: username.trim(), email: email.trim() || undefined, role, password: password || undefined },
    })
    onDone(a)
  }, [username, email, role, password, onDone])
  const save = useAction(sendFn)
  const nameProblem = !username.trim() ? 'Enter a username.' : !/^[a-zA-Z0-9][a-zA-Z0-9._@-]{1,62}$/.test(username.trim()) ? 'Use 2 to 63 letters, digits, dots, dashes, underscores or @.' : undefined
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setTried(true)
    if (!nameProblem) await save.run()
  }
  return (
    <form className="panel" onSubmit={submit} noValidate>
      <h2>New user</h2>
      <ErrorBanner message={save.error} details={save.details} />
      <div className="row">
        <Field id="lu-name" label="Username" error={tried ? nameProblem : undefined}>
          <input id="lu-name" type="text" autoComplete="off" autoCapitalize="none" spellCheck={false} value={username} onChange={(e) => setUsername(e.target.value)} />
        </Field>
        <Field id="lu-email" label="E-mail address" optional hint="Team invitations for this address are accepted at once.">
          <input id="lu-email" type="text" inputMode="email" autoComplete="off" value={email} onChange={(e) => setEmail(e.target.value)} />
        </Field>
        <Field id="lu-role" label="Role">
          <select id="lu-role" value={role} onChange={(e) => setRole(e.target.value as Role)}>
            <option value="user">User</option>
            <option value="admin">Administrator</option>
          </select>
        </Field>
      </div>
      <Field id="lu-password" label="Password" optional hint="Leave empty and Plinth makes one. Either way they must change it at first sign-in.">
        <input id="lu-password" type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} />
      </Field>
      <div className="actions">
        <button className="btn primary" disabled={save.busy}>{save.busy ? 'Creating…' : 'Create user'}</button>
        <button type="button" className="btn" onClick={onCancel}>Cancel</button>
      </div>
    </form>
  )
}

function Row({ a, self, onChanged, onSecret }: { a: LocalAccount; self: boolean; onChanged: () => void; onSecret: (name: string, pw: string) => void }) {
  const patchFn = useCallback(
    async (body: Record<string, unknown>) => {
      await api(`/local-users/${a.id}`, { method: 'PUT', body })
      onChanged()
    },
    [a.id, onChanged],
  )
  const patch = useAction(patchFn)
  const resetFn = useCallback(async () => {
    const r = await api<{ temporary_password?: string }>(`/local-users/${a.id}/reset-password`, { method: 'POST' })
    if (r.temporary_password) onSecret(a.username, r.temporary_password)
    onChanged()
  }, [a.id, a.username, onChanged, onSecret])
  const reset = useAction(resetFn)
  const delFn = useCallback(async () => {
    await api(`/local-users/${a.id}`, { method: 'DELETE' })
    onChanged()
  }, [a.id, onChanged])
  const del = useAction(delFn)
  const problem = patch.error ?? reset.error ?? del.error

  return (
    <tr>
      <td data-label="User">
        <span className="title">{a.username}</span> {self && <span className="muted">(you)</span>}
        {a.email !== a.username && <div className="sub">{a.email}</div>}
      </td>
      <td data-label="Role">
        <select aria-label={`Role of ${a.username}`} value={a.role} disabled={self || patch.busy} onChange={(e) => void patch.run({ role: e.target.value })}>
          <option value="user">User</option>
          <option value="admin">Administrator</option>
        </select>
      </td>
      <td data-label="Status">
        {a.disabled ? <span className="chip">disabled</span> : a.locked_until ? <span className="chip" title={fullTime(a.locked_until)}>locked</span> : <span className="chip accent">active</span>}
        {a.must_change_password && <div className="sub">must choose a new password</div>}
        <div className="sub" title={a.last_login ? fullTime(a.last_login) : undefined}>{a.last_login ? `signed in ${relativeTime(a.last_login)}` : 'never signed in'}</div>
      </td>
      <td data-label="Actions">
        {!self && (
          <div className="actions">
            <button className="btn sm" disabled={patch.busy} onClick={() => void patch.run({ disabled: !a.disabled })}>{a.disabled ? 'Enable' : 'Disable'}</button>
            <ConfirmButton label="Reset password" confirmLabel="Confirm reset" disabled={reset.busy} onConfirm={() => void reset.run()} />
            <ConfirmButton label="Delete" confirmLabel="Confirm delete" disabled={del.busy} onConfirm={() => void del.run()} />
          </div>
        )}
        {problem && <p className="field-error" role="alert" style={{ marginTop: 6 }}>{sentence(problem)}</p>}
      </td>
    </tr>
  )
}

export function LocalUsersPage() {
  const me = useMe()
  const list = useLoad((s) => api<LocalAccount[]>('/local-users', { signal: s }), [])
  const [adding, setAdding] = useState(false)
  const [secret, setSecret] = useState<{ username: string; password: string }>()
  const showSecret = useCallback((username: string, password: string) => setSecret({ username, password }), [])
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Users</h1>
          <p className="sub">People who sign in to Plinth with a password. People who sign in through an identity provider are not listed here: they appear when they first sign in.</p>
        </div>
        {!adding && <button className="btn primary" onClick={() => setAdding(true)}>New user</button>}
      </div>
      {list.error && <Banner title="Could not load the users.">{' '}{list.error}</Banner>}
      {secret && <OneTimePassword username={secret.username} password={secret.password} onClose={() => setSecret(undefined)} />}
      {adding && (
        <CreateForm
          onCancel={() => setAdding(false)}
          onDone={(a) => {
            setAdding(false)
            if (a.temporary_password) setSecret({ username: a.username, password: a.temporary_password })
            list.reload()
          }}
        />
      )}
      {list.data && (
        <div className="panel flush">
          <table className="table">
            <thead>
              <tr><th scope="col">User</th><th scope="col">Role</th><th scope="col">Status</th><th scope="col">Actions</th></tr>
            </thead>
            <tbody>
              {list.data.map((a) => <Row key={a.id} a={a} self={a.id === me.id} onChanged={list.reload} onSecret={showSecret} />)}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
