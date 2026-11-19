import type { RequestRecord, StepStatus } from '../types'

/** What a step looks like on screen, including steps that have not started. */
export type ViewStatus = StepStatus | 'waiting' | 'not-run'

export interface StepView {
  key: string
  label: string
  env?: string
  status: ViewStatus
  attempts: number
  error?: string
}

export interface Section {
  /** "Team" for steps shared by the whole request, otherwise a project name. */
  title: string
  project?: string
  steps: StepView[]
}

interface Described {
  label: string
  project?: string
  env?: string
  rank: number
}

/**
 * Step keys look like "cluster/<project>/<env>". Turn them into words a
 * developer recognises. Unknown keys are shown as they are.
 */
export function describeStep(key: string): Described {
  const [kind, a, b, c] = key.split('/')
  switch (`${kind}/${a}`) {
    case 'git/group':
      return { label: 'Team group', rank: 0 }
    case 'git/repo':
      return { label: 'Repository', project: b, rank: 1 }
    case 'git/vars':
      return { label: 'Repository variables', project: b, rank: 2 }
    case 'registry/repo':
      return { label: 'Registry project', project: b, rank: 3 }
    case 'registry/credential':
      return { label: 'Registry access', project: b, rank: 5 }
  }
  if (kind === 'cluster') return { label: 'Namespace', project: a, env: b, rank: 4 }
  if (kind === 'database') return { label: 'Database', project: a, env: b, rank: 6 }
  void c
  return { label: key, rank: 99 }
}

/** Steps in execution order: the server's plan, plus anything it did not list. */
export function orderedKeys(rec: RequestRecord): string[] {
  const keys = [...(rec.plan ?? [])]
  const known = new Set(keys)
  const extra = Object.keys(rec.state.steps)
    .filter((k) => !known.has(k))
    .sort((x, y) => {
      const dx = describeStep(x)
      const dy = describeStep(y)
      const px = rec.payload.projects.findIndex((p) => p.name.toLowerCase() === dx.project)
      const py = rec.payload.projects.findIndex((p) => p.name.toLowerCase() === dy.project)
      return px - py || dx.rank - dy.rank || x.localeCompare(y)
    })
  return [...keys, ...extra]
}

function viewOf(rec: RequestRecord, key: string): StepView {
  const d = describeStep(key)
  const st = rec.state.steps[key]
  const open = rec.status === 'awaiting_approval' || rec.status === 'pending' || rec.status === 'running'
  const status: ViewStatus = st ? st.status : open ? 'waiting' : 'not-run'
  return { key, label: d.label, env: d.env, status, attempts: st?.attempts ?? 0, error: st?.error }
}

/** Group the steps into the team section and one section per project. */
export function buildSections(rec: RequestRecord): Section[] {
  const sections: Section[] = []
  const bucket = (project?: string): Section => {
    const title = project ?? 'Team'
    let s = sections.find((x) => x.project === project)
    if (!s) {
      s = { title, project, steps: [] }
      sections.push(s)
    }
    return s
  }
  for (const key of orderedKeys(rec)) bucket(describeStep(key).project).steps.push(viewOf(rec, key))
  return sections
}

export interface Progress {
  done: number
  total: number
  failed: number
  running: number
}

export function progress(rec: RequestRecord): Progress {
  const keys = orderedKeys(rec)
  const total = keys.length || Object.keys(rec.state.steps).length
  let done = 0
  let failed = 0
  let running = 0
  for (const k of keys) {
    const s = rec.state.steps[k]?.status
    if (s === 'succeeded') done++
    else if (s === 'failed') failed++
    else if (s === 'running') running++
  }
  return { done, total, failed, running }
}

export const statusLabel: Record<string, string> = {
  awaiting_approval: 'Needs approval',
  rejected: 'Rejected',
  cancelled: 'Withdrawn',
  abandoned: 'Given up',
  pending: 'Queued',
  running: 'Running',
  succeeded: 'Done',
  failed: 'Failed',
  waiting: 'Waiting',
  'not-run': 'Not run',
}
