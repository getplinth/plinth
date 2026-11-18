import { getToken, signOut, startLogin } from './auth'

/** An error response from the API, with the server's own explanation. */
export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public details: string[] = [],
  ) {
    super(message)
  }
}

interface Options {
  method?: string
  body?: unknown
  signal?: AbortSignal
}

/**
 * Call the Plinth API as the signed-in user. A 401 means the token expired or
 * was rejected: sign out and go through the login again, coming back here.
 */
export async function api<T>(path: string, opts: Options = {}): Promise<T> {
  const token = await getToken()
  if (!token) {
    await startLogin(window.location.pathname + window.location.search)
    return new Promise<T>(() => {}) // the page is navigating away
  }
  const res = await fetch(`/api/v1${path}`, {
    method: opts.method ?? 'GET',
    signal: opts.signal,
    headers: {
      Authorization: `Bearer ${token}`,
      ...(opts.body !== undefined ? { 'Content-Type': 'application/json' } : {}),
    },
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  })

  if (res.status === 401) {
    await signOut()
    await startLogin(window.location.pathname + window.location.search)
    return new Promise<T>(() => {})
  }
  if (res.status === 204) return undefined as T

  const text = await res.text()
  let data: unknown
  try {
    data = text ? JSON.parse(text) : undefined
  } catch {
    data = undefined
  }
  if (!res.ok) {
    const body = (data ?? {}) as { error?: string; details?: unknown }
    const details = Array.isArray(body.details) ? body.details.map(String) : []
    throw new ApiError(res.status, body.error ?? `Request failed (${res.status})`, details)
  }
  return data as T
}

/** A readable message for any thrown value. */
export function messageOf(e: unknown): string {
  if (e instanceof ApiError) return e.message
  if (e instanceof Error) return e.message
  return 'Something went wrong.'
}
