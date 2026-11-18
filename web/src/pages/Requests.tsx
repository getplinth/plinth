import { useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api'
import { Banner, StackGlyph, StatusPill } from '../components'
import { useLoad } from '../hooks'
import { fullTime, listProjects, relativeTime } from '../lib/format'
import { progress } from '../lib/steps'
import { useMe } from '../session'
import type { RequestRecord } from '../types'

const active = (list?: RequestRecord[]) => !!list?.some((r) => r.status === 'pending' || r.status === 'running')

export function RequestsPage() {
  const me = useMe()
  const [everyone, setEveryone] = useState(false)
  const { data, error, loading } = useLoad(
    (signal) => api<RequestRecord[]>(`/requests${everyone ? '?all=true' : ''}`, { signal }),
    [everyone],
    (list) => (active(list) ? 3000 : 0),
  )

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Requests</h1>
          <p className="sub">{everyone ? 'Every request on this platform.' : 'Projects you asked Plinth to set up.'}</p>
        </div>
        <div className="actions">
          {me.role === 'admin' && (
            <label className="check" style={{ margin: 0 }}>
              <input type="checkbox" checked={everyone} onChange={(e) => setEveryone(e.target.checked)} />
              <span>Show everyone's</span>
            </label>
          )}
          <Link className="btn primary" to="/new">New request</Link>
        </div>
      </div>

      {error && <Banner title="Could not load requests.">{' '}{error}</Banner>}
      {loading && !data && <p className="muted" role="status">Loading requests…</p>}

      {data && data.length === 0 && (
        <div className="panel empty">
          <h2>No requests yet</h2>
          <p>Ask for a repository, registry project, namespace and database in one go.</p>
          <Link className="btn primary" to="/new">Start a request</Link>
        </div>
      )}

      {data && data.length > 0 && (
        <div className="panel flush">
          <table className="table">
            <thead>
              <tr>
                <th scope="col">Progress</th>
                <th scope="col">Team and projects</th>
                <th scope="col">Blueprint</th>
                <th scope="col">Submitted</th>
              </tr>
            </thead>
            <tbody>
              {data.map((r) => {
                const p = progress(r)
                return (
                  <tr key={r.id}>
                    <td data-label="Progress">
                      <div className="actions" style={{ flexWrap: 'nowrap' }}>
                        <StackGlyph rec={r} />
                        <div>
                          <StatusPill status={r.status} />
                          <div className="sub">{p.total > 0 ? `${p.done} of ${p.total} steps` : ''}</div>
                        </div>
                      </div>
                    </td>
                    <td data-label="Team and projects">
                      <Link className="title" to={`/requests/${r.id}`}>{r.tenant}</Link>
                      <div className="sub">{listProjects(r.payload.projects.map((x) => x.name))}</div>
                    </td>
                    <td data-label="Blueprint">{r.blueprint}</td>
                    <td data-label="Submitted">
                      <span title={fullTime(r.created_at)}>{relativeTime(r.created_at)}</span>
                      {everyone && <div className="sub">{r.requested_by}</div>}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
