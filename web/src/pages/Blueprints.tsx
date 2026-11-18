import { useCallback, useState, type FormEvent } from 'react'
import { api } from '../api'
import { Banner, ConfirmButton, ErrorBanner, Field } from '../components'
import { useAction, useLoad } from '../hooks'
import type { Blueprint, BlueprintEnv, Integration } from '../types'

interface VarRow {
  key: string
  value: string
}

const blankEnv = (): BlueprintEnv => ({ name: '', cluster: '', database: '', access_prefix: '', ingress_namespace: '' })

function toForm(b?: Blueprint) {
  return {
    name: b?.name ?? '',
    git_source: b?.git_source ?? '',
    registry: b?.registry ?? '',
    git_parent: b?.git_parent ?? '',
    template_repo: b?.template_repo ?? '',
    branches: (b?.extra_branches ?? []).join(', '),
    approval: b?.requires_approval ?? false,
    vars: Object.entries(b?.template_vars ?? {}).map(([key, value]): VarRow => ({ key, value })),
    envs: b?.environments.length ? b.environments.map((e) => ({ ...blankEnv(), ...e })) : [blankEnv()],
  }
}

function Editor({ existing, integrations, onDone, onCancel }: { existing?: Blueprint; integrations: Integration[]; onDone: () => void; onCancel: () => void }) {
  const [f, setF] = useState(() => toForm(existing))
  const [tried, setTried] = useState(false)
  const of = (kind: string) => integrations.filter((i) => i.kind === kind)

  const body = () => ({
    name: f.name.trim(),
    git_source: f.git_source,
    registry: f.registry || undefined,
    git_parent: f.git_parent.trim() || undefined,
    template_repo: f.template_repo.trim() || undefined,
    template_vars: Object.fromEntries(f.vars.filter((v) => v.key.trim()).map((v) => [v.key.trim(), v.value])),
    extra_branches: f.branches.split(',').map((s) => s.trim()).filter(Boolean),
    requires_approval: f.approval,
    environments: f.envs.map((e) => ({
      name: e.name.trim(),
      cluster: e.cluster,
      database: e.database || undefined,
      access_prefix: e.access_prefix?.trim() || undefined,
      ingress_namespace: e.ingress_namespace?.trim() || undefined,
    })),
  })
  const send = useCallback(async () => {
    if (existing) await api(`/blueprints/${existing.name}`, { method: 'PUT', body: body() })
    else await api('/blueprints', { method: 'POST', body: body() })
    onDone()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [f, existing, onDone])
  const save = useAction(send)

  const problems = {
    name: !f.name.trim() ? 'Enter a name.' : !/^[a-z0-9][a-z0-9-]*$/.test(f.name.trim()) ? 'Use lowercase letters, numbers and dashes.' : undefined,
    git: !f.git_source ? 'Choose the Git host.' : undefined,
    envs: f.envs.some((e) => !e.name.trim() || !e.cluster) ? 'Every environment needs a name and a cluster.' : undefined,
  }
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setTried(true)
    if (problems.name || problems.git || problems.envs) return
    await save.run()
  }
  const setEnv = (i: number, patch: Partial<BlueprintEnv>) => setF({ ...f, envs: f.envs.map((e, j) => (j === i ? { ...e, ...patch } : e)) })
  const move = (i: number, d: -1 | 1) => {
    const envs = [...f.envs]
    const j = i + d
    if (j < 0 || j >= envs.length) return
    ;[envs[i], envs[j]] = [envs[j]!, envs[i]!]
    setF({ ...f, envs })
  }
  const select = (id: string, kind: string, value: string, set: (v: string) => void, optional = false) => (
    <select id={id} value={value} onChange={(e) => set(e.target.value)}>
      <option value="">{optional ? 'None' : 'Choose…'}</option>
      {of(kind).map((i) => <option key={i.id} value={i.name}>{i.name}</option>)}
    </select>
  )

  return (
    <form className="panel" onSubmit={submit} noValidate>
      <h2>{existing ? `Edit ${existing.name}` : 'New blueprint'}</h2>
      <ErrorBanner message={save.error} details={save.details} />
      <div className="row">
        <Field id="bp-name" label="Name" error={tried ? problems.name : undefined} hint={existing ? 'A blueprint cannot be renamed.' : 'What developers choose in the request form.'}>
          <input id="bp-name" type="text" autoComplete="off" value={f.name} disabled={!!existing} onChange={(e) => setF({ ...f, name: e.target.value })} placeholder="standard" />
        </Field>
        <Field id="bp-git" label="Git host" error={tried ? problems.git : undefined}>{select('bp-git', 'gitsource', f.git_source, (v) => setF({ ...f, git_source: v }))}</Field>
        <Field id="bp-reg" label="Container registry" optional>{select('bp-reg', 'registry', f.registry, (v) => setF({ ...f, registry: v }), true)}</Field>
      </div>
      <div className="row">
        <Field id="bp-parent" label="Git organization or group" optional hint="Where team groups are created. Some Git hosts ignore this.">
          <input id="bp-parent" type="text" value={f.git_parent} onChange={(e) => setF({ ...f, git_parent: e.target.value })} placeholder="tenants" />
        </Field>
        <Field id="bp-tpl" label="Template repository" optional hint="Copied into every new repository, as owner/name.">
          <input id="bp-tpl" type="text" value={f.template_repo} onChange={(e) => setF({ ...f, template_repo: e.target.value })} placeholder="platform/app-template" />
        </Field>
        <Field id="bp-branches" label="Extra branches" optional hint="Created from the default branch, separated by commas.">
          <input id="bp-branches" type="text" value={f.branches} onChange={(e) => setF({ ...f, branches: e.target.value })} placeholder="develop, staging" />
        </Field>
      </div>

      <div className="check">
        <input id="bp-approval" type="checkbox" checked={f.approval} onChange={(e) => setF({ ...f, approval: e.target.checked })} />
        <label htmlFor="bp-approval">
          <strong>Needs approval</strong>
          <p className="hint">Requests wait until an administrator other than the requester approves them. Nothing is set up before then.</p>
        </label>
      </div>

      <div className="field">
        <span className="label">Replacements in the template <span className="opt">(optional)</span></span>
        <p className="hint">Text replaced in every template file. Use {'{project}'} and {'{tenant}'} in the value.</p>
        <div className="kvrows">
          {f.vars.map((v, i) => (
            <div className="kvrow" key={i}>
              <input type="text" aria-label="Text to replace" placeholder="PROJECT_NAME" value={v.key} onChange={(e) => setF({ ...f, vars: f.vars.map((x, j) => (j === i ? { ...x, key: e.target.value } : x)) })} />
              <input type="text" aria-label="Replacement" placeholder="{project}" value={v.value} onChange={(e) => setF({ ...f, vars: f.vars.map((x, j) => (j === i ? { ...x, value: e.target.value } : x)) })} />
              <button type="button" className="btn sm quiet" onClick={() => setF({ ...f, vars: f.vars.filter((_, j) => j !== i) })}>Remove</button>
            </div>
          ))}
        </div>
        <div><button type="button" className="btn sm" onClick={() => setF({ ...f, vars: [...f.vars, { key: '', value: '' }] })}>Add a replacement</button></div>
      </div>

      <div className="field">
        <span className="label">Environments</span>
        <p className="hint">Created in this order. Each has its own cluster, and a database server if it needs databases.</p>
        {tried && problems.envs && <p className="field-error">{problems.envs}</p>}
        {f.envs.map((e, i) => (
          <div className="env-row" key={i}>
            <Field id={`env-${i}-name`} label="Name"><input id={`env-${i}-name`} type="text" value={e.name} onChange={(ev) => setEnv(i, { name: ev.target.value })} placeholder="dev" /></Field>
            <Field id={`env-${i}-cluster`} label="Cluster">{select(`env-${i}-cluster`, 'cluster', e.cluster, (v) => setEnv(i, { cluster: v }))}</Field>
            <Field id={`env-${i}-db`} label="Database server" optional>{select(`env-${i}-db`, 'database', e.database ?? '', (v) => setEnv(i, { database: v }), true)}</Field>
            <Field id={`env-${i}-prefix`} label="Group prefix" optional><input id={`env-${i}-prefix`} type="text" value={e.access_prefix ?? ''} onChange={(ev) => setEnv(i, { access_prefix: ev.target.value })} placeholder="oidc:" /></Field>
            <Field id={`env-${i}-ingress`} label="Ingress namespace" optional><input id={`env-${i}-ingress`} type="text" value={e.ingress_namespace ?? ''} onChange={(ev) => setEnv(i, { ingress_namespace: ev.target.value })} placeholder="ingress" /></Field>
            <div className="actions" style={{ marginBottom: 2 }}>
              <button type="button" className="btn sm quiet" aria-label={`Move ${e.name || 'environment'} up`} disabled={i === 0} onClick={() => move(i, -1)}>Up</button>
              <button type="button" className="btn sm quiet" aria-label={`Move ${e.name || 'environment'} down`} disabled={i === f.envs.length - 1} onClick={() => move(i, 1)}>Down</button>
              <button type="button" className="btn sm quiet" disabled={f.envs.length === 1} onClick={() => setF({ ...f, envs: f.envs.filter((_, j) => j !== i) })}>Remove</button>
            </div>
          </div>
        ))}
        <div><button type="button" className="btn sm" onClick={() => setF({ ...f, envs: [...f.envs, blankEnv()] })}>Add an environment</button></div>
      </div>

      <div className="actions">
        <button className="btn primary" disabled={save.busy}>{save.busy ? 'Saving…' : existing ? 'Save changes' : 'Create blueprint'}</button>
        <button type="button" className="btn quiet" onClick={onCancel}>Cancel</button>
      </div>
    </form>
  )
}

