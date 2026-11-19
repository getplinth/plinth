import { useCallback, useState, type FormEvent } from 'react'
import { api } from '../api'
import { Banner, ConfirmButton, ErrorBanner, Field } from '../components'
import { useAction, useLoad } from '../hooks'
import { sentence } from '../lib/format'
import type { QuotaTier, Resources } from '../types'

const blank = (): QuotaTier => ({
  pods: 10,
  requests: { cpu: '', memory: '' },
  limits: { cpu: '', memory: '' },
  limitRange: { min: { cpu: '', memory: '' }, max: { cpu: '', memory: '' }, default: { cpu: '', memory: '' }, defaultRequest: { cpu: '', memory: '' } },
})

function Pair({ id, label, hint, value, onChange }: { id: string; label: string; hint?: string; value: Resources; onChange: (r: Resources) => void }) {
  return (
    <div className="row">
      <Field id={`${id}-cpu`} label={`${label}: CPU`} hint={hint}>
        <input id={`${id}-cpu`} type="text" autoComplete="off" value={value.cpu} placeholder="500m" onChange={(e) => onChange({ ...value, cpu: e.target.value })} />
      </Field>
      <Field id={`${id}-mem`} label={`${label}: memory`}>
        <input id={`${id}-mem`} type="text" autoComplete="off" value={value.memory} placeholder="512Mi" onChange={(e) => onChange({ ...value, memory: e.target.value })} />
      </Field>
    </div>
  )
}

function Editor({ name: existing, initial, onDone, onCancel }: { name?: string; initial?: QuotaTier; onDone: () => void; onCancel: () => void }) {
  const [name, setName] = useState(existing ?? '')
  const [t, setT] = useState<QuotaTier>(initial ?? blank())
  const [tried, setTried] = useState(false)
  const lr = t.limitRange
  const sendFn = useCallback(async () => {
    await api(`/quota-tiers/${name.trim()}`, { method: 'PUT', body: { ...t, pods: Number(t.pods) } })
    onDone()
  }, [name, t, onDone])
  const save = useAction(sendFn)

  const nameProblem = !name.trim() ? 'Enter a name.' : !/^[a-z0-9][a-z0-9-]{0,30}$/.test(name.trim()) ? 'Use lowercase letters, numbers and dashes, up to 31 characters.' : undefined
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setTried(true)
    if (!nameProblem) await save.run()
  }
  return (
    <form className="panel" onSubmit={submit} noValidate>
      <h2>{existing ? `Edit ${existing}` : 'New quota tier'}</h2>
      <p className="hint" style={{ marginBottom: 12 }}>
        A tier applies to projects requested from now on. Namespaces that already exist keep what they have until a request for them runs again.
      </p>
      <ErrorBanner message={save.error} details={save.details} />
      <div className="row">
        <Field id="tier-name" label="Name" error={tried ? nameProblem : undefined} hint={existing ? 'A tier cannot be renamed.' : 'What developers choose as the size of a project.'}>
          <input id="tier-name" type="text" autoComplete="off" value={name} disabled={!!existing} onChange={(e) => setName(e.target.value)} placeholder="platinum" />
        </Field>
        <Field id="tier-pods" label="Pods" hint="Most pods in a namespace.">
          <input id="tier-pods" type="number" min={1} value={t.pods} onChange={(e) => setT({ ...t, pods: e.target.value === '' ? 0 : Number(e.target.value) })} />
        </Field>
      </div>
      <h3>Whole namespace</h3>
      <Pair id="tier-req" label="Requests" hint="Quantities such as 500m, 2 or 512Mi." value={t.requests} onChange={(requests) => setT({ ...t, requests })} />
      <Pair id="tier-lim" label="Limits" value={t.limits} onChange={(limits) => setT({ ...t, limits })} />
      <h3>Each container</h3>
      <Pair id="tier-min" label="Smallest allowed" value={lr.min} onChange={(min) => setT({ ...t, limitRange: { ...lr, min } })} />
      <Pair id="tier-max" label="Largest allowed" value={lr.max} onChange={(max) => setT({ ...t, limitRange: { ...lr, max } })} />
      <Pair id="tier-dreq" label="Default request" value={lr.defaultRequest} onChange={(defaultRequest) => setT({ ...t, limitRange: { ...lr, defaultRequest } })} />
      <Pair id="tier-def" label="Default limit" value={lr.default} onChange={(d) => setT({ ...t, limitRange: { ...lr, default: d } })} />
      <div className="actions">
        <button className="btn primary" disabled={save.busy}>{save.busy ? 'Saving…' : 'Save tier'}</button>
        <button type="button" className="btn" onClick={onCancel}>Cancel</button>
      </div>
    </form>
  )
}

function Row({ name, t, onEdit, onChanged }: { name: string; t: QuotaTier; onEdit: () => void; onChanged: () => void }) {
  const delFn = useCallback(async () => {
    await api(`/quota-tiers/${name}`, { method: 'DELETE' })
    onChanged()
  }, [name, onChanged])
  const del = useAction(delFn)
  return (
    <tr>
      <td data-label="Tier"><span className="title">{name}</span>{name === 'bronze' && <div className="sub">default</div>}</td>
      <td data-label="Pods">{t.pods}</td>
      <td data-label="Requests">{t.requests.cpu} CPU, {t.requests.memory}</td>
      <td data-label="Limits">{t.limits.cpu} CPU, {t.limits.memory}</td>
      <td data-label="Per container">up to {t.limitRange.max.cpu} CPU, {t.limitRange.max.memory}</td>
      <td data-label="Actions">
        <button className="btn sm" onClick={onEdit}>Edit</button>{' '}
        {name !== 'bronze' && <ConfirmButton label="Delete" confirmLabel="Confirm delete" disabled={del.busy} onConfirm={() => void del.run()} />}
        {del.error && <p className="field-error" role="alert" style={{ marginTop: 6 }}>{sentence(del.error)}</p>}
      </td>
    </tr>
  )
}

export function TiersPage() {
  const tiers = useLoad((s) => api<Record<string, QuotaTier>>('/quota-tiers', { signal: s }), [])
  const [editing, setEditing] = useState<string | 'new' | null>(null)
  const done = () => {
    setEditing(null)
    tiers.reload()
  }
  const names = Object.keys(tiers.data ?? {}).sort()
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Quota tiers</h1>
          <p className="sub">The sizes developers choose for a project: how much a namespace may use, and what each container gets by default.</p>
        </div>
        {!editing && <button className="btn primary" onClick={() => setEditing('new')}>New tier</button>}
      </div>
      {tiers.error && <Banner title="Could not load the tiers.">{' '}{tiers.error}</Banner>}
      {editing && (
        <Editor
          key={editing}
          name={editing === 'new' ? undefined : editing}
          initial={editing === 'new' ? undefined : tiers.data?.[editing]}
          onDone={done}
          onCancel={() => setEditing(null)}
        />
      )}
      {tiers.data && (
        <div className="panel flush">
          <table className="table">
            <thead>
              <tr><th scope="col">Tier</th><th scope="col">Pods</th><th scope="col">Requests</th><th scope="col">Limits</th><th scope="col">Per container</th><th scope="col">Actions</th></tr>
            </thead>
            <tbody>
              {names.map((n) => <Row key={n} name={n} t={tiers.data![n]!} onEdit={() => setEditing(n)} onChanged={tiers.reload} />)}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
