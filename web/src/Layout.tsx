import { useEffect } from 'react'
import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { api } from './api'
import { useLoad } from './hooks'
import { approvalsChanged, waitingPath } from './pages/Approvals'
import type { RequestRecord } from './types'
import { signOut } from './auth'
import { Mark } from './components'
import { useMe } from './session'

export function Layout() {
  const me = useMe()
  const navigate = useNavigate()
  // Admins see how many requests are waiting for them. A failed check just hides the number.
  const waiting = useLoad(async (s) => (me.role === 'admin' ? (await api<RequestRecord[]>(waitingPath, { signal: s })).filter((r) => r.requested_by !== me.email).length : 0), [me.role], () => 20000)
  const count = waiting.data ?? 0
  const reloadWaiting = waiting.reload
  useEffect(() => {
    window.addEventListener(approvalsChanged, reloadWaiting)
    return () => window.removeEventListener(approvalsChanged, reloadWaiting)
  }, [reloadWaiting])
  const leave = async () => {
    await signOut()
    navigate('/signed-out', { replace: true })
  }
  return (
    <>
      <a className="sr-only" href="#main">Skip to content</a>
      <header className="topbar">
        <div className="topbar-inner">
          <NavLink to="/" className="brand" end>
            <Mark /> Plinth
          </NavLink>
          <nav className="nav" aria-label="Main">
            <NavLink to="/" end>Requests</NavLink>
            <NavLink to="/new">New request</NavLink>
            <NavLink to="/teams">Teams</NavLink>
            {me.role === 'admin' && (
              <>
                <span className="nav-label" aria-hidden="true">Admin</span>
                <NavLink to="/admin/approvals" aria-label={count > 0 ? `Approvals, ${count} waiting for you` : 'Approvals'}>
                  Approvals{count > 0 && <span className="badge" aria-hidden="true">{count}</span>}
                </NavLink>
                <NavLink to="/admin/integrations">Integrations</NavLink>
                <NavLink to="/admin/blueprints">Blueprints</NavLink>
                <NavLink to="/admin/tiers">Quota tiers</NavLink>
                <NavLink to="/admin/users">Users</NavLink>
                <NavLink to="/admin/sign-in">Sign-in</NavLink>
                <NavLink to="/admin/notifications">Notifications</NavLink>
                <NavLink to="/admin/audit">Audit log</NavLink>
              </>
            )}
          </nav>
          <div className="who">
            <span>{me.email}</span>
            {me.role === 'admin' && <span className="chip accent">admin</span>}
            {me.auth_source === 'local' && <NavLink to="/account/password" className="btn sm quiet">Change password</NavLink>}
            <button className="btn sm quiet" onClick={leave}>Sign out</button>
          </div>
        </div>
      </header>
      <main id="main" className="page">
        <Outlet />
      </main>
    </>
  )
}
