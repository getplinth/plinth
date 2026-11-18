import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { signOut } from './auth'
import { Mark } from './components'
import { useMe } from './session'

export function Layout() {
  const me = useMe()
  const navigate = useNavigate()
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
            {me.role === 'admin' && (
              <>
                <span className="nav-label" aria-hidden="true">Admin</span>
                <NavLink to="/admin/integrations">Integrations</NavLink>
                <NavLink to="/admin/blueprints">Blueprints</NavLink>
                <NavLink to="/admin/audit">Audit log</NavLink>
              </>
            )}
          </nav>
          <div className="who">
            <span>{me.email}</span>
            {me.role === 'admin' && <span className="chip accent">admin</span>}
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
