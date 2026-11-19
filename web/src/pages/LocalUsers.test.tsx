import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { LocalAccount } from '../types'

type Call = { path: string; method: string; body?: unknown }
const calls: Call[] = []
let accounts: LocalAccount[]
let failWith: string | undefined
let created: Partial<LocalAccount>

const account = (id: string, username: string, extra: Partial<LocalAccount> = {}): LocalAccount => ({
  id, username, email: username, role: 'user', disabled: false, must_change_password: false, created_at: new Date().toISOString(), ...extra,
})

vi.mock('../api', () => ({
  api: vi.fn(async (path: string, opts: { method?: string; body?: unknown } = {}) => {
    const method = opts.method ?? 'GET'
    calls.push({ path, method, body: opts.body })
    if (method !== 'GET' && failWith) throw new Error(failWith)
    if (path === '/local-users' && method === 'GET') return accounts
    if (path === '/local-users' && method === 'POST') return created
    if (path.endsWith('/reset-password')) return { temporary_password: 'Zq7mK2xRvB9tNd4HwY3e' }
    return {}
  }),
  messageOf: (e: unknown) => (e instanceof Error ? e.message : String(e)),
}))
vi.mock('../session', () => ({ useMe: () => ({ id: 'u-boss', email: 'boss', role: 'admin' }) }))

import { LocalUsersPage } from './LocalUsers'

const open = () => render(<MemoryRouter><LocalUsersPage /></MemoryRouter>)

beforeEach(() => {
  calls.length = 0
  failWith = undefined
  created = {}
  accounts = [
    account('u-boss', 'boss', { role: 'admin' }),
    account('u-dana', 'dana', { email: 'dana@example.test', must_change_password: true }),
    account('u-off', 'off', { disabled: true }),
  ]
})

describe('users page', () => {
  it('lists accounts with their state, and offers no actions on your own', async () => {
    open()
    const boss = (await screen.findByText('boss')).closest('tr')!
    expect(within(boss).getByText('(you)')).toBeInTheDocument()
    expect(within(boss).queryByRole('button')).toBeNull()
    expect(within(boss).getByLabelText('Role of boss')).toBeDisabled()
    const dana = screen.getByText('dana').closest('tr')!
    expect(within(dana).getByText('dana@example.test')).toBeInTheDocument()
    expect(within(dana).getByText('must choose a new password')).toBeInTheDocument()
    expect(within(screen.getByText('off').closest('tr')!).getByText('disabled')).toBeInTheDocument()
  })

  it('creates a user and shows the one-time password', async () => {
    created = account('u-new', 'erin', { must_change_password: true, temporary_password: 'Pw9aBcDeFgHjKmNpQrSt' })
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'New user' }))
    await user.type(screen.getByLabelText('Username'), 'erin')
    await user.selectOptions(screen.getByLabelText('Role'), 'admin')
    await user.click(screen.getByRole('button', { name: 'Create user' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'POST')).toMatchObject({ path: '/local-users', body: { username: 'erin', role: 'admin' } }))
    expect(await screen.findByText('Pw9aBcDeFgHjKmNpQrSt')).toBeInTheDocument()
    expect(screen.getByText(/shown only once/)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Done' }))
    expect(screen.queryByText('Pw9aBcDeFgHjKmNpQrSt')).toBeNull()
  })

  it('checks the username before asking the server', async () => {
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'New user' }))
    await user.type(screen.getByLabelText('Username'), 'has space')
    await user.click(screen.getByRole('button', { name: 'Create user' }))
    expect(await screen.findByText(/2 to 63 letters/)).toBeInTheDocument()
    expect(calls.some((c) => c.method === 'POST')).toBe(false)
  })

  it('disables and enables an account', async () => {
    const user = userEvent.setup()
    open()
    const dana = (await screen.findByText('dana')).closest('tr')!
    await user.click(within(dana).getByRole('button', { name: 'Disable' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'PUT')).toMatchObject({ path: '/local-users/u-dana', body: { disabled: true } }))
    const off = screen.getByText('off').closest('tr')!
    await user.click(within(off).getByRole('button', { name: 'Enable' }))
    await waitFor(() => expect(calls.filter((c) => c.method === 'PUT')[1]).toMatchObject({ path: '/local-users/u-off', body: { disabled: false } }))
  })

  it('changes a role', async () => {
    const user = userEvent.setup()
    open()
    await user.selectOptions(await screen.findByLabelText('Role of dana'), 'admin')
    await waitFor(() => expect(calls.find((c) => c.method === 'PUT')).toMatchObject({ path: '/local-users/u-dana', body: { role: 'admin' } }))
  })

  it('resets a password after confirmation and shows the new one once', async () => {
    const user = userEvent.setup()
    open()
    const dana = (await screen.findByText('dana')).closest('tr')!
    await user.click(within(dana).getByRole('button', { name: 'Reset password' }))
    await user.click(within(dana).getByRole('button', { name: 'Confirm reset' }))
    expect(await screen.findByText('Zq7mK2xRvB9tNd4HwY3e')).toBeInTheDocument()
    expect(screen.getByText('Password for dana')).toBeInTheDocument()
  })

  it('shows why something cannot be done', async () => {
    failWith = 'this account has requests or team memberships on record: disable it instead'
    const user = userEvent.setup()
    open()
    const dana = (await screen.findByText('dana')).closest('tr')!
    await user.click(within(dana).getByRole('button', { name: 'Delete' }))
    await user.click(within(dana).getByRole('button', { name: 'Confirm delete' }))
    expect(await within(dana).findByRole('alert')).toHaveTextContent('disable it instead')
  })
})
