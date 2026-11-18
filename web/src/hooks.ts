import { useCallback, useEffect, useRef, useState } from 'react'
import { messageOf } from './api'

export interface Loaded<T> {
  data: T | undefined
  error: string | undefined
  loading: boolean
  reload: () => void
}

/**
 * Load data when the component mounts and whenever deps change. If pollMs
 * returns a number, reload quietly at that interval (return 0 or undefined to
 * stop). Stale responses from earlier calls are ignored.
 */
export function useLoad<T>(
  fn: (signal: AbortSignal) => Promise<T>,
  deps: unknown[],
  pollMs?: (data: T | undefined) => number | undefined,
): Loaded<T> {
  const [data, setData] = useState<T>()
  const [error, setError] = useState<string>()
  const [loading, setLoading] = useState(true)
  const [tick, setTick] = useState(0)
  const fnRef = useRef(fn)
  fnRef.current = fn
  const pollRef = useRef(pollMs)
  pollRef.current = pollMs

  useEffect(() => {
    const ctl = new AbortController()
    let timer: ReturnType<typeof setTimeout> | undefined
    let first = true
    const run = async () => {
      if (first) setLoading(true)
      try {
        const d = await fnRef.current(ctl.signal)
        if (ctl.signal.aborted) return
        setData(d)
        setError(undefined)
        const next = pollRef.current?.(d)
        if (next && next > 0) timer = setTimeout(run, next)
      } catch (e) {
        if (ctl.signal.aborted) return
        setError(messageOf(e))
        const next = pollRef.current?.(undefined)
        if (next && next > 0) timer = setTimeout(run, Math.max(next, 5000)) // back off after an error
      } finally {
        if (!ctl.signal.aborted && first) setLoading(false)
        first = false
      }
    }
    void run()
    return () => {
      ctl.abort()
      if (timer) clearTimeout(timer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick])

  const reload = useCallback(() => setTick((t) => t + 1), [])
  return { data, error, loading, reload }
}

/** Run an action with busy/error state, for buttons and forms. */
export function useAction<A extends unknown[], R>(fn: (...args: A) => Promise<R>) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const [details, setDetails] = useState<string[]>([])
  const run = useCallback(
    async (...args: A): Promise<R | undefined> => {
      setBusy(true)
      setError(undefined)
      setDetails([])
      try {
        return await fn(...args)
      } catch (e) {
        setError(messageOf(e))
        setDetails((e as { details?: string[] }).details ?? [])
        return undefined
      } finally {
        setBusy(false)
      }
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [fn],
  )
  const clear = useCallback(() => {
    setError(undefined)
    setDetails([])
  }, [])
  return { run, busy, error, details, clear }
}
