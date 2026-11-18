import { useCallback, useMemo, useState, type FormEvent } from 'react'
import { api } from '../api'
import { Banner, ConfirmButton, ErrorBanner, Field } from '../components'
import { useAction, useLoad } from '../hooks'
import { driverTitle, fieldsFor, kindLabel, kindOrder, looksSecret } from '../lib/drivers'
import { fullTime, relativeTime } from '../lib/format'
import type { DriverInfo, Integration, IntegrationKind } from '../types'

interface Extra {
  key: string
  value: string
}

/** Settings for an integration: the known fields of its driver plus free key/value rows. */
function Settings({
  driver,
  values,
  setValues,
  extras,
  setExtras,
  missing,
}: {
  driver: string
  values: Record<string, string>
  setValues: (v: Record<string, string>) => void
  extras: Extra[]
  setExtras: (e: Extra[]) => void
  missing: string[]
}) {
  const fields = fieldsFor(driver)
  return (
    <>
      {fields.map((f) => (
        <Field
          key={f.key}
          id={`cfg-${f.key}`}
          label={f.label}
          optional={!f.required}
          hint={f.help}
          error={missing.includes(f.key) ? `${f.label} is required.` : undefined}
        >
          {f.multiline ? (
            <textarea id={`cfg-${f.key}`} spellCheck={false} value={values[f.key] ?? ''} onChange={(e) => setValues({ ...values, [f.key]: e.target.value })} placeholder={f.placeholder} />
          ) : (
            <input
              id={`cfg-${f.key}`}
              type={f.secret ? 'password' : 'text'}
              autoComplete={f.secret ? 'new-password' : 'off'}
              spellCheck={false}
              value={values[f.key] ?? ''}
              onChange={(e) => setValues({ ...values, [f.key]: e.target.value })}
              placeholder={f.placeholder}
            />
          )}
        </Field>
      ))}
      <div className="field">
        <span className="label">{fields.length ? 'Other settings' : 'Settings'} <span className="opt">(optional)</span></span>
        <div className="kvrows">
          {extras.map((x, i) => (
            <div className="kvrow" key={i}>
              <input type="text" aria-label="Setting name" placeholder="name" value={x.key} onChange={(e) => setExtras(extras.map((y, j) => (j === i ? { ...y, key: e.target.value } : y)))} />
              <input
                type={looksSecret(x.key) ? 'password' : 'text'}
                autoComplete="off"
                aria-label="Setting value"
                placeholder="value"
                value={x.value}
                onChange={(e) => setExtras(extras.map((y, j) => (j === i ? { ...y, value: e.target.value } : y)))}
              />
              <button type="button" className="btn sm quiet" onClick={() => setExtras(extras.filter((_, j) => j !== i))}>Remove</button>
            </div>
          ))}
        </div>
        <div>
          <button type="button" className="btn sm" onClick={() => setExtras([...extras, { key: '', value: '' }])}>Add a setting</button>
        </div>
      </div>
    </>
  )
}

function buildConfig(driver: string, values: Record<string, string>, extras: Extra[]) {
  const cfg: Record<string, string> = {}
  for (const [k, v] of Object.entries(values)) if (v.trim() !== '') cfg[k] = v.trim()
  for (const x of extras) if (x.key.trim() && x.value !== '') cfg[x.key.trim()] = x.value
  const missing = fieldsFor(driver).filter((f) => f.required && !cfg[f.key]).map((f) => f.key)
  return { cfg, missing }
}

