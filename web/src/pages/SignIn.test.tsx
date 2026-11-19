import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { AuthSettings, OidcProvider } from '../types'

type Call = { path: string; method: string; body?: unknown }
const calls: Call[] = []
let providers: OidcProvider[]
let settings: AuthSettings
let failWith: { message: string; path?: string } | undefined

const provider = (id: string, extra: Partial<OidcProvider> = {}): OidcProvider => ({
  id, name: id, display_name: `Provider ${id}`, issuer: `https://${id}.example.com`, client_id: 'plinth', scopes: 'openid profile email groups',
  groups_claim: 'groups', admin_group: 'plinth-admins', trust_unverified_email: false, enabled: true, read_only: false, ...extra,
})

vi.mock('../api', () => ({
  api: vi.fn(async (path: string, opts: { method?: string; body?: unknown } = {}) => {
    const method = opts.method ?? 'GET'
    calls.push({ path, method, body: opts.body })
    if (method !== 'GET' && failWith && (!failWith.path || path.startsWith(failWith.path))) throw new Error(failWith.message)
    if (path === '/oidc-providers' && method === 'GET') return providers
    if (path === '/auth-settings' && method === 'GET') return settings
    return {}
  }),
  messageOf: (e: unknown) => (e instanceof Error ? e.message : String(e)),
}))

import { SignInPage } from './SignIn'

const open = () => render(<MemoryRouter><SignInPage /></MemoryRouter>)

beforeEach(() => {
  calls.length = 0
  failWith = undefined
  providers = []
  settings = { local_login_enabled: true, forced: false }
})

describe('sign-in settings page', () => {
  it('explains the empty state: everyone uses a password', async () => {
    open()
    expect(await screen.findByText('No identity provider yet')).toBeInTheDocument()
    expect(screen.getByRole('switch')).toBeChecked()
  })

  it('shows the provider from the environment as read-only', async () => {
    providers = [provider('environment', { read_only: true, display_name: 'Company SSO' }), provider('second')]
    open()
    const env = (await screen.findByText('Company SSO')).closest('tr')!
    expect(within(env).getByText('set in the environment')).toBeInTheDocument()
    expect(within(env).queryByRole('button')).toBeNull()
    const other = screen.getByText('Provider second').closest('tr')!
    expect(within(other).getByRole('button', { name: 'Edit' })).toBeInTheDocument()
    expect(within(other).getByRole('button', { name: 'Delete' })).toBeInTheDocument()
  })

  it('tells the administrator the address to register at the provider', async () => {
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'Add a provider' }))
    expect(screen.getByText(`${window.location.origin}/callback`)).toBeInTheDocument()
    expect(screen.getByText(/public client/)).toBeInTheDocument()
  })

  it('adds a provider with sensible defaults', async () => {
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'Add a provider' }))
    await user.type(screen.getByLabelText('Short name'), 'company-sso')
    await user.type(screen.getByLabelText('Shown on the sign-in page'), 'Company SSO')
    await user.type(screen.getByLabelText('Issuer address'), 'https://login.example.com/realms/plinth')
    await user.type(screen.getByLabelText('Client id'), 'plinth-web')
    await user.type(screen.getByLabelText(/Administrator group/), 'plinth-admins')
    await user.click(screen.getByRole('button', { name: 'Save provider' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'POST')).toBeTruthy())
    expect(calls.find((c) => c.method === 'POST')).toMatchObject({
      path: '/oidc-providers',
      body: { name: 'company-sso', display_name: 'Company SSO', issuer: 'https://login.example.com/realms/plinth', client_id: 'plinth-web', scopes: 'openid profile email groups', groups_claim: 'groups', admin_group: 'plinth-admins', trust_unverified_email: false, enabled: true },
    })
  })

  it('checks the form before asking the server', async () => {
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'Add a provider' }))
    await user.type(screen.getByLabelText('Short name'), 'Bad Name')
    await user.type(screen.getByLabelText('Issuer address'), 'login.example.com')
    await user.click(screen.getByRole('button', { name: 'Save provider' }))
    expect(await screen.findByText('Use lowercase letters, numbers and dashes.')).toBeInTheDocument()
    expect(screen.getByText(/Enter the issuer address/)).toBeInTheDocument()
    expect(screen.getByText(/Enter the client id/)).toBeInTheDocument()
    expect(calls.some((c) => c.method === 'POST')).toBe(false)
  })

  it("shows the server's reason when the provider cannot be used", async () => {
    failWith = { message: 'the provider cannot be used: cannot discover https://x.example.com: connection refused', path: '/oidc-providers' }
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('button', { name: 'Add a provider' }))
    await user.type(screen.getByLabelText('Short name'), 'x')
    await user.type(screen.getByLabelText('Shown on the sign-in page'), 'X')
    await user.type(screen.getByLabelText('Issuer address'), 'https://x.example.com')
    await user.type(screen.getByLabelText('Client id'), 'c')
    await user.click(screen.getByRole('button', { name: 'Save provider' }))
    expect(await screen.findByText(/connection refused/)).toBeInTheDocument()
  })

  it('edits an existing provider without renaming it', async () => {
    providers = [provider('second')]
    const user = userEvent.setup()
    open()
    const row = (await screen.findByText('Provider second')).closest('tr')!
    await user.click(within(row).getByRole('button', { name: 'Edit' }))
    expect(screen.getByLabelText('Short name')).toBeDisabled()
    await user.clear(screen.getByLabelText('Shown on the sign-in page'))
    await user.type(screen.getByLabelText('Shown on the sign-in page'), 'Renamed')
    await user.click(screen.getByRole('button', { name: 'Save provider' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'PUT')).toMatchObject({ path: '/oidc-providers/second', body: { display_name: 'Renamed' } }))
  })

  it('deletes after confirmation and explains a refusal', async () => {
    providers = [provider('second')]
    failWith = { message: 'this is the only way to sign in: turn password sign-in back on first', path: '/oidc-providers/' }
    const user = userEvent.setup()
    open()
    const row = (await screen.findByText('Provider second')).closest('tr')!
    await user.click(within(row).getByRole('button', { name: 'Delete' }))
    await user.click(within(row).getByRole('button', { name: 'Confirm delete' }))
    expect(await within(row).findByRole('alert')).toHaveTextContent('only way to sign in')
  })

  it('turns password sign-in off and shows why it cannot be', async () => {
    const user = userEvent.setup()
    open()
    const toggle = await screen.findByRole('switch')
    await user.click(toggle)
    await waitFor(() => expect(calls.find((c) => c.method === 'PUT')).toMatchObject({ path: '/auth-settings', body: { local_login_enabled: false } }))
  })

  it('shows the refusal when no administrator has proven the provider works', async () => {
    failWith = { message: 'no administrator has signed in through an identity provider yet: do that once', path: '/auth-settings' }
    const user = userEvent.setup()
    open()
    await user.click(await screen.findByRole('switch'))
    expect(await screen.findByRole('alert')).toHaveTextContent('No administrator has signed in through an identity provider')
  })

  it('mentions the recovery switch when it is in force', async () => {
    settings = { local_login_enabled: false, forced: true }
    open()
    expect(await screen.findByText(/PLINTH_FORCE_LOCAL_LOGIN/)).toBeInTheDocument()
  })
})
