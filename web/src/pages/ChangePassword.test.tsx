import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'

type Call = { path: string; method: string; body?: unknown }
const calls: Call[] = []
let failWith: string | undefined
const assign = vi.fn()
const signOut = vi.fn(async () => {})

vi.mock('../api', () => ({
  api: vi.fn(async (path: string, opts: { method?: string; body?: unknown } = {}) => {
    calls.push({ path, method: opts.method ?? 'GET', body: opts.body })
    if (failWith) throw new Error(failWith)
    return undefined
  }),
  messageOf: (e: unknown) => (e instanceof Error ? e.message : String(e)),
}))
vi.mock('../auth', () => ({ signOut: () => signOut() }))

import { ChangePasswordPage } from './ChangePassword'

const open = (forced = false) => render(<MemoryRouter><ChangePasswordPage forced={forced} /></MemoryRouter>)
const fill = async (user: ReturnType<typeof userEvent.setup>, cur: string, next: string, again: string) => {
  await user.type(screen.getByLabelText('Current password'), cur)
  await user.type(screen.getByLabelText('New password'), next)
  await user.type(screen.getByLabelText('New password again'), again)
  await user.click(screen.getByRole('button', { name: 'Change password' }))
}

beforeEach(() => {
  calls.length = 0
  failWith = undefined
  assign.mockClear()
  signOut.mockClear()
  Object.defineProperty(window, 'location', { value: { ...window.location, assign }, writable: true })
})

describe('change password', () => {
  it('checks the new password before asking the server', async () => {
    const user = userEvent.setup()
    open()
    await fill(user, 'old-password-here', 'short', 'different')
    expect(await screen.findByText(/at least 12 characters/)).toBeInTheDocument()
    expect(screen.getByText('The two passwords differ.')).toBeInTheDocument()
    expect(calls).toHaveLength(0)
  })

  it('refuses to reuse the current password', async () => {
    const user = userEvent.setup()
    open()
    await fill(user, 'the same passphrase', 'the same passphrase', 'the same passphrase')
    expect(await screen.findByText('Choose a password different from the current one.')).toBeInTheDocument()
    expect(calls).toHaveLength(0)
  })

  it('sends both passwords and says so when it worked', async () => {
    const user = userEvent.setup()
    open()
    await fill(user, 'old-password-here', 'a brand new passphrase', 'a brand new passphrase')
    await waitFor(() => expect(calls[0]).toMatchObject({ path: '/auth/password', method: 'POST', body: { current_password: 'old-password-here', new_password: 'a brand new passphrase' } }))
    expect(await screen.findByText('Password changed.')).toBeInTheDocument()
    expect(screen.getByLabelText('Current password')).toHaveValue('')
    expect(assign).not.toHaveBeenCalled()
  })

  it("shows the server's reason", async () => {
    failWith = 'the current password is not right'
    const user = userEvent.setup()
    open()
    await fill(user, 'wrong-current-pw', 'a brand new passphrase', 'a brand new passphrase')
    expect(await screen.findByText('The current password is not right')).toBeInTheDocument()
  })

  it('when forced, explains why, continues to the app afterwards and can sign out', async () => {
    const user = userEvent.setup()
    open(true)
    expect(screen.getByText(/was set by someone else/)).toBeInTheDocument()
    await fill(user, 'first-password-xyz', 'a brand new passphrase', 'a brand new passphrase')
    await waitFor(() => expect(assign).toHaveBeenCalledWith('/'))
    await user.click(screen.getByRole('button', { name: 'Sign out' }))
    await waitFor(() => expect(signOut).toHaveBeenCalled())
    expect(assign).toHaveBeenCalledWith('/login')
  })
})