function Card({ bp, integrations, onChanged }: { bp: Blueprint; integrations: Integration[]; onChanged: () => void }) {
  const [editing, setEditing] = useState(false)
  const delFn = useCallback(async () => {
    await api(`/blueprints/${bp.name}`, { method: 'DELETE' })
    onChanged()
  }, [bp.name, onChanged])
  const del = useAction(delFn)

  if (editing) return <Editor existing={bp} integrations={integrations} onCancel={() => setEditing(false)} onDone={() => { setEditing(false); onChanged() }} />
  return (
    <div className="panel">
      <div className="page-head" style={{ marginBottom: 10 }}>
        <h2>{bp.name} {bp.requires_approval && <span className="chip warn">needs approval</span>}</h2>
        <div className="actions">
          <button className="btn sm" onClick={() => setEditing(true)}>Edit</button>
          <ConfirmButton label="Delete" confirmLabel="Confirm delete" disabled={del.busy} onConfirm={() => void del.run()} />
        </div>
      </div>
      {del.error && <p className="field-error">{del.error === 'still referenced by other objects' ? 'Requests were made with this blueprint, so it cannot be deleted.' : del.error}</p>}
      <dl className="kv">
        <dt>git host</dt><dd>{bp.git_source}{bp.git_parent ? ` (${bp.git_parent})` : ''}</dd>
        <dt>registry</dt><dd>{bp.registry || 'none'}</dd>
        <dt>template</dt><dd>{bp.template_repo || 'none'}{bp.extra_branches?.length ? `, plus branches ${bp.extra_branches.join(', ')}` : ''}</dd>
        <dt>environments</dt>
        <dd>{bp.environments.map((e) => (
          <span key={e.name} className="chip env" style={{ marginRight: 6 }}>{e.name}: {e.cluster}{e.database ? ` + ${e.database}` : ''}</span>
        ))}</dd>
      </dl>
    </div>
  )
}

