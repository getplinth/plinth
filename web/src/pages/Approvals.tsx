import { useCallback, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api'
import { Banner, ErrorBanner } from '../components'
import { useAction, useLoad } from '../hooks'
import { fullTime, relativeTime } from '../lib/format'
import { namespaceOf } from '../lib/naming'
import { useMe } from '../session'
import type { ProjectInput, RequestRecord } from '../types'

export const waitingPath = '/requests?all=true&status=awaiting_approval&limit=200'

/** Told to the navigation bar so its count updates the moment a decision is made. */
export const approvalsChanged = 'plinth:approvals-changed'

function describeProject(p: ProjectInput): string {
  const bits = [p.quota ?? 'bronze']
  if (p.use_db) bits.push(`${p.db_type ?? 'database'} database`)
  if (p.host_url) bits.push(p.host_url)
  if (p.public) bits.push('public repository')
  return bits.join(' · ')
}

function Card({ rec, onDecided }: { rec: RequestRecord; onDecided: (message: string) => void }) {
  const me = useMe()
  const [note, setNote] = useState('')
  const own = rec.requested_by === me.email

  const decideFn = useCallback(
    async (action: 'approve' | 'reject') => {
      await api(`/requests/${rec.id}/${action}`, { method: 'POST', body: { note } })
      onDecided(`${action === 'approve' ? 'Approved' : 'Rejected'} ${rec.tenant}.`)
    },
    [rec.id, rec.tenant, note, onDecided],
  )
  const decide = useAction(decideFn)
  const noteId = `note-${rec.id}`

  return (
    <article className="panel" aria-label={`Request from ${rec.requested_by} for ${rec.tenant}`}>
      <div className="page-head" style={{ marginBottom: 8 }}>
        <div>
          <h2>
            <Link to={`/requests/${rec.id}`} className="title">{rec.tenant}</Link>
          </h2>
          <p className="facts">
            <span>Requested by <b>{rec.requested_by}</b></span>
            <span>Blueprint <b>{rec.blueprint}</b></span>
            <span title={fullTime(rec.created_at)}>{relativeTime(rec.created_at)}</span>
          </p>
        </div>
      </div>

      <ul className="plain">
        {rec.payload.projects.map((p) => (
          <li key={p.name}>
            <strong>{p.name}</strong> <span className="mono muted">{namespaceOf(rec.tenant, p.name)}</span>
            <div className="sub">{describeProject(p)}</div>
          </li>
        ))}
      </ul>

      <ErrorBanner message={decide.error} details={decide.details} />
      <div className="field" style={{ marginTop: 14 }}>
        <label htmlFor={noteId}>Note <span className="opt">(required to reject)</span></label>
        <input id={noteId} type="text" value={note} maxLength={500} onChange={(e) => setNote(e.target.value)} placeholder="Why, or what to change" autoComplete="off" />
        <p className="hint">The requester sees this, and it is kept in the audit log.</p>
      </div>
      {own && <Banner kind="info" title="You asked for this one.">{' '}Another administrator has to approve it. You can still reject it.</Banner>}
      <div className="actions">
        <button className="btn primary" disabled={decide.busy || own} onClick={() => void decide.run('approve')}>Approve</button>
        <button className="btn danger" disabled={decide.busy || note.trim().length < 3} onClick={() => void decide.run('reject')}>Reject</button>
      </div>
    </article>
  )
}

export function ApprovalsPage() {
  const { data, error, loading, reload } = useLoad((s) => api<RequestRecord[]>(waitingPath, { signal: s }), [], () => 10000)
  const [message, setMessage] = useState('')
  const done = useCallback(
    (m: string) => {
      setMessage(m)
      reload()
      window.dispatchEvent(new Event(approvalsChanged))
    },
    [reload],
  )

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Approvals</h1>
          <p className="sub">Requests for blueprints that need an administrator's approval. Nothing is set up until you approve.</p>
        </div>
      </div>
      {message && <Banner kind="info">{message}</Banner>}
      {error && <Banner title="Could not load the queue.">{' '}{error}</Banner>}
      {loading && !data && <p className="muted" role="status">Loading…</p>}
      {data && data.length === 0 && (
        <div className="panel empty">
          <h2>Nothing is waiting</h2>
          <p>Requests that need approval show up here as soon as they are submitted.</p>
        </div>
      )}
      {data?.map((r) => <Card key={r.id} rec={r} onDecided={done} />)}
    </>
  )
}
