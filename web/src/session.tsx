import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { api, messageOf } from './api'
import { finishLogin, getToken, startLogin } from './auth'
import { Mark } from './components'
import type { Me } from './types'

const SessionContext = createContext<Me | null>(null)

/** The signed-in user. Only available below <Session>. */
export function useMe(): Me {
  const me = useContext(SessionContext)
  if (!me) throw new Error('useMe outside <Session>')
  return me
}

function Waiting({ text }: { text: string }) {
  return (
    <div className="login-wait">
      <Mark size={44} />
      <h1>Plinth</h1>
      <p className="muted" role="status">{text}</p>
    </div>
  )
}

function Problem({ message }: { message: string }) {
  return (
    <div className="login-wait">
      <Mark size={44} />
      <h1>Can't sign in</h1>
      <p>{message}</p>
      <button className="btn primary" onClick={() => (window.location.href = '/')}>Try again</button>
    </div>
  )
}

/**
 * Gate for the whole app: finishes the login on /callback, sends signed-out
 * visitors to the identity provider, and loads who the user is.
 */
export function Session({ children }: { children: ReactNode }) {
  const loc = useLocation()
  const navigate = useNavigate()
  const [me, setMe] = useState<Me | null>(null)
  const [problem, setProblem] = useState<string>()
  const onCallback = loc.pathname === '/callback'
  const signedOutPage = loc.pathname === '/signed-out'

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        if (onCallback) {
          const returnTo = await finishLogin()
          if (!cancelled) navigate(returnTo, { replace: true })
          return
        }
        if (signedOutPage) return
        if (!(await getToken())) {
          await startLogin(loc.pathname + loc.search)
          return
        }
        const user = await api<Me>('/me')
        if (!cancelled) setMe(user)
      } catch (e) {
        if (!cancelled) setProblem(messageOf(e))
      }
    })()
    return () => {
      cancelled = true
    }
    // The gate runs on navigation between the special pages and the app.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [onCallback, signedOutPage])

  if (problem) return <Problem message={problem} />
  if (signedOutPage) return <>{children}</>
  if (onCallback) return <Waiting text="Finishing sign-in…" />
  if (!me) return <Waiting text="Signing you in…" />
  return <SessionContext.Provider value={me}>{children}</SessionContext.Provider>
}