function CreateForm({ drivers, onDone, onCancel }: { drivers: DriverInfo[]; onDone: () => void; onCancel: () => void }) {
  const [kind, setKind] = useState<IntegrationKind>('gitsource')
  const [driver, setDriver] = useState('')
  const [name, setName] = useState('')
  const [values, setValues] = useState<Record<string, string>>({})
  const [extras, setExtras] = useState<Extra[]>([])
  const [tried, setTried] = useState(false)
  const options = drivers.filter((d) => d.kind === kind)
  const { cfg, missing } = buildConfig(driver, values, extras)

  const send = useCallback(async () => {
    await api('/integrations', { method: 'POST', body: { kind, driver, name: name.trim(), config: cfg } })
    onDone()
  }, [kind, driver, name, cfg, onDone])
  const create = useAction(send)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setTried(true)
    if (!driver || !name.trim() || missing.length > 0) return
    await create.run()
  }

  return (
    <form className="panel" onSubmit={submit} noValidate>
      <h2>Add an integration</h2>
      <p className="hint" style={{ marginBottom: 14 }}>
        Settings are encrypted before they are stored and never shown again. Plinth only uses them to set up projects.
      </p>
      <ErrorBanner message={create.error} details={create.details} />
      <div className="row">
        <Field id="kind" label="What kind of system?">
          <select id="kind" value={kind} onChange={(e) => { setKind(e.target.value as IntegrationKind); setDriver(''); setValues({}) }}>
            {kindOrder.map((k) => (
              <option key={k} value={k}>{kindLabel[k]}</option>
            ))}
          </select>
        </Field>
        <Field id="driver" label="Which product?" error={tried && !driver ? 'Choose a product.' : undefined}>
          <select id="driver" value={driver} onChange={(e) => { setDriver(e.target.value); setValues({}) }}>
            <option value="">Choose…</option>
            {options.map((d) => (
              <option key={d.driver} value={d.driver}>{driverTitle(d.driver)}</option>
            ))}
          </select>
        </Field>
        <Field id="int-name" label="Name" hint="Blueprints refer to it by this name. Lowercase letters, numbers and dashes." error={tried && !name.trim() ? 'Enter a name.' : undefined}>
          <input id="int-name" type="text" autoComplete="off" value={name} onChange={(e) => setName(e.target.value)} placeholder="git-main" />
        </Field>
      </div>
      {driver && <Settings driver={driver} values={values} setValues={setValues} extras={extras} setExtras={setExtras} missing={tried ? missing : []} />}
      <div className="actions">
        <button className="btn primary" disabled={create.busy}>{create.busy ? 'Adding…' : 'Add integration'}</button>
        <button type="button" className="btn quiet" onClick={onCancel}>Cancel</button>
      </div>
    </form>
  )
}

function ReplaceForm({ item, onDone, onCancel }: { item: Integration; onDone: () => void; onCancel: () => void }) {
  const [values, setValues] = useState<Record<string, string>>({})
  const [extras, setExtras] = useState<Extra[]>([])
  const [tried, setTried] = useState(false)
  const { cfg, missing } = buildConfig(item.driver, values, extras)
  const send = useCallback(async () => {
    await api(`/integrations/${item.name}`, { method: 'PUT', body: { config: cfg } })
    onDone()
  }, [item.name, cfg, onDone])
  const save = useAction(send)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setTried(true)
    if (Object.keys(cfg).length === 0 || missing.length > 0) return
    await save.run()
  }
  return (
    <form className="panel" onSubmit={submit} noValidate>
      <h2>Replace settings for {item.name}</h2>
      <Banner kind="info" title="Stored values are never shown.">
        {' '}Enter the complete set of settings again. They replace what is stored. Currently set: {item.config_keys.map((k) => <code key={k}>{k} </code>)}
      </Banner>
      <ErrorBanner message={save.error} details={save.details} />
      <Settings driver={item.driver} values={values} setValues={setValues} extras={extras} setExtras={setExtras} missing={tried ? missing : []} />
      <div className="actions">
        <button className="btn primary" disabled={save.busy}>{save.busy ? 'Saving…' : 'Replace settings'}</button>
        <button type="button" className="btn quiet" onClick={onCancel}>Cancel</button>
      </div>
    </form>
  )
}

