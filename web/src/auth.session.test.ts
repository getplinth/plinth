import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { getToken, loginLocal, signOut } from './auth'

// Password sessions are kept in sessionStorage, sent as a bearer token, and
// ended on the server at sign-out.
const fetchMock = vi.fn()

beforeEach(() => {
  window.sessionStorage.clear()
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.unstubAllGlobals())

const reply = (status: number, body: unknown) => fetchMock.mockResolvedValueOnce({ ok: status < 400, status, json: async () => body })

describe('password sessions', () => {
  it('stores the token after a successful sign-in and hands it out until it expires', async () => {
    reply(200, { token: 'plt_abc', expires_at: new Date(Date.now() + 3_600_000).toISOString(), must_change_password: true })
    expect(await loginLocal('alice', 'pw')).toEqual({ mustChange: true })
    expect(fetchMock).toHaveBeenCalledWith('/api/v1/auth/login', expect.objectContaining({ method: 'POST', body: JSON.stringify({ username: 'alice', password: 'pw' }) }))
    expect(await getToken()).toBe('plt_abc')
  })

  it('treats an expired token as signed out', async () => {
    reply(200, { token: 'plt_old', expires_at: new Date(Date.now() - 1000).toISOString() })
    await loginLocal('alice', 'pw')
    expect(await getToken()).toBeNull()
    expect(window.sessionStorage.getItem('plinth.local')).toBeNull()
  })

  it("keeps nothing and shows the server's message when sign-in fails", async () => {
    reply(401, { error: 'invalid username or password' })
    await expect(loginLocal('alice', 'nope')).rejects.toThrow('invalid username or password')
    expect(await getToken()).toBeNull()
  })

  it('ends the session on the server and forgets the token at sign-out', async () => {
    reply(200, { token: 'plt_abc', expires_at: new Date(Date.now() + 3_600_000).toISOString() })
    await loginLocal('alice', 'pw')
    fetchMock.mockResolvedValueOnce({ ok: true, status: 204 })
    await signOut()
    expect(fetchMock).toHaveBeenLastCalledWith('/api/v1/auth/logout', expect.objectContaining({ method: 'POST', headers: { Authorization: 'Bearer plt_abc' } }))
    expect(await getToken()).toBeNull()
  })

  it('forgets the token even when the server cannot be reached', async () => {
    reply(200, { token: 'plt_abc', expires_at: new Date(Date.now() + 3_600_000).toISOString() })
    await loginLocal('alice', 'pw')
    fetchMock.mockRejectedValueOnce(new Error('offline'))
    await signOut()
    expect(await getToken()).toBeNull()
  })

  it('is signed out when nothing was stored', async () => {
    expect(await getToken()).toBeNull()
  })
})
