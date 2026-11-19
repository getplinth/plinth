import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { RequestRecord } from '../types'

type Call = { path: string; method: string; body?: unknown }
const calls: Call[] = []
let rec: RequestRecord
let me = { id: 'u-root', email: 'root@example.test', role: 'admin' as 'admin' | 'user' }

vi.mock('../api', () => ({
  api: vi.fn(async (path: string, opts: { method?: string; body?: unknown } = {}) => {
    const method = opts.method ?? 'GET'
    calls.push({ path, method, body: opts.body })
    if (path.startsWith('/requests/') && method === 'GET') return rec
    if (path.startsWith('/tenants/')) throw new Error('not a member')
    return {}
  }),
  messageOf: (e: unknown) => (e instanceof Error ? e.message : String(e)),
}))
vi.mock('../session', () => ({ useMe: () => me }))

import { RequestPage } from './RequestDetail'

const record = (status: RequestRecord['status'], extra: Partial<RequestRecord> = {}): RequestRecord => ({
  id: 'r1', blueprint: 'standard', tenant: 'payments', requested_by: 'alice@example.test', status, error: '', created_at: new Date().toISOString(),
  state: { steps: { 'git/repo/app': { status: 'succeeded', attempts: 1 }, 'cluster/app/dev': { status: 'failed', attempts: 2, error: 'unreachable' } }, outputs: {} },
  plan: ['git/repo/app', 'cluster/app/dev'],
  ...extra,
} as RequestRecord)

const open = () =>
  render(
    <MemoryRouter initialEntries={['/requests/r1']}>
      <Routes><Route path="/requests/:id" element={<RequestPage />} /></Routes>
    </MemoryRouter>,
  )

beforeEach(() => {
  calls.length = 0
  me = { id: 'u-root', email: 'root@example.test', role: 'admin' }
  rec = record('failed')
})

describe('giving up a failed request', () => {
  it('is offered to administrators, warns about leftovers and needs a reason', async () => {
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'Give up…' }))
    expect(screen.getByText(/Nothing that was already created is removed/)).toBeInTheDocument()
    const form = screen.getByRole('heading', { name: 'Give up this request' }).closest('form')!
    expect(within(form).getByText('git/repo/app')).toBeInTheDocument()
    expect(within(form).getByText('cluster/app/dev')).toBeInTheDocument()
    const confirm = screen.getByRole('button', { name: 'Release the names' })
    expect(confirm).toBeDisabled()
    await user.type(screen.getByLabelText('Why is it given up?'), 'deleted the repo by hand')
    await user.click(confirm)
    await waitFor(() => expect(calls.find((c) => c.method === 'POST')).toMatchObject({ path: '/requests/r1/release', body: { note: 'deleted the repo by hand' } }))
  })

  it('is not offered to other people', async () => {
    me = { id: 'u-alice', email: 'alice@example.test', role: 'user' }
    open()
    await screen.findByText('This request stopped.')
    expect(screen.queryByRole('button', { name: 'Give up…' })).toBeNull()
  })

  it('is not offered for a request that has not failed', async () => {
    rec = record('succeeded')
    open()
    await screen.findByRole('heading', { name: 'payments' })
    expect(screen.queryByRole('button', { name: 'Give up…' })).toBeNull()
  })

  it('explains an abandoned request', async () => {
    rec = record('abandoned', { decided_by: 'root@example.test', decision_note: 'cleaned up' })
    open()
    expect(await screen.findByText('Given up by root@example.test.')).toBeInTheDocument()
    expect(screen.getByText(/The names are free again/)).toBeInTheDocument()
    expect(screen.getByText(/was not removed/)).toBeInTheDocument()
  })
})
