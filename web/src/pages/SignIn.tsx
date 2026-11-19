import { useCallback, useState, type FormEvent } from 'react'
import { api } from '../api'
import { Banner, ConfirmButton, ErrorBanner, Field } from '../components'
import { useAction, useLoad } from '../hooks'
import { sentence } from '../lib/format'
import type { AuthSettings, OidcProvider } from '../types'

const blank = (): OidcProvider => ({
  id: '', name: '', display_name: '', issuer: '', client_id: '', scopes: 'openid profile email groups', groups_claim: 'groups',
  admin_group: '', trust_unverified_email: false, enabled: true, read_only: false,
})

function Editor({ existing, onDone, onCancel }: { existing?: OidcProvider; onDone: () => void; onCancel: () => void }) {
  const [f, setF] = useState<OidcProvider>(existing ?? blank())
  const [tried, setTried] = useState(false)
  const callback = `${window.location.origin}/callback`
  const set = <K extends keyof OidcProvider>(k: K, v: OidcProvider[K]) => setF((cur) => ({ ...cur, [k]: v }))

  const sendFn = useCallback(async () => {
    const body = {
      name: f.name.trim(), display_name: f.display_name.trim(), issuer: f.issuer.trim(), client_id: f.client_id.trim(),
      scopes: f.scopes.trim(), groups_claim: f.groups_claim.trim(), admin_group: f.admin_group.trim(),
      trust_unverified_email: f.trust_unverified_email, enabled: f.enabled,
    }
    if (existing) await api(`/oidc-providers/${existing.id}`, { method: 'PUT', body })
    else await api('/oidc-providers', { method: 'POST', body })
    onDone()
  }, [f, existing, onDone])
  const save = useAction(sendFn)

  const problems = {
    name: !f.name.trim() ? 'Enter a short name.' : !/^[a-z0-9][a-z0-9-]{0,30}$/.test(f.name.trim()) ? 'Use lowercase letters, numbers and dashes.' : undefined,
    display: !f.display_name.trim() ? 'Enter the name people will see on the sign-in page.' : undefined,
    issuer: !/^https?:\/\/[^/\s]+/.test(f.issuer.trim()) ? 'Enter the issuer address, for example https://login.example.com/realms/plinth.' : undefined,
    client: !f.client_id.trim() ? 'Enter the client id registered for Plinth.' : undefined,
  }
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setTried(true)
    if ((!existing && problems.name) || problems.display || problems.issuer || problems.client) return
    await save.run()
  }

  return (
    <form className="panel" onSubmit={submit} noValidate>
      <h2>{existing ? `Edit ${existing.display_name}` : 'Add an identity provider'}</h2>
      <p className="hint" style={{ marginBottom: 12 }}>
        Any OpenID Connect provider works (Keycloak, Microsoft Entra ID, Okta, Auth0, Google Workspace and others). At the provider, register Plinth as a
        <b> public client</b> (a single-page app using the authorization code flow with PKCE) with the redirect address <code>{callback}</code>. Plinth contacts the issuer when you save.
      </p>
      <ErrorBanner message={save.error} details={save.details} />
      <div className="row">
        <Field id="p-name" label="Short name" error={tried && !existing ? problems.name : undefined} hint={existing ? 'A name cannot be changed.' : 'Used internally, for example company-sso.'}>
          <input id="p-name" type="text" autoComplete="off" value={f.name} disabled={!!existing} onChange={(e) => set('name', e.target.value)} />
        </Field>
        <Field id="p-display" label="Shown on the sign-in page" error={tried ? problems.display : undefined}>
          <input id="p-display" type="text" autoComplete="off" value={f.display_name} onChange={(e) => set('display_name', e.target.value)} placeholder="Company SSO" />
        </Field>
      </div>
      <div className="row">
        <Field id="p-issuer" label="Issuer address" error={tried ? problems.issuer : undefined} hint="Must use https. Plinth looks for /.well-known/openid-configuration below it.">
          <input id="p-issuer" type="text" autoComplete="off" spellCheck={false} value={f.issuer} onChange={(e) => set('issuer', e.target.value)} placeholder="https://login.example.com/realms/plinth" />
        </Field>
        <Field id="p-client" label="Client id" error={tried ? problems.client : undefined}>
          <input id="p-client" type="text" autoComplete="off" spellCheck={false} value={f.client_id} onChange={(e) => set('client_id', e.target.value)} />
        </Field>
      </div>
      <div className="row">
        <Field id="p-admin" label="Administrator group" optional hint="Members of this group are administrators. Others are ordinary users.">
          <input id="p-admin" type="text" autoComplete="off" value={f.admin_group} onChange={(e) => set('admin_group', e.target.value)} placeholder="plinth-admins" />
        </Field>
        <Field id="p-groups" label="Groups claim" hint="The token claim that lists a person's groups.">
          <input id="p-groups" type="text" autoComplete="off" value={f.groups_claim} onChange={(e) => set('groups_claim', e.target.value)} />
        </Field>
        <Field id="p-scopes" label="Scopes" hint="Separated by spaces. Must include openid.">
          <input id="p-scopes" type="text" autoComplete="off" value={f.scopes} onChange={(e) => set('scopes', e.target.value)} />
        </Field>
      </div>
      <div className="check">
        <input id="p-trust" type="checkbox" checked={f.trust_unverified_email} onChange={(e) => set('trust_unverified_email', e.target.checked)} />
        <label htmlFor="p-trust">
          <strong>Trust e-mail addresses this provider did not verify</strong>
          <p className="hint">Only if people cannot choose an address they do not own (a company directory). Team invitations are claimed by e-mail address.</p>
        </label>
      </div>
      <div className="check">
        <input id="p-enabled" type="checkbox" checked={f.enabled} onChange={(e) => set('enabled', e.target.checked)} />
        <label htmlFor="p-enabled"><strong>Offer it on the sign-in page</strong></label>
      </div>
      <div className="actions">
        <button className="btn primary" disabled={save.busy}>{save.busy ? 'Checking the provider…' : 'Save provider'}</button>
        <button type="button" className="btn" onClick={onCancel}>Cancel</button>
      </div>
    </form>
  )
}

