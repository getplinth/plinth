import { useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api'
import { Banner } from '../components'
import { useLoad } from '../hooks'
import { useMe } from '../session'
import type { Team } from '../types'

export const roleLabel = { owner: 'Owner', member: 'Member', '': 'Administrator view' } as const

export function RoleChip({ role }: { role: Team['role'] }) {
  return <span className={`chip ${role === 'owner' ? 'accent' : ''}`}>{roleLabel[role]}</span>
}

export function TeamsPage() {
  const me = useMe()
  const [everyone, setEveryone] = useState(false)
  const { data, error, loading } = useLoad((s) => api<Team[]>(`/tenants${everyone ? '?all=true' : ''}`, { signal: s }), [everyone])

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Teams</h1>
          <p className="sub">{everyone ? 'Every team on this platform.' : 'The teams you belong to. Members request projects together and see each other’s requests.'}</p>
        </div>
        {me.role === 'admin' && (
          <label className="check" style={{ margin: 0 }}>
            <input type="checkbox" checked={everyone} onChange={(e) => setEveryone(e.target.checked)} />
            <span>Show every team</span>
          </label>
        )}
      </div>

      {error && <Banner title="Could not load teams.">{' '}{error}</Banner>}
      {loading && !data && <p className="muted" role="status">Loading teams…</p>}

      {data && data.length === 0 && (
        <div className="panel empty">
          <h2>You are not in a team yet</h2>
          <p>
            Request a project to create a team and become its owner. To join an existing team, ask one of its owners to add you.
            They need the e-mail address you sign in with, and you must have signed in to Plinth once.
          </p>
          <Link className="btn primary" to="/new">Request a project</Link>
        </div>
      )}

      {data && data.length > 0 && (
        <div className="panel flush">
          <table className="table">
            <thead>
              <tr>
                <th scope="col">Team</th>
                <th scope="col">Your role</th>
                <th scope="col">People</th>
                <th scope="col">Projects</th>
              </tr>
            </thead>
            <tbody>
              {data.map((t) => (
                <tr key={t.name}>
                  <td data-label="Team"><Link className="title" to={`/teams/${t.name}`}>{t.name}</Link></td>
                  <td data-label="Your role"><RoleChip role={t.role} /></td>
                  <td data-label="People">{t.member_count}</td>
                  <td data-label="Projects">{t.projects.length > 0 ? t.projects.join(', ') : <span className="muted">none yet</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
