import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { SignInOptions } from '../types'

let options: SignInOptions
let loginFails: string | undefined
const loginLocal = vi.fn()
const loginWithProvider = vi.fn(async (_provider: unknown) => {})

vi.mock('../auth', () => ({
  getOptions: vi.fn(async () => options),
  loginLocal: (u: string, p: string) => {
    loginLocal(u, p)
    return loginFails ? Promise.reject(new Error(loginFails)) : Promise.resolve({ mustChange: false })
  },
  loginWithProvider: (p: unknown) => loginWithProvider(p),
  returnPath: () => '/requests/abc',
}))

import { LoginPage } from './Login'

const provider = (id: string, name: string) => ({ id, name: id, display_name: name, issuer: `https://${id}.example.com`, client_id: 'plinth', scopes: 'openid' })
const open = () =>
  render(
    <MemoryRouter initialEntries={['/login']}>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/requests/:id" element={<p>the request page</p>} />
      </Routes>
    </MemoryRouter>,
  )

beforeEach(() => {
  loginLocal.mockClear()
  loginWithProvider.mockClear()
  loginFails = undefined
  options = { local_login: true, providers: [] }
})

describe('sign-in page', () => {
  it('signs in with a password and goes back to where the person was', async () => {
    const user = userEvent.setup()
    open()
    await user.type(await screen.findByLabelText('Username'), '  alice ')
    await user.type(screen.getByLabelText('Password'), 'a long passphrase')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    await waitFor(() => expect(loginLocal).toHaveBeenCalledWith('alice', 'a long passphrase'))
    expect(await screen.findByText('the request page')).toBeInTheDocument()
  })

  it('does not submit an empty form', async () => {
    open()
    expect(await screen.findByRole('button', { name: 'Sign in' })).toBeDisabled()
  })

  it('shows the answer when the password is wrong and clears the field', async () => {
    loginFails = 'invalid username or password'
    const user = userEvent.setup()
    open()
    await user.type(await screen.findByLabelText('Username'), 'alice')
    await user.type(screen.getByLabelText('Password'), 'wrong')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByText('Invalid username or password')).toBeInTheDocument()
    expect(screen.getByLabelText('Password')).toHaveValue('')
    expect(screen.queryByText('the request page')).toBeNull()
  })

  it('offers providers next to the password form', async () => {
    options = { local_login: true, providers: [provider('sso', 'Company SSO'), provider('other', 'Partner login')] }
    const user = userEvent.setup()
    open()
    expect(await screen.findByLabelText('Username')).toBeInTheDocument()
    expect(screen.getByText('or with a password')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Continue with Partner login' }))
    expect(loginWithProvider).toHaveBeenCalledWith(expect.objectContaining({ id: 'other' }))
  })

  it('goes straight to the only provider when passwords are off', async () => {
    options = { local_login: false, providers: [provider('sso', 'Company SSO')] }
    open()
    await waitFor(() => expect(loginWithProvider).toHaveBeenCalledTimes(1))
    expect(screen.queryByLabelText('Username')).toBeNull()
  })

  it('lets people choose when there are several providers and no password', async () => {
    options = { local_login: false, providers: [provider('a', 'First'), provider('b', 'Second')] }
    open()
    expect(await screen.findByRole('button', { name: 'Continue with First' })).toBeInTheDocument()
    expect(loginWithProvider).not.toHaveBeenCalled()
    expect(screen.queryByLabelText('Username')).toBeNull()
  })

  it('says what to do when nobody can sign in yet', async () => {
    options = { local_login: false, providers: [] }
    open()
    expect(await screen.findByText('Nobody can sign in yet.')).toBeInTheDocument()
    expect(screen.getByText('plinth bootstrap-admin')).toBeInTheDocument()
  })
})
