import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { NotificationStatus } from '../types'

type Call = { path: string; method: string }
const calls: Call[] = []
let status: NotificationStatus
let failTest: string | undefined

vi.mock('../api', () => ({
  api: vi.fn(async (path: string, opts: { method?: string } = {}) => {
    const method = opts.method ?? 'GET'
    calls.push({ path, method })
    if (method === 'POST' && failTest) throw new Error(failTest)
    if (path === '/notifications' && method === 'GET') return status
    return {}
  }),
  messageOf: (e: unknown) => (e instanceof Error ? e.message : String(e)),
}))

import { NotificationsPage } from './Notifications'

const open = () => render(<MemoryRouter><NotificationsPage /></MemoryRouter>)
const now = new Date().toISOString()

beforeEach(() => {
  calls.length = 0
  failTest = undefined
  status = { email: true, webhook: true, recent: [] }
})

describe('notifications page', () => {
  it('explains how to switch notifications on when no channel is configured', async () => {
    status = { email: false, webhook: false, recent: [] }
    open()
    expect(await screen.findByText('Notifications are off')).toBeInTheDocument()
    expect(screen.getByText('PLINTH_SMTP_HOST')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Send a test message' })).toBeNull()
  })

  it('shows what was sent and why something gave up', async () => {
    status = {
      email: true, webhook: true,
      recent: [
        { id: 3, channel: 'email', recipient: 'bob@example.test', event: 'invitation.created', state: 'sent', attempts: 1, created_at: now },
        { id: 2, channel: 'webhook', event: 'request.failed', state: 'failed', attempts: 8, last_error: 'the webhook answered 502', created_at: now },
        { id: 1, channel: 'email', recipient: 'alice@example.test', event: 'request.succeeded', state: 'waiting', attempts: 2, last_error: 'connect to the mail server: refused', created_at: now },
      ],
    }
    open()
    expect(await screen.findByText('Team invitation')).toBeInTheDocument()
    expect(screen.getByText('bob@example.test')).toBeInTheDocument()
    expect(screen.getByText('Gave up')).toBeInTheDocument()
    expect(screen.getByText('The webhook answered 502')).toBeInTheDocument()
    expect(screen.getByText('Waiting')).toBeInTheDocument()
    expect(screen.getByText('after 8 tries')).toBeInTheDocument()
    expect(screen.getByText('Request failed')).toBeInTheDocument()
  })

  it('queues a test message and says so', async () => {
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'Send a test message' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'POST')).toMatchObject({ path: '/notifications/test' }))
    expect(await screen.findByText('Test message queued.')).toBeInTheDocument()
  })

  it('shows the server problem when the test cannot be queued', async () => {
    failTest = 'no notification channel is configured'
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'Send a test message' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('No notification channel')
  })
})
