import { useCallback, useState, type FormEvent } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { api } from '../api'
import { Banner, ConfirmButton, ErrorBanner, Field } from '../components'
import { useAction, useLoad } from '../hooks'
import { fullTime, relativeTime, sentence } from '../lib/format'
import { useMe } from '../session'
import type { Invitation, Team, TeamMember, TeamRole } from '../types'
import { RoleChip } from './Teams'

function AddMember({ team, onAdded }: { team: string; onAdded: () => void }) {
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<TeamRole>('member')
  const [done, setDone] = useState('')
  const sendFn = useCallback(async () => {
    const sent = email.trim()
    const res = await api<TeamMember | Invitation>(`/tenants/${team}/members`, { method: 'POST', body: { email: sent, role } })
    // An invitation (it has an expiry) means the person has not signed in yet.
    setDone(
      res && 'expires_at' in res
        ? `${sent} has not signed in to Plinth yet. They are invited and join automatically when they sign in with this address. Plinth does not send e-mail, so tell them.`
        : `${sent} was added.`,
    )
    setEmail('')
    onAdded()
  }, [team, email, role, onAdded])
  const add = useAction(sendFn)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setDone('')
    if (email.trim()) await add.run()
  }
  return (
    <form className="panel" onSubmit={submit} noValidate>
      <h2>Add someone</h2>
      <p className="hint" style={{ marginBottom: 12 }}>
        Use the e-mail address they sign in with. If they have not signed in to Plinth yet, they are invited and join the first time they sign in with a verified address.
      </p>
      <ErrorBanner message={add.error} details={add.details} />
      {done && <Banner kind="info">{done}</Banner>}
      <div className="row">
        <Field id="member-email" label="E-mail address">
          <input id="member-email" type="text" inputMode="email" autoComplete="off" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="name@example.com" />
        </Field>
        <Field id="member-role" label="Role" hint={role === 'owner' ? 'Owners also manage the team and its people.' : 'Members request projects and retry the team’s requests.'}>
          <select id="member-role" value={role} onChange={(e) => setRole(e.target.value as TeamRole)}>
            <option value="member">Member</option>
            <option value="owner">Owner</option>
          </select>
        </Field>
      </div>
      <button className="btn primary" disabled={add.busy || !email.trim()}>{add.busy ? 'Adding…' : 'Add to team'}</button>
    </form>
  )
}

function InvitationRow({ team, inv, onChanged }: { team: string; inv: Invitation; onChanged: () => void }) {
  const revokeFn = useCallback(async () => {
    await api(`/tenants/${team}/invitations/${inv.id}`, { method: 'DELETE' })
    onChanged()
  }, [team, inv.id, onChanged])
  const revoke = useAction(revokeFn)
  return (
    <tr>
      <td data-label="Invited">
        <span className="title">{inv.email}</span>
        <div className="sub" title={fullTime(inv.created_at)}>
          {inv.invited_by ? `invited by ${inv.invited_by}, ` : 'invited '}{relativeTime(inv.created_at)}; expires {fullTime(inv.expires_at)}
        </div>
      </td>
      <td data-label="Role"><RoleChip role={inv.role} /></td>
      <td data-label="Actions">
        <ConfirmButton label="Revoke" confirmLabel="Confirm revoke" disabled={revoke.busy} onConfirm={() => void revoke.run()} />
        {revoke.error && <p className="field-error" role="alert" style={{ marginTop: 6 }}>{sentence(revoke.error)}</p>}
      </td>
    </tr>
  )
}

