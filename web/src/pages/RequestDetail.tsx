import { useCallback } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api } from '../api'
import { Banner, Courses, ErrorBanner, StatusPill } from '../components'
import { useAction, useLoad } from '../hooks'
import { fullTime, relativeTime } from '../lib/format'
import { progress } from '../lib/steps'
import type { RequestRecord } from '../types'

const active = (r?: RequestRecord) => r?.status === 'pending' || r?.status === 'running'

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
          </p>
        </div>
        <div className="actions">
          <StatusPill status={rec.status} />
          {p.total > 0 && <span className="muted">{p.done} of {p.total} steps done</span>}
          {rec.status === 'failed' && (
            <button className="btn primary" disabled={retry.busy} onClick={() => void retry.run()}>
              {retry.busy ? 'Retrying…' : 'Retry failed steps'}
            </button>
          )}
        </div>
      </div>

      <ErrorBanner message={retry.error} details={retry.details} />
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
