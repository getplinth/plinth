import { useCallback, useState } from 'react'
import { api } from '../api'
import { Banner, ErrorBanner } from '../components'
import { useAction, useLoad } from '../hooks'
import { fullTime, relativeTime, sentence } from '../lib/format'
import type { NotificationStatus } from '../types'

const eventLabel: Record<string, string> = {
  'invitation.created': 'Team invitation',
  'request.awaiting_approval': 'Approval needed',
  'request.approved': 'Request approved',
  'request.rejected': 'Request rejected',
  'request.succeeded': 'Request done',
  'request.failed': 'Request failed',
  'request.abandoned': 'Request given up',
  test: 'Test message',
}

const stateLabel = { waiting: 'Waiting', sent: 'Sent', failed: 'Gave up' } as const

export function NotificationsPage() {
  const status = useLoad((s) => api<NotificationStatus>('/notifications', { signal: s }), [], (d) => (d?.recent.some((n) => n.state === 'waiting') ? 3000 : 0))
  const [queued, setQueued] = useState(false)
  const testFn = useCallback(async () => {
    setQueued(false)
    await api('/notifications/test', { method: 'POST' })
    setQueued(true)
    status.reload()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status.reload])
  const test = useAction(testFn)
  const d = status.data
  const off = d && !d.email && !d.webhook

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Notifications</h1>
          <p className="sub">Plinth tells people about invitations, approvals and finished or failed requests, by e-mail and to a webhook.</p>
        </div>
        {d && !off && <button className="btn primary" disabled={test.busy} onClick={() => void test.run()}>{test.busy ? 'Sending…' : 'Send a test message'}</button>}
      </div>
      {status.error && <Banner title="Could not load notifications.">{' '}{status.error}</Banner>}
      <ErrorBanner message={test.error} details={test.details} />
      {queued && <Banner kind="info" title="Test message queued.">{' '}The e-mail goes to your address. Check the list below for the outcome.</Banner>}
      {off && (
        <div className="panel empty">
          <h2>Notifications are off</h2>
          <p>Set <code>PLINTH_SMTP_HOST</code> for e-mail or <code>PLINTH_WEBHOOK_URL</code> for a webhook (Helm: <code>notifications.smtp</code> and <code>notifications.webhook</code>), then restart Plinth.</p>
        </div>
      )}
      {d && !off && (
        <p className="facts">
          <span>E-mail <b>{d.email ? 'on' : 'off'}</b></span>
          <span>Webhook <b>{d.webhook ? 'on' : 'off'}</b></span>
        </p>
      )}
      {d && !off && d.recent.length === 0 && <div className="panel empty"><h2>Nothing sent yet</h2><p>Messages appear here as people are invited and requests move.</p></div>}
      {d && d.recent.length > 0 && (
        <div className="panel flush">
          <table className="table">
            <thead>
              <tr><th scope="col">What</th><th scope="col">Channel</th><th scope="col">To</th><th scope="col">State</th><th scope="col">When</th></tr>
            </thead>
            <tbody>
              {d.recent.map((n) => (
                <tr key={n.id}>
                  <td data-label="What">{eventLabel[n.event] ?? n.event}</td>
                  <td data-label="Channel">{n.channel === 'email' ? 'E-mail' : 'Webhook'}</td>
                  <td data-label="To">{n.recipient || <span className="muted">webhook</span>}</td>
                  <td data-label="State">
                    <span className={`pill ${n.state === 'failed' ? 'bad' : n.state === 'sent' ? 'accent' : ''}`}>{stateLabel[n.state]}</span>
                    {n.attempts > 1 && <span className="muted"> after {n.attempts} tries</span>}
                    {n.last_error && n.state !== 'sent' && <div className="sub">{sentence(n.last_error)}</div>}
                  </td>
                  <td data-label="When" title={fullTime(n.created_at)}>{relativeTime(n.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
