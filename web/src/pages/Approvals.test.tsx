import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { RequestRecord } from '../types'

const calls: { path: string; method?: string; body?: unknown }[] = []
let queue: RequestRecord[] = []

vi.mock('../api', () => ({
  api: vi.fn(async (path: string, opts: { method?: string; body?: unknown } = {}) => {
    calls.push({ path, method: opts.method, body: opts.body })
    if (path.startsWith('/requests?')) return queue
    return {}
  }),
  messageOf: (e: unknown) => String(e),
}))
vi.mock('../session', () => ({ useMe: () => ({ id: 'me', email: 'root@example.test', role: 'admin' }) }))

import { ApprovalsPage } from './Approvals'

const waiting = (id: string, by: string, tenant: string): RequestRecord => ({
  id,
  blueprint: 'regulated',
  tenant,
  requested_by: by,
  payload: { tenant, projects: [{ name: 'checkout', quota: 'silver', use_db: true, db_type: 'postgres', host_url: 'checkout.example.com' }] },
  status: 'awaiting_approval',
  state: { steps: {}, outputs: {} },
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
})

beforeEach(() => {
  calls.length = 0
  queue = [waiting('r1', 'alice@example.test', 'payments'), waiting('r2', 'root@example.test', 'mine')]
})

const open = () =>
  render(
    <MemoryRouter>
      <ApprovalsPage />
    </MemoryRouter>,
  )

describe('Approvals queue', () => {
  it('asks for the waiting requests and shows what would be created', async () => {
    open()
    const card = await screen.findByRole('article', { name: /payments/ })
    expect(calls[0]?.path).toBe('/requests?all=true&status=awaiting_approval&limit=200')
    expect(within(card).getByText('payments-checkout')).toBeInTheDocument()
    expect(within(card).getByText(/silver · postgres database · checkout.example.com/)).toBeInTheDocument()
  })

  it('approves with the note and refreshes', async () => {
    const user = userEvent.setup()
    open()
    const card = await screen.findByRole('article', { name: /payments/ })
    await user.type(within(card).getByLabelText(/Note/), 'checked')
    await user.click(within(card).getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(calls.some((c) => c.path === '/requests/r1/approve' && c.method === 'POST')).toBe(true))
    expect(calls.find((c) => c.path === '/requests/r1/approve')?.body).toEqual({ note: 'checked' })
    expect(await screen.findByText('Approved payments.')).toBeInTheDocument()
  })

  it('tells the navigation bar to refresh its count after a decision', async () => {
    const heard = vi.fn()
    window.addEventListener('plinth:approvals-changed', heard)
    const user = userEvent.setup()
    open()
    const card = await screen.findByRole('article', { name: /payments/ })
    await user.click(within(card).getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(heard).toHaveBeenCalled())
    window.removeEventListener('plinth:approvals-changed', heard)
  })

  it('will not let a reject go out without a reason', async () => {
    const user = userEvent.setup()
    open()
    const card = await screen.findByRole('article', { name: /payments/ })
    const reject = within(card).getByRole('button', { name: 'Reject' })
    expect(reject).toBeDisabled()
    await user.type(within(card).getByLabelText(/Note/), 'no')
    expect(reject).toBeDisabled()
    await user.type(within(card).getByLabelText(/Note/), ' thanks')
    expect(reject).toBeEnabled()
    await user.click(reject)
    await waitFor(() => expect(calls.some((c) => c.path === '/requests/r1/reject')).toBe(true))
    expect(await screen.findByText('Rejected payments.')).toBeInTheDocument()
  })

  it("does not offer approval of the administrator's own request", async () => {
    open()
    const own = await screen.findByRole('article', { name: /mine/ })
    expect(within(own).getByRole('button', { name: 'Approve' })).toBeDisabled()
    expect(within(own).getByText(/Another administrator has to approve it/)).toBeInTheDocument()
  })

  it('says so when nothing is waiting', async () => {
    queue = []
    open()
    expect(await screen.findByText('Nothing is waiting')).toBeInTheDocument()
  })
})
