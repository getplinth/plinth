import { useCallback } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api } from '../api'
import { Banner, ConfirmButton, Courses, ErrorBanner, StatusPill } from '../components'
import { useAction, useLoad } from '../hooks'
import { fullTime, relativeTime } from '../lib/format'
import { progress } from '../lib/steps'
import { useMe } from '../session'
import type { RequestRecord, Team } from '../types'

const active = (r?: RequestRecord) => r?.status === 'awaiting_approval' || r?.status === 'pending' || r?.status === 'running'

/** Outputs are plain facts about what was created; show those that help. */
function outputRows(rec: RequestRecord): [string, string][] {
  const rows: [string, string][] = []
  for (const [k, v] of Object.entries(rec.state.outputs)) {
    if (/\.(created|populated|issued|default_branch|name)$/.test(k)) continue
    rows.push([k, v])
  }
  return rows.sort((a, b) => a[0].localeCompare(b[0]))
}

export function RequestPage() {
  const { id = '' } = useParams()
  const { data: rec, error, loading, reload } = useLoad(
    (signal) => api<RequestRecord>(`/requests/${id}`, { signal }),
    [id],
    (r) => (active(r) ? 1500 : 0),
  )
  const retryFn = useCallback(async () => {
    await api(`/requests/${id}/retry`, { method: 'POST' })
    reload()
  }, [id, reload])
  const retry = useAction(retryFn)
  const withdrawFn = useCallback(async () => {
    await api(`/requests/${id}/cancel`, { method: 'POST' })
    reload()
  }, [id, reload])
  const withdraw = useAction(withdrawFn)
  const me = useMe()
  // Teammates may retry; owners may also withdraw. A missing team (not a member) just means no extra rights.
  const team = useLoad(async (s) => (rec ? api<Team>(`/tenants/${rec.tenant}`, { signal: s }).catch(() => null) : null), [rec?.tenant])
  const teamRole = team.data?.role ?? ''

  if (loading && !rec) return <p className="muted" role="status">Loading request…</p>
  if (error && !rec) {
    return (
      <>
        <Link className="back" to="/">← Requests</Link>
        <Banner title="This request could not be opened.">{' '}{error}</Banner>
      </>
    )
  }
  if (!rec) return null

  const p = progress(rec)
  const outputs = outputRows(rec)
  const failedStep = Object.entries(rec.state.steps).find(([, s]) => s.status === 'failed')

  return (
    <>
      <Link className="back" to="/">← Requests</Link>
      <div className="page-head">
        <div>
          <h1>{rec.tenant}</h1>
          <p className="facts">
            <span>Blueprint <b>{rec.blueprint}</b></span>
            <span>Requested by <b>{rec.requested_by}</b></span>
            <span title={fullTime(rec.created_at)}>{relativeTime(rec.created_at)}</span>
            {rec.decided_by && rec.status !== 'rejected' && rec.status !== 'cancelled' && <span>Approved by <b>{rec.decided_by}</b></span>}
          </p>
        </div>
        <div className="actions">
          <StatusPill status={rec.status} />
          {p.total > 0 && <span className="muted">{p.done} of {p.total} steps done</span>}
          {rec.status === 'awaiting_approval' && (me.role === 'admin' || teamRole === 'owner' || rec.requested_by === me.email) && (
            <ConfirmButton label="Withdraw request" confirmLabel="Confirm withdraw" disabled={withdraw.busy} onConfirm={() => void withdraw.run()} />
          )}
          {rec.status === 'failed' && (me.role === 'admin' || teamRole !== '') && (
            <button className="btn primary" disabled={retry.busy} onClick={() => void retry.run()}>
              {retry.busy ? 'Retrying…' : 'Retry failed steps'}
            </button>
          )}
        </div>
      </div>

      <ErrorBanner message={retry.error} details={retry.details} />
      <ErrorBanner message={withdraw.error} details={withdraw.details} />
      {rec.status === 'awaiting_approval' && (
        <Banner kind="warn" title="Waiting for approval.">
          {' '}
          This blueprint needs an administrator to approve the request before anything is set up. Nothing has been created yet.
          {me.role === 'admin' && rec.requested_by !== me.email && (
            <> <Link to="/admin/approvals">Review it in Approvals.</Link></>
          )}
          {me.role === 'admin' && rec.requested_by === me.email && <> Because you asked for it, another administrator has to approve it.</>}
        </Banner>
      )}
      {rec.status === 'rejected' && (
        <Banner title={`Rejected by ${rec.decided_by ?? 'an administrator'}.`}>
          {' '}
          {rec.decision_note ? <>Their reason: “{rec.decision_note}” </> : null}
          Nothing was set up. Change the request and submit it again.
        </Banner>
      )}
      {rec.status === 'cancelled' && (
        <Banner kind="info" title="Withdrawn.">
          {' '}
          {rec.decision_note ? <>{rec.decision_note} </> : null}
          Nothing was set up.
        </Banner>
      )}
      {rec.status === 'failed' && (
        <Banner title="This request stopped.">
          {' '}
          {failedStep ? (
            <>
              A step did not finish: {failedStep[1].error || 'no reason was reported'}. Steps that already finished are kept, and a retry starts at the one that
              failed.
            </>
          ) : (
            rec.error
          )}
        </Banner>
      )}
      {rec.status === 'succeeded' && (
        <Banner kind="info" title="Everything is set up.">
          {' '}
          Repositories, registry access, namespaces and databases are ready. Credentials were delivered to the repository variables and the namespaces; Plinth does not keep them.
        </Banner>
      )}

      <Courses rec={rec} />

      {outputs.length > 0 && (
        <div className="panel">
          <details className="outputs">
            <summary>What was created ({outputs.length})</summary>
            <dl className="kv">
              {outputs.map(([k, v]) => (
                <div key={k} style={{ display: 'contents' }}>
                  <dt>{k}</dt>
                  <dd>{/^https?:\/\//.test(v) ? <a href={v} target="_blank" rel="noreferrer noopener">{v}</a> : v}</dd>
                </div>
              ))}
            </dl>
          </details>
        </div>
      )}
    </>
  )
}
