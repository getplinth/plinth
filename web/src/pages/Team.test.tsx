import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Team } from '../types'

type Call = { path: string; method: string; body?: unknown }
const calls: Call[] = []
let team: Team
let teams: Team[] = []
let failWith: string | undefined
let me = { id: 'u-alice', email: 'alice@example.test', role: 'user' as 'user' | 'admin' }

vi.mock('../api', () => ({
  api: vi.fn(async (path: string, opts: { method?: string; body?: unknown } = {}) => {
    const method = opts.method ?? 'GET'
    calls.push({ path, method, body: opts.body })
    if (method !== 'GET' && failWith) {
      const e = new Error(failWith)
      throw e
    }
    if (path === '/tenants' || path === '/tenants?all=true') return teams
    if (path.startsWith('/tenants/') && method === 'GET') return team
    return {}
  }),
  messageOf: (e: unknown) => (e instanceof Error ? e.message : String(e)),
}))
vi.mock('../session', () => ({ useMe: () => me }))

import { TeamPage } from './Team'
import { TeamsPage } from './Teams'

const member = (id: string, email: string, role: 'owner' | 'member', by = 'alice@example.test') => ({
  user_id: id, email, role, added_by: by, added_at: new Date().toISOString(),
})

const base = (role: Team['role']): Team => ({
  name: 'payments', role, member_count: 3, projects: ['checkout'], created_at: new Date().toISOString(),
  members: [member('u-alice', 'alice@example.test', 'owner'), member('u-bob', 'bob@example.test', 'member'), member('u-carol', 'carol@example.test', 'member')],
})

const open = (path = '/teams/payments') =>
  render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/teams" element={<TeamsPage />} />
        <Route path="/teams/:name" element={<TeamPage />} />
      </Routes>
    </MemoryRouter>,
  )

beforeEach(() => {
  calls.length = 0
  failWith = undefined
  me = { id: 'u-alice', email: 'alice@example.test', role: 'user' }
  team = base('owner')
  teams = [{ ...base('owner'), members: undefined }]
})

