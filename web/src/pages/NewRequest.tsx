import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { api } from '../api'
import { Banner, ErrorBanner, Field, describe } from '../components'
import { useAction, useLoad } from '../hooks'
import { databaseTitle } from '../lib/drivers'
import { emptyProject, hasErrors, toPayload, validate, type FormErrors, type NewRequestForm, type ProjectForm } from '../lib/form'
import { namespaceOf, normalize } from '../lib/naming'
import type { BlueprintSummary, QuotaTier, RequestRecord } from '../types'

const tierSummary = (t: QuotaTier) => `${t.pods} pods, ${t.requests.cpu} CPU, ${t.requests.memory} memory`

export function NewRequestPage() {
  const navigate = useNavigate()
  const blueprints = useLoad((signal) => api<BlueprintSummary[]>('/blueprints', { signal }), [])
  const tiers = useLoad((signal) => api<Record<string, QuotaTier>>('/quota-tiers', { signal }), [])

  const [form, setForm] = useState<NewRequestForm>({ blueprint: '', tenant: '', projects: [emptyProject()] })
  const [submitted, setSubmitted] = useState(false)
  const summaryRef = useRef<HTMLDivElement>(null)

  // Preselect when there is only one blueprint.
  useEffect(() => {
    if (blueprints.data?.length === 1 && !form.blueprint) setForm((f) => ({ ...f, blueprint: blueprints.data![0]!.name }))
  }, [blueprints.data, form.blueprint])

  const bp = blueprints.data?.find((b) => b.name === form.blueprint)
  const tierNames = useMemo(() => Object.keys(tiers.data ?? {}).sort((a, b) => (tiers.data![a]!.pods - tiers.data![b]!.pods)), [tiers.data])
  const errors = validate(form)
  const shown: FormErrors = submitted ? errors : { projects: form.projects.map(() => ({})) }

  const send = useCallback(async () => {
    const rec = await api<RequestRecord>('/requests', { method: 'POST', body: toPayload(form) })
    navigate(`/requests/${rec.id}`)
  }, [form, navigate])
  const submit = useAction(send)

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault()
    setSubmitted(true)
    if (hasErrors(errors)) {
      summaryRef.current?.focus()
      return
    }
    await submit.run()
  }

  const setProject = (i: number, patch: Partial<ProjectForm>) =>
    setForm((f) => ({ ...f, projects: f.projects.map((p, j) => (j === i ? { ...p, ...patch } : p)) }))
  const defaultQuota = tierNames[0] ?? 'bronze'
  const defaultDb = bp?.database_types[0] ?? ''
  const problemCount = submitted ? errors.projects.reduce((n, p) => n + Object.keys(p).length, 0) + (errors.tenant ? 1 : 0) + (errors.blueprint ? 1 : 0) : 0

  if (blueprints.loading && !blueprints.data) return <p className="muted" role="status">Loading…</p>
  if (blueprints.data && blueprints.data.length === 0) {
    return (
      <div className="panel empty">
        <h2>Nothing to request yet</h2>
        <p>An administrator has to create a blueprint before projects can be requested.</p>
      </div>
    )
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>New request</h1>
          <p className="sub">Tell Plinth what to set up. It creates the repository, registry project, namespace and database for each project.</p>
        </div>
      </div>

      <form onSubmit={onSubmit} noValidate>
        {blueprints.error && <Banner title="Could not load blueprints.">{' '}{blueprints.error}</Banner>}
        <div ref={summaryRef} tabIndex={-1}>
          {problemCount > 0 && <Banner title={`${problemCount} ${problemCount === 1 ? 'thing needs' : 'things need'} fixing below.`} />}
          <ErrorBanner message={submit.error} details={submit.details} />
        </div>

        <div className="panel">
          <h2>Team</h2>
          <div className="row">
            <Field id="blueprint" label="Blueprint" error={shown.blueprint} hint={bp ? `Sets up ${bp.environments.join(', ')}${bp.has_registry ? ', with a container registry' : ''}.${bp.requires_approval ? ' An administrator has to approve it first.' : ''}` : 'The combination of systems your projects are created in.'}>
              <select id="blueprint" value={form.blueprint} onChange={(e) => setForm({ ...form, blueprint: e.target.value, projects: form.projects.map((p) => ({ ...p, useDb: false })) })} {...describe('blueprint', shown.blueprint)}>
                <option value="">Choose a blueprint…</option>
                {blueprints.data?.map((b) => (
                  <option key={b.name} value={b.name}>{b.name}</option>
                ))}
              </select>
            </Field>
            <Field id="tenant" label="Team name" error={shown.tenant} hint="Lowercase letters, numbers and dashes. The first request for a team makes you its owner.">
              <input id="tenant" type="text" autoComplete="off" value={form.tenant} onChange={(e) => setForm({ ...form, tenant: e.target.value })} placeholder="payments" {...describe('tenant', shown.tenant, 'x')} />
            </Field>
          </div>
        </div>

        {form.projects.map((p, i) => {
          const e = shown.projects[i] ?? {}
          const id = `p${i}`
          const ns = p.name.trim() && form.tenant.trim() ? namespaceOf(form.tenant, p.name) : ''
          return (
            <fieldset key={i} className="project-card" style={{ margin: '0 0 14px' }}>
              <legend className="sr-only">Project {i + 1}</legend>
              <header>
                <h2>Project {form.projects.length > 1 ? i + 1 : ''}</h2>
                {form.projects.length > 1 && (
                  <button type="button" className="btn sm quiet" onClick={() => setForm({ ...form, projects: form.projects.filter((_, j) => j !== i) })}>
                    Remove
                  </button>
                )}
              </header>
              <div className="row">
                <Field id={`${id}-name`} label="Project name" error={e.name} hint="Used in the repository, registry project and namespace.">
                  <input id={`${id}-name`} type="text" autoComplete="off" value={p.name} onChange={(ev) => setProject(i, { name: ev.target.value })} placeholder="checkout" {...describe(`${id}-name`, e.name, 'x')} />
                </Field>
                <Field id={`${id}-quota`} label="Size" hint={tiers.data?.[p.quota] ? tierSummary(tiers.data[p.quota]!) : undefined}>
                  <select id={`${id}-quota`} value={p.quota} onChange={(ev) => setProject(i, { quota: ev.target.value })}>
                    {tierNames.length === 0 && <option value={p.quota}>{p.quota}</option>}
                    {tierNames.map((t) => (
                      <option key={t} value={t}>{t}</option>
                    ))}
                  </select>
                </Field>
              </div>
              {ns && (
                <p className="derived">
                  Creates <code>{normalize(form.tenant)}/{normalize(p.name)}</code> in Git, and the namespace and registry project <code>{ns}</code>
                  {bp ? <> in {bp.environments.join(' and ')}</> : null}.
                </p>
              )}
              <Field id={`${id}-desc`} label="Description" optional>
                <input id={`${id}-desc`} type="text" value={p.description} onChange={(ev) => setProject(i, { description: ev.target.value })} />
              </Field>
              <div className="row">
                <Field id={`${id}-host`} label="Host name" optional error={e.host} hint="Where the app will be reachable. Saved as the HOST variable.">
                  <input id={`${id}-host`} type="text" autoComplete="off" value={p.host} onChange={(ev) => setProject(i, { host: ev.target.value })} placeholder="checkout.example.com" {...describe(`${id}-host`, e.host, 'x')} />
                </Field>
                <Field id={`${id}-port`} label="Port" optional error={e.port} hint="The port the app listens on. Saved as PORT.">
                  <input id={`${id}-port`} type="text" inputMode="numeric" autoComplete="off" value={p.port} onChange={(ev) => setProject(i, { port: ev.target.value })} placeholder="8080" {...describe(`${id}-port`, e.port, 'x')} />
                </Field>
              </div>
              {bp?.has_database ? (
                <>
                  <div className="check">
                    <input id={`${id}-db`} type="checkbox" checked={p.useDb} onChange={(ev) => setProject(i, { useDb: ev.target.checked, dbType: ev.target.checked ? p.dbType || defaultDb : p.dbType })} />
                    <label htmlFor={`${id}-db`}>
                      <strong>Needs a database</strong>
                      <p className="hint">A separate database and login per environment. The password goes to the repository and the namespace, not to Plinth.</p>
                    </label>
                  </div>
                  {p.useDb && (
                    <Field id={`${id}-dbtype`} label="Database type" error={e.dbType}>
                      <select id={`${id}-dbtype`} value={p.dbType} onChange={(ev) => setProject(i, { dbType: ev.target.value })} {...describe(`${id}-dbtype`, e.dbType)}>
                        <option value="">Choose…</option>
                        {bp.database_types.map((t) => (
                          <option key={t} value={t}>{databaseTitle(t)}</option>
                        ))}
                      </select>
                    </Field>
                  )}
                </>
              ) : (
                form.blueprint && <p className="hint">This blueprint has no database servers.</p>
              )}
              <div className="check" style={{ marginBottom: 0 }}>
                <input id={`${id}-public`} type="checkbox" checked={p.isPublic} onChange={(ev) => setProject(i, { isPublic: ev.target.checked })} />
                <label htmlFor={`${id}-public`}>
                  <strong>Public repository</strong>
                  <p className="hint">Anyone who can reach the Git host can read it. Private by default.</p>
                </label>
              </div>
            </fieldset>
          )
        })}

        <div className="actions" style={{ marginBottom: 24 }}>
          <button type="button" className="btn" onClick={() => setForm({ ...form, projects: [...form.projects, emptyProject(defaultQuota, defaultDb)] })}>
            Add another project
          </button>
        </div>

        <div className="actions">
          <button className="btn primary" type="submit" disabled={submit.busy}>{submit.busy ? 'Submitting…' : bp?.requires_approval ? 'Submit for approval' : 'Submit request'}</button>
          <Link className="btn quiet" to="/">Cancel</Link>
        </div>
      </form>
    </>
  )
}