export function BlueprintsPage() {
  const bps = useLoad((s) => api<Blueprint[]>('/blueprints', { signal: s }), [])
  const ints = useLoad((s) => api<Integration[]>('/integrations', { signal: s }), [])
  const [adding, setAdding] = useState(false)
  const noGit = ints.data && !ints.data.some((i) => i.kind === 'gitsource')
  const noCluster = ints.data && !ints.data.some((i) => i.kind === 'cluster')

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Blueprints</h1>
          <p className="sub">A blueprint is the set of systems a project is created in. Developers pick one by name.</p>
        </div>
        {!adding && <button className="btn primary" onClick={() => setAdding(true)} disabled={!!(noGit || noCluster)}>New blueprint</button>}
      </div>
      {bps.error && <Banner title="Could not load blueprints.">{' '}{bps.error}</Banner>}
      {(noGit || noCluster) && <Banner kind="warn" title="Add integrations first.">{' '}A blueprint needs at least a Git host and a Kubernetes cluster.</Banner>}
      {adding && ints.data && <Editor integrations={ints.data} onCancel={() => setAdding(false)} onDone={() => { setAdding(false); bps.reload() }} />}
      {bps.data?.length === 0 && !adding && !noGit && !noCluster && (
        <div className="panel empty"><h2>No blueprints yet</h2><p>Combine your integrations into something developers can request.</p></div>
      )}
      {ints.data && bps.data?.map((b) => <Card key={b.name} bp={b} integrations={ints.data!} onChanged={bps.reload} />)}
    </>
  )
}
