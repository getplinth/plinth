import { api } from '../api'
import { Banner } from '../components'
import { useLoad } from '../hooks'
import { fullTime, relativeTime } from '../lib/format'
import type { AuditEntry } from '../types'

export function AuditPage() {
  const { data, error, loading } = useLoad((s) => api<AuditEntry[]>('/audit?limit=200', { signal: s }), [])
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Audit log</h1>
          <p className="sub">Who changed what, newest first. Secrets are never recorded.</p>
        </div>
      </div>
      {error && <Banner title="Could not load the audit log.">{' '}{error}</Banner>}
      {loading && !data && <p className="muted" role="status">Loading…</p>}
      {data && data.length === 0 && <div className="panel empty"><h2>Nothing recorded yet</h2></div>}
      {data && data.length > 0 && (
        <div className="panel flush">
          <table className="table">
            <thead>
              <tr><th scope="col">When</th><th scope="col">Who</th><th scope="col">Action</th><th scope="col">Target</th></tr>
            </thead>
            <tbody>
              {data.map((e) => (
                <tr key={e.id}>
                  <td data-label="When" title={fullTime(e.at)}>{relativeTime(e.at)}</td>
                  <td data-label="Who">{e.actor}</td>
                  <td data-label="Action"><span className="chip">{e.action}</span></td>
                  <td data-label="Target" className="mono">{e.target}
                    {e.detail != null && <pre className="detail">{JSON.stringify(e.detail, null, 2)}</pre>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