function ProviderRow({ p, onEdit, onChanged }: { p: OidcProvider; onEdit: () => void; onChanged: () => void }) {
  const delFn = useCallback(async () => {
    await api(`/oidc-providers/${p.id}`, { method: 'DELETE' })
    onChanged()
  }, [p.id, onChanged])
  const del = useAction(delFn)
  return (
    <tr>
      <td data-label="Provider">
        <span className="title">{p.display_name}</span>
        <div className="sub mono">{p.issuer}</div>
      </td>
      <td data-label="Client id" className="mono">{p.client_id}</td>
      <td data-label="Administrators">{p.admin_group ? <span className="mono">{p.admin_group}</span> : <span className="muted">nobody</span>}</td>
      <td data-label="Status">
        {p.enabled ? <span className="chip accent">offered</span> : <span className="chip">off</span>}
        {p.read_only && <div className="sub">set in the environment</div>}
      </td>
      <td data-label="Actions">
        {!p.read_only && (
          <div className="actions">
            <button className="btn sm" onClick={onEdit}>Edit</button>
            <ConfirmButton label="Delete" confirmLabel="Confirm delete" disabled={del.busy} onConfirm={() => void del.run()} />
          </div>
        )}
        {del.error && <p className="field-error" role="alert" style={{ marginTop: 6 }}>{sentence(del.error)}</p>}
      </td>
    </tr>
  )
}

export function SignInPage() {
  const providers = useLoad((s) => api<OidcProvider[]>('/oidc-providers', { signal: s }), [])
  const settings = useLoad((s) => api<AuthSettings>('/auth-settings', { signal: s }), [])
  const [editing, setEditing] = useState<string | 'new' | null>(null)
  const done = () => {
    setEditing(null)
    providers.reload()
  }
  const toggleFn = useCallback(
    async (on: boolean) => {
      await api('/auth-settings', { method: 'PUT', body: { local_login_enabled: on } })
      settings.reload()
    },
    [settings],
  )
  const toggle = useAction(toggleFn)
  const local = settings.data?.local_login_enabled ?? true
  const list = providers.data ?? []

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Sign-in</h1>
          <p className="sub">How people prove who they are: a password kept by Plinth, an identity provider such as your company's single sign-on, or both.</p>
        </div>
        {!editing && <button className="btn primary" onClick={() => setEditing('new')}>Add a provider</button>}
      </div>

      <div className="panel">
        <div className="switch-row">
          <div>
            <h2>Password sign-in</h2>
            <p className="hint">People with an account under <b>Users</b> sign in with a username and password. Turn it off once an identity provider works and an administrator has signed in with it.</p>
            {settings.data?.forced && <p className="hint">It is kept on by <code>PLINTH_FORCE_LOCAL_LOGIN</code> whatever you choose here.</p>}
          </div>
          <div className="check" style={{ margin: 0 }}>
            <input id="local-on" type="checkbox" role="switch" checked={local} disabled={toggle.busy || !settings.data} onChange={(e) => void toggle.run(e.target.checked)} />
            <label htmlFor="local-on"><strong>{local ? 'On' : 'Off'}</strong></label>
          </div>
        </div>
        {toggle.error && <p className="field-error" role="alert" style={{ marginTop: 6 }}>{sentence(toggle.error)}</p>}
      </div>

      {providers.error && <Banner title="Could not load the providers.">{' '}{providers.error}</Banner>}
      {editing && (
        <Editor key={editing} existing={editing === 'new' ? undefined : list.find((p) => p.id === editing)} onDone={done} onCancel={() => setEditing(null)} />
      )}
      {providers.data && list.length === 0 && !editing && (
        <div className="panel empty">
          <h2>No identity provider yet</h2>
          <p>Everyone signs in with a password. Add your company's single sign-on so people can use their usual account.</p>
        </div>
      )}
      {list.length > 0 && (
        <div className="panel flush">
          <table className="table">
            <thead>
              <tr><th scope="col">Provider</th><th scope="col">Client id</th><th scope="col">Administrators</th><th scope="col">Status</th><th scope="col">Actions</th></tr>
            </thead>
            <tbody>
              {list.map((p) => <ProviderRow key={p.id} p={p} onEdit={() => setEditing(p.id)} onChanged={providers.reload} />)}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
