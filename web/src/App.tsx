import { Link, Navigate, Route, Routes } from 'react-router-dom'
import { Layout } from './Layout'
import { Mark } from './components'
import { ApprovalsPage } from './pages/Approvals'
import { AuditPage } from './pages/Audit'
import { BlueprintsPage } from './pages/Blueprints'
import { TiersPage } from './pages/Tiers'
import { IntegrationsPage } from './pages/Integrations'
import { NewRequestPage } from './pages/NewRequest'
import { RequestPage } from './pages/RequestDetail'
import { RequestsPage } from './pages/Requests'
import { TeamPage } from './pages/Team'
import { TeamsPage } from './pages/Teams'
import { Session, useMe } from './session'
import { startLogin } from './auth'

function SignedOut() {
  return (
    <div className="login-wait">
      <Mark size={44} />
      <h1>You're signed out</h1>
      <p className="muted">Your session ended on this device.</p>
      <button className="btn primary" onClick={() => void startLogin('/')}>Sign in again</button>
    </div>
  )
}

function AdminOnly({ children }: { children: JSX.Element }) {
  const me = useMe()
  if (me.role !== 'admin') {
    return (
      <div className="panel empty">
        <h2>Administrators only</h2>
        <p>Your account can request projects but cannot change how they are set up.</p>
        <Link className="btn" to="/">Back to requests</Link>
      </div>
    )
  }
  return children
}

function NotFound() {
  return (
    <div className="panel empty">
      <h2>That page doesn't exist</h2>
      <Link className="btn" to="/">Go to requests</Link>
    </div>
  )
}

export function App() {
  return (
    <Session>
      <Routes>
        <Route path="/signed-out" element={<SignedOut />} />
        <Route path="/callback" element={null} />
        <Route element={<Layout />}>
          <Route index element={<RequestsPage />} />
          <Route path="new" element={<NewRequestPage />} />
          <Route path="teams" element={<TeamsPage />} />
          <Route path="teams/:name" element={<TeamPage />} />
          <Route path="requests/:id" element={<RequestPage />} />
          <Route path="requests" element={<Navigate to="/" replace />} />
          <Route path="admin/approvals" element={<AdminOnly><ApprovalsPage /></AdminOnly>} />
          <Route path="admin/integrations" element={<AdminOnly><IntegrationsPage /></AdminOnly>} />
          <Route path="admin/blueprints" element={<AdminOnly><BlueprintsPage /></AdminOnly>} />
          <Route path="admin/tiers" element={<AdminOnly><TiersPage /></AdminOnly>} />
          <Route path="admin/audit" element={<AdminOnly><AuditPage /></AdminOnly>} />
          <Route path="*" element={<NotFound />} />
        </Route>
      </Routes>
    </Session>
  )
}
