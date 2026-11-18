import { useEffect, useState, type ReactNode } from 'react'
import type { RequestRecord } from './types'
import { sentence } from './lib/format'
import { buildSections, progress, statusLabel } from './lib/steps'

/** The logo: three courses of stone. */
export function Mark({ size = 26 }: { size?: number }) {
  return (
    <svg className="mark" width={size} height={size} viewBox="0 0 26 26" aria-hidden="true">
      <rect x="5" y="3" width="17" height="6" rx="1.5" />
      <rect x="2" y="10" width="20" height="6" rx="1.5" opacity="0.72" />
      <rect x="4" y="17" width="20" height="6" rx="1.5" opacity="0.46" />
    </svg>
  )
}

/**
 * Progress as five courses laid from the bottom: filled slabs are finished
 * work, a paler one is in progress, a red one is where it stopped.
 */
export function StackGlyph({ rec }: { rec: RequestRecord }) {
  const p = progress(rec)
  const total = Math.max(p.total, 1)
  let filled = rec.status === 'succeeded' ? 5 : Math.floor((p.done / total) * 5)
  if (rec.status !== 'succeeded' && p.done > 0 && filled === 0) filled = 1
  const slabs = [0, 1, 2, 3, 4].map((i) => {
    if (i < filled) return 'on'
    if (i === filled && rec.status === 'failed') return 'fail'
    if (i === filled && (rec.status === 'running' || p.running > 0)) return 'run'
    return ''
  })
  return (
    <svg className="glyph mark" width="22" height="22" viewBox="0 0 22 22" aria-hidden="true">
      {slabs.map((cls, i) => (
        <rect key={i} className={cls} x={i % 2 ? 2 : 0} y={18 - i * 4.4} width="19" height="3.4" rx="1" />
      ))}
    </svg>
  )
}

const pillClass: Record<string, string> = { succeeded: 'accent', failed: 'bad', running: 'accent', pending: '', awaiting_approval: 'warn', rejected: 'bad', cancelled: '' }

export function StatusPill({ status }: { status: string }) {
  return <span className={`chip ${pillClass[status] ?? ''}`}>{statusLabel[status] ?? status}</span>
}

export function Banner({ kind = 'error', title, children }: { kind?: 'error' | 'info' | 'warn'; title?: string; children?: ReactNode }) {
  return (
    <div className={`banner ${kind}`} role={kind === 'error' ? 'alert' : 'status'}>
      {title && <strong>{title}</strong>}
      {children}
    </div>
  )
}

/** A server error with the list of problems it reported. */
export function ErrorBanner({ message, details }: { message?: string; details?: string[] }) {
  if (!message) return null
  return (
    <Banner title={details && details.length ? `${sentence(message)}. Fix these and try again:` : sentence(message)}>
      {details && details.length > 0 && (
        <ul>
          {details.map((d) => (
            <li key={d}>{d}</li>
          ))}
        </ul>
      )}
    </Banner>
  )
}

export function Field({
  id,
  label,
  hint,
  error,
  optional,
  children,
}: {
  id: string
  label: string
  hint?: string
  error?: string
  optional?: boolean
  children: ReactNode
}) {
  return (
    <div className="field">
      <label htmlFor={id}>
        {label} {optional && <span className="opt">(optional)</span>}
      </label>
      {children}
      {hint && !error && (
        <p className="hint" id={`${id}-hint`}>
          {hint}
        </p>
      )}
      {error && (
        <p className="field-error" id={`${id}-err`}>
          {error}
        </p>
      )}
    </div>
  )
}

/** Describe an input for assistive technology: its hint or error and validity. */
export function describe(id: string, error?: string, hint?: string) {
  return {
    'aria-invalid': error ? true : undefined,
    'aria-describedby': error ? `${id}-err` : hint ? `${id}-hint` : undefined,
  } as const
}

/** A destructive button that needs a second click to confirm. */
export function ConfirmButton({ label, confirmLabel, onConfirm, disabled }: { label: string; confirmLabel: string; onConfirm: () => void; disabled?: boolean }) {
  const [armed, setArmed] = useState(false)
  useEffect(() => {
    if (!armed) return
    const t = setTimeout(() => setArmed(false), 4000)
    return () => clearTimeout(t)
  }, [armed])
  return (
    <button
      type="button"
      className={`btn sm danger ${armed ? 'confirm' : ''}`}
      disabled={disabled}
      onClick={() => {
        if (armed) {
          setArmed(false)
          onConfirm()
        } else setArmed(true)
      }}
    >
      {armed ? confirmLabel : label}
    </button>
  )
}

/** Every step of a request as stacked courses, grouped by team and project. */
export function Courses({ rec }: { rec: RequestRecord }) {
  const sections = buildSections(rec)
  return (
    <div aria-live="polite">
      {sections.map((s) => (
        <section className="team" key={s.title} aria-label={s.project ? `Project ${s.title}` : 'Team'}>
          <div className="team-head">
            <h2>{s.title}</h2>
            {s.project && <span className="mono">{rec.tenant}-{s.project}</span>}
          </div>
          <ol className="courses">
            {s.steps.map((st) => (
              <li className="course" data-status={st.status} key={st.key}>
                <div className="what">
                  <strong>{st.label}</strong>
                  {st.env && <span className="chip env">{st.env}</span>}
                  {st.attempts > 1 && <span className="muted">tried {st.attempts} times</span>}
                </div>
                <div className="state">{statusLabel[st.status]}</div>
                {st.error && (
                  <p className="why">
                    {st.error} <span className="muted">(step <code>{st.key}</code>)</span>
                  </p>
                )}
              </li>
            ))}
          </ol>
        </section>
      ))}
    </div>
  )
}