describe('team page for an owner', () => {
  it('shows the people, who added them, and the projects', async () => {
    open()
    expect(await screen.findByRole('heading', { name: 'payments' })).toBeInTheDocument()
    for (const e of ['alice@example.test', 'bob@example.test', 'carol@example.test']) expect(screen.getByText(e)).toBeInTheDocument()
    expect(screen.getAllByText(/added by alice@example.test/).length).toBeGreaterThan(0)
    expect(screen.getByText('payments-checkout')).toBeInTheDocument()
    expect(screen.getByText('(you)')).toBeInTheDocument()
  })

  it('adds a person by e-mail with the chosen role and refreshes', async () => {
    const user = userEvent.setup()
    open()
    await user.type(await screen.findByLabelText('E-mail address'), '  dave@example.test ')
    await user.selectOptions(screen.getByLabelText('Role'), 'owner')
    await user.click(screen.getByRole('button', { name: 'Add to team' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'POST')).toBeTruthy())
    expect(calls.find((c) => c.method === 'POST')).toMatchObject({ path: '/tenants/payments/members', body: { email: 'dave@example.test', role: 'owner' } })
    expect(await screen.findByText('dave@example.test was added.')).toBeInTheDocument()
    expect(calls.filter((c) => c.method === 'GET' && c.path === '/tenants/payments').length).toBeGreaterThan(1) // reloaded
  })

  it("shows the server's explanation when someone cannot be added", async () => {
    failWith = 'nobody with that e-mail address has signed in to Plinth yet: ask them to sign in once, then add them'
    const user = userEvent.setup()
    open()
    await user.type(await screen.findByLabelText('E-mail address'), 'ghost@example.test')
    await user.click(screen.getByRole('button', { name: 'Add to team' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('signed in to Plinth yet')
  })

  it('changes a role', async () => {
    const user = userEvent.setup()
    open()
    await user.selectOptions(await screen.findByLabelText('Role of bob@example.test'), 'owner')
    await waitFor(() => expect(calls.find((c) => c.method === 'PUT')).toBeTruthy())
    expect(calls.find((c) => c.method === 'PUT')).toMatchObject({ path: '/tenants/payments/members/u-bob', body: { role: 'owner' } })
  })

  it('removes someone only after a second click', async () => {
    const user = userEvent.setup()
    open()
    const bobRow = (await screen.findByText('bob@example.test')).closest('tr')!
    await user.click(within(bobRow).getByRole('button', { name: 'Remove' }))
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false)
    await user.click(within(bobRow).getByRole('button', { name: 'Confirm remove' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'DELETE')).toMatchObject({ path: '/tenants/payments/members/u-bob' }))
  })

  it('offers to leave on your own row, not to remove yourself', async () => {
    open()
    const mine = (await screen.findByText('alice@example.test')).closest('tr')!
    expect(within(mine).getByRole('button', { name: 'Leave team' })).toBeInTheDocument()
    expect(within(mine).queryByRole('button', { name: 'Remove' })).toBeNull()
  })

  it('explains why the last owner cannot step down', async () => {
    failWith = 'a team needs at least one owner: make someone else an owner first'
    const user = userEvent.setup()
    open()
    await user.selectOptions(await screen.findByLabelText('Role of alice@example.test'), 'member')
    expect(await screen.findByText(/needs at least one owner/)).toBeInTheDocument()
  })

  it('warns when there is a single owner', async () => {
    open()
    expect(await screen.findByText('This team has one owner.')).toBeInTheDocument()
  })
})

describe('team page for a plain member', () => {
  beforeEach(() => {
    me = { id: 'u-bob', email: 'bob@example.test', role: 'user' }
    team = base('member')
  })

  it('can look and leave, but not manage', async () => {
    open()
    await screen.findByRole('heading', { name: 'payments' })
    expect(screen.queryByRole('button', { name: 'Add to team' })).toBeNull()
    expect(screen.queryByLabelText(/Role of/)).toBeNull()
    const carolRow = screen.getByText('carol@example.test').closest('tr')!
    expect(within(carolRow).queryByRole('button')).toBeNull()
    const mine = screen.getByText('bob@example.test').closest('tr')!
    expect(within(mine).getByRole('button', { name: 'Leave team' })).toBeInTheDocument()
  })

  it('leaving returns to the team list', async () => {
    const user = userEvent.setup()
    teams = []
    open()
    const mine = (await screen.findByText('bob@example.test')).closest('tr')!
    await user.click(within(mine).getByRole('button', { name: 'Leave team' }))
    await user.click(within(mine).getByRole('button', { name: 'Confirm leave' }))
    expect(await screen.findByRole('heading', { name: 'Teams' })).toBeInTheDocument()
  })
})

describe('an administrator who is not in the team', () => {
  it('can manage it', async () => {
    me = { id: 'u-root', email: 'root@example.test', role: 'admin' }
    team = { ...base(''), role: '' }
    open()
    expect(await screen.findByRole('button', { name: 'Add to team' })).toBeInTheDocument()
    expect(screen.getByText(/Administrator \(not a member\)/)).toBeInTheDocument()
  })
})

describe('teams list', () => {
  it('shows your role and the people and projects of each team', async () => {
    open('/teams')
    const link = await screen.findByRole('link', { name: 'payments' })
    const row = link.closest('tr')!
    expect(within(row).getByText('Owner')).toBeInTheDocument()
    expect(within(row).getByText('3')).toBeInTheDocument()
    expect(within(row).getByText('checkout')).toBeInTheDocument()
  })

  it('tells people with no team how to join one', async () => {
    teams = []
    open('/teams')
    expect(await screen.findByText('You are not in a team yet')).toBeInTheDocument()
    expect(screen.getByText(/ask one of its owners to add you/)).toBeInTheDocument()
  })

  it('lets administrators ask for every team', async () => {
    me = { id: 'u-root', email: 'root@example.test', role: 'admin' }
    const user = userEvent.setup()
    open('/teams')
    await user.click(await screen.findByLabelText('Show every team'))
    await waitFor(() => expect(calls.some((c) => c.path === '/tenants?all=true')).toBe(true))
  })
})
