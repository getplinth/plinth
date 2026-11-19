import { useCallback, useState, type FormEvent } from 'react'
import { api } from '../api'
import { signOut } from '../auth'
import { Banner, ErrorBanner, Field, Mark } from '../components'
import { useAction } from '../hooks'

const MIN = 12

/**
 * Choose a new password. With `forced` it is the only thing the person can do:
 * the password was chosen by somebody else (the first administrator's, or one an
 * administrator set or reset).
 */
export function ChangePasswordPage({ forced = false }: { forced?: boolean }) {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [again, setAgain] = useState('')
  const [tried, setTried] = useState(false)
  const [done, setDone] = useState(false)

  const sendFn = useCallback(async () => {
    await api('/auth/password', { method: 'POST', body: { current_password: current, new_password: next } })
    setCurrent('')
    setNext('')
    setAgain('')
    setTried(false)
    if (forced) window.location.assign('/')
    else setDone(true)
  }, [current, next, forced])
  const send = useAction(sendFn)

  const problems = {
    current: !current ? 'Enter your current password.' : undefined,
    next: next.length < MIN ? `Use at least ${MIN} characters. A few unrelated words make a good password.` : next === current ? 'Choose a password different from the current one.' : undefined,
    again: again !== next ? 'The two passwords differ.' : undefined,
  }
  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setTried(true)
    setDone(false)
    if (problems.current || problems.next || problems.again) return
    await send.run()
  }
  const leave = async () => {
    await signOut()
    window.location.assign('/login')
  }

  const form = (
    <form className="panel" onSubmit={submit} noValidate>
      <h2>{forced ? 'Choose a new password' : 'Change your password'}</h2>
      {forced && <p>The password you signed in with was set by someone else. Choose your own to continue.</p>}
      <ErrorBanner message={send.error} details={send.details} />
      {done && <Banner kind="info" title="Password changed.">{' '}Your other sessions were signed out.</Banner>}
      <Field id="pw-current" label="Current password" error={tried ? problems.current : undefined}>
        <input id="pw-current" type="password" autoComplete="current-password" value={current} onChange={(e) => setCurrent(e.target.value)} />
      </Field>
      <Field id="pw-new" label="New password" error={tried ? problems.next : undefined} hint={`At least ${MIN} characters.`}>
        <input id="pw-new" type="password" autoComplete="new-password" value={next} onChange={(e) => setNext(e.target.value)} />
      </Field>
      <Field id="pw-again" label="New password again" error={tried ? problems.again : undefined}>
        <input id="pw-again" type="password" autoComplete="new-password" value={again} onChange={(e) => setAgain(e.target.value)} />
      </Field>
      <div className="actions">
        <button className="btn primary" disabled={send.busy}>{send.busy ? 'Saving…' : 'Change password'}</button>
        {forced && <button type="button" className="btn" onClick={() => void leave()}>Sign out</button>}
      </div>
    </form>
  )
  if (!forced) return <div className="narrow">{form}</div>
  return (
    <div className="login-card">
      <Mark size={44} />
      {form}
    </div>
  )
}
