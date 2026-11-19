import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { QuotaTier } from '../types'

type Call = { path: string; method: string; body?: unknown }
const calls: Call[] = []
let failWith: { message: string; details?: string[] } | undefined

const tier = (pods: number): QuotaTier => ({
  pods,
  requests: { cpu: '2', memory: '2Gi' },
  limits: { cpu: '3', memory: '3Gi' },
  limitRange: { min: { cpu: '100m', memory: '128Mi' }, max: { cpu: '1', memory: '1Gi' }, default: { cpu: '500m', memory: '512Mi' }, defaultRequest: { cpu: '250m', memory: '256Mi' } },
})
let tiers: Record<string, QuotaTier>

vi.mock('../api', () => ({
  api: vi.fn(async (path: string, opts: { method?: string; body?: unknown } = {}) => {
    const method = opts.method ?? 'GET'
    calls.push({ path, method, body: opts.body })
    if (method !== 'GET' && failWith) {
      const e = new Error(failWith.message) as Error & { details?: string[] }
      e.details = failWith.details
      throw e
    }
    if (path === '/quota-tiers' && method === 'GET') return tiers
    return {}
  }),
  messageOf: (e: unknown) => (e instanceof Error ? e.message : String(e)),
}))

import { TiersPage } from './Tiers'

const open = () => render(<MemoryRouter><TiersPage /></MemoryRouter>)

beforeEach(() => {
  calls.length = 0
  failWith = undefined
  tiers = { bronze: tier(5), silver: tier(10) }
})

describe('quota tiers page', () => {
  it('lists the tiers, marking the default one that cannot be deleted', async () => {
    open()
    const bronze = (await screen.findByText('bronze')).closest('tr')!
    expect(within(bronze).getByText('default')).toBeInTheDocument()
    expect(within(bronze).queryByRole('button', { name: 'Delete' })).toBeNull()
    const silver = screen.getByText('silver').closest('tr')!
    expect(within(silver).getByRole('button', { name: 'Delete' })).toBeInTheDocument()
  })

  it('creates a tier from the form', async () => {
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'New tier' }))
    await user.type(screen.getByLabelText('Name'), 'platinum')
    await user.clear(screen.getByLabelText('Pods'))
    await user.type(screen.getByLabelText('Pods'), '40')
    await user.type(screen.getByLabelText('Requests: CPU'), '8')
    await user.type(screen.getByLabelText('Requests: memory'), '8Gi')
    await user.click(screen.getByRole('button', { name: 'Save tier' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'PUT')).toBeTruthy())
    expect(calls.find((c) => c.method === 'PUT')).toMatchObject({
      path: '/quota-tiers/platinum',
      body: { pods: 40, requests: { cpu: '8', memory: '8Gi' } },
    })
  })

  it('refuses a bad name before asking the server', async () => {
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'New tier' }))
    await user.type(screen.getByLabelText('Name'), 'Big One')
    await user.click(screen.getByRole('button', { name: 'Save tier' }))
    expect(await screen.findByText(/lowercase letters, numbers and dashes/)).toBeInTheDocument()
    expect(calls.some((c) => c.method === 'PUT')).toBe(false)
  })

  it('shows every problem the server found', async () => {
    failWith = { message: 'validation failed', details: ['pods must be between 1 and 1000', 'requests CPU "" is not a quantity such as 500m, 2 or 512Mi'] }
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'New tier' }))
    await user.type(screen.getByLabelText('Name'), 'broken')
    await user.click(screen.getByRole('button', { name: 'Save tier' }))
    expect(await screen.findByText(/pods must be between 1 and 1000/)).toBeInTheDocument()
    expect(screen.getByText(/is not a quantity/)).toBeInTheDocument()
  })

  it('edits an existing tier without allowing a rename', async () => {
    const user = userEvent.setup()
    open()
    const row = (await screen.findByText('silver')).closest('tr')!
    await user.click(within(row).getByRole('button', { name: 'Edit' }))
    expect(screen.getByLabelText('Name')).toBeDisabled()
    expect(screen.getByLabelText('Name')).toHaveValue('silver')
    await user.clear(screen.getByLabelText('Pods'))
    await user.type(screen.getByLabelText('Pods'), '12')
    await user.click(screen.getByRole('button', { name: 'Save tier' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'PUT')).toMatchObject({ path: '/quota-tiers/silver', body: { pods: 12 } }))
  })

  it('deletes after confirmation and shows why a tier cannot be deleted', async () => {
    const user = userEvent.setup()
    failWith = { message: '2 unfinished request(s) still use this tier: wait for them to finish or be withdrawn' }
    open()
    const row = (await screen.findByText('silver')).closest('tr')!
    await user.click(within(row).getByRole('button', { name: 'Delete' }))
    await user.click(within(row).getByRole('button', { name: 'Confirm delete' }))
    expect(await within(row).findByRole('alert')).toHaveTextContent('2 unfinished request(s)')
    expect(calls.find((c) => c.method === 'DELETE')).toMatchObject({ path: '/quota-tiers/silver' })
  })
})