function MemberRow({ team, m, canManage, selfId, onChanged, onLeft }: { team: string; m: TeamMember; canManage: boolean; selfId: string; onChanged: () => void; onLeft: () => void }) {
  const self = m.user_id === selfId
  const roleFn = useCallback(
    async (role: TeamRole) => {
      await api(`/tenants/${team}/members/${m.user_id}`, { method: 'PUT', body: { role } })
      onChanged()
    },
    [team, m.user_id, onChanged],
  )
  const setRole = useAction(roleFn)
  const removeFn = useCallback(async () => {
    await api(`/tenants/${team}/members/${m.user_id}`, { method: 'DELETE' })
    if (self) onLeft()
    else onChanged()
  }, [team, m.user_id, self, onChanged, onLeft])
  const remove = useAction(removeFn)
  const problem = setRole.error ?? remove.error

  return (
    <tr>
      <td data-label="Person">
        <span className="title">{m.email}</span> {self && <span className="muted">(you)</span>}
        {m.added_by && <div className="sub" title={fullTime(m.added_at)}>added by {m.added_by}, {relativeTime(m.added_at)}</div>}
      </td>
      <td data-label="Role">
        {canManage ? (
          <select aria-label={`Role of ${m.email}`} value={m.role} disabled={setRole.busy} onChange={(e) => void setRole.run(e.target.value as TeamRole)}>
            <option value="member">Member</option>
            <option value="owner">Owner</option>
          </select>
        ) : (
          <RoleChip role={m.role} />
        )}
      </td>
      <td data-label="Actions">
        {self ? (
          <ConfirmButton label="Leave team" confirmLabel="Confirm leave" disabled={remove.busy} onConfirm={() => void remove.run()} />
        ) : canManage ? (
          <ConfirmButton label="Remove" confirmLabel="Confirm remove" disabled={remove.busy} onConfirm={() => void remove.run()} />
        ) : null}
        {problem && <p className="field-error" role="alert" style={{ marginTop: 6 }}>{sentence(problem)}</p>}
      </td>
    </tr>
  )
}

export function TeamPage() {
  const { name = '' } = useParams()
  const me = useMe()
  const navigate = useNavigate()
  const { data: team, error, loading, reload } = useLoad((s) => api<Team>(`/tenants/${name}`, { signal: s }), [name])
  const left = useCallback(() => navigate('/teams'), [navigate])

  if (loading && !team) return <p className="muted" role="status">Loading team…</p>
  if (error && !team) {
    return (
      <>
        <Link className="back" to="/teams">← Teams</Link>
        <Banner title="This team could not be opened.">{' '}It may not exist, or you may not be in it. {error}</Banner>
      </>
    )
  }
  if (!team) return null

  const canManage = me.role === 'admin' || team.role === 'owner'
  const owners = (team.members ?? []).filter((m) => m.role === 'owner').length

  return (
    <>
      <Link className="back" to="/teams">← Teams</Link>
      <div className="page-head">
        <div>
          <h1>{team.name}</h1>
          <p className="facts">
            <span>Your role <b>{team.role ? (team.role === 'owner' ? 'Owner' : 'Member') : 'Administrator (not a member)'}</b></span>
            <span><b>{team.member_count}</b> {team.member_count === 1 ? 'person' : 'people'}</span>
            <span><b>{team.projects.length}</b> {team.projects.length === 1 ? 'project' : 'projects'}</span>
          </p>
        </div>
        <Link className="btn" to={`/?team=${team.name}`}>See this team&rsquo;s requests</Link>
      </div>

      {owners === 1 && canManage && (
        <Banner kind="info" title="This team has one owner.">{' '}A team always keeps at least one owner. Make someone else an owner before the current one leaves or steps down.</Banner>
      )}

      <div className="panel flush">
        <table className="table">
          <thead>
            <tr><th scope="col">Person</th><th scope="col">Role</th><th scope="col">Actions</th></tr>
          </thead>
          <tbody>
            {(team.members ?? []).map((m) => (
              <MemberRow key={m.user_id} team={team.name} m={m} canManage={canManage} selfId={me.id} onChanged={reload} onLeft={left} />
            ))}
          </tbody>
        </table>
      </div>

      {canManage && (team.invitations ?? []).length > 0 && (
        <div className="panel flush">
          <h2 style={{ padding: '16px 16px 0' }}>Invited, not joined yet</h2>
          <table className="table">
            <thead>
              <tr><th scope="col">Invited</th><th scope="col">Role</th><th scope="col">Actions</th></tr>
            </thead>
            <tbody>
              {(team.invitations ?? []).map((inv) => (
                <InvitationRow key={inv.id} team={team.name} inv={inv} onChanged={reload} />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {canManage && <AddMember team={team.name} onAdded={reload} />}

      <div className="panel">
        <h2>Projects</h2>
        {team.projects.length === 0 ? (
          <p className="muted">No projects yet. <Link to="/new">Request one</Link>.</p>
        ) : (
          <ul className="plain">
            {team.projects.map((p) => (
              <li key={p}><strong>{p}</strong> <span className="mono muted">{team.name}-{p}</span></li>
            ))}
          </ul>
        )}
      </div>
    </>
  )
}