function Row({ item, onChanged }: { item: Integration; onChanged: () => void }) {
  const [result, setResult] = useState<{ ok: boolean; error?: string }>()
  const [editing, setEditing] = useState(false)
  const checkFn = useCallback(async () => setResult(await api<{ ok: boolean; error?: string }>(`/integrations/${item.name}/validate`, { method: 'POST' })), [item.name])
  const check = useAction(checkFn)
  const delFn = useCallback(async () => {
    await api(`/integrations/${item.name}`, { method: 'DELETE' })
    onChanged()
  }, [item.name, onChanged])
  const del = useAction(delFn)

  return (
    <>
      <tr>
        <td data-label="Name">
          <span className="title">{item.name}</span>
          <div className="sub">{item.config_keys.length} settings stored</div>
        </td>
        <td data-label="Product"><span className="chip">{driverTitle(item.driver)}</span></td>
        <td data-label="Changed" title={fullTime(item.updated_at)}>{relativeTime(item.updated_at)}</td>
        <td data-label="Actions">
          <div className="actions">
            <button className="btn sm" disabled={check.busy} onClick={() => void check.run()}>{check.busy ? 'Checking…' : 'Check connection'}</button>
            <button className="btn sm" onClick={() => setEditing(true)}>Replace settings</button>
            <ConfirmButton label="Delete" confirmLabel="Confirm delete" disabled={del.busy} onConfirm={() => void del.run()} />
          </div>
          {result && (
            <p className={result.ok ? 'hint' : 'field-error'} role="status" style={{ marginTop: 6 }}>
              {result.ok ? 'Connected. The system accepted these settings.' : `Not connected: ${result.error}`}
            </p>
          )}
          {check.error && <p className="field-error">{check.error}</p>}
          {del.error && <p className="field-error">{del.error === 'still referenced by other objects' ? 'A blueprint still uses this integration. Remove it from the blueprint first.' : del.error}</p>}
        </td>
      </tr>
      {editing && (
        <tr>
          <td colSpan={4}>
            <ReplaceForm item={item} onCancel={() => setEditing(false)} onDone={() => { setEditing(false); onChanged() }} />
          </td>
        </tr>
      )}
    </>
  )
}

export function IntegrationsPage() {
  const list = useLoad((s) => api<Integration[]>('/integrations', { signal: s }), [])
  const drivers = useLoad((s) => api<DriverInfo[]>('/drivers', { signal: s }), [])
  const [adding, setAdding] = useState(false)
  const grouped = useMemo(() => kindOrder.map((k) => ({ kind: k, items: (list.data ?? []).filter((i) => i.kind === k) })), [list.data])

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Integrations</h1>
          <p className="sub">The systems Plinth connects to. Blueprints combine them.</p>
        </div>
        {!adding && <button className="btn primary" onClick={() => setAdding(true)}>Add an integration</button>}
      </div>
      {list.error && <Banner title="Could not load integrations.">{' '}{list.error}</Banner>}
      {adding && drivers.data && <CreateForm drivers={drivers.data} onCancel={() => setAdding(false)} onDone={() => { setAdding(false); list.reload() }} />}

      {list.data && list.data.length === 0 && !adding && (
        <div className="panel empty">
          <h2>No integrations yet</h2>
          <p>Connect your Git host, container registry, Kubernetes clusters and database servers.</p>
          <button className="btn primary" onClick={() => setAdding(true)}>Add the first one</button>
        </div>
      )}

      {grouped.filter((g) => g.items.length > 0).map((g) => (
        <section key={g.kind} aria-label={kindLabel[g.kind]} style={{ marginBottom: 20 }}>
          <h2 style={{ marginBottom: 8 }}>{kindLabel[g.kind]}</h2>
          <div className="panel flush">
            <table className="table">
              <thead>
                <tr><th scope="col">Name</th><th scope="col">Product</th><th scope="col">Changed</th><th scope="col">Actions</th></tr>
              </thead>
              <tbody>
                {g.items.map((i) => <Row key={i.id} item={i} onChanged={list.reload} />)}
              </tbody>
            </table>
          </div>
        </section>
      ))}
    </>
  )
}
