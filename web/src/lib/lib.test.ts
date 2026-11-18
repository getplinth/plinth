import { describe, expect, it } from 'vitest'
import { databaseOf, isDnsLabel, isHostname, namespaceOf, normalize, stripScheme } from './naming'
import { emptyProject, hasErrors, toPayload, validate, type NewRequestForm } from './form'
import { buildSections, describeStep, orderedKeys, progress, statusLabel } from './steps'
import { listProjects, relativeTime } from './format'
import type { RequestRecord } from '../types'

describe('naming matches the Go implementation (same vectors as internal/naming)', () => {
  const vectors: Record<string, string> = {
    '  My App  ': 'myapp',
    'Sample_App-1': 'sample_app-1',
    'héllo wörld!': 'hllowrld',
    UPPER: 'upper',
    '!!!': '',
    'a\tb\nc': 'abc',
  }
  for (const [input, want] of Object.entries(vectors)) {
    it(`normalize(${JSON.stringify(input)})`, () => expect(normalize(input)).toBe(want))
  }

  it('derives the legacy names', () => {
    expect(namespaceOf('team-a', 'shop')).toBe('team-a-shop')
    expect(databaseOf('Team-A', 'Shop', 'DEV')).toBe('team-a_shop_dev')
  })

  it('accepts and rejects DNS labels like the server', () => {
    for (const ok of ['a', 'team-a', 'a1-b2']) expect(isDnsLabel(ok)).toBe(true)
    for (const bad of ['', '-a', 'a-', 'a_b', 'A', 'x'.repeat(64)]) expect(isDnsLabel(bad)).toBe(false)
  })

  it('checks host names', () => {
    expect(isHostname('shop.example.com')).toBe(true)
    for (const bad of ['', 'not a host', '10.0.0.1', 'a..b', '-x.com']) expect(isHostname(bad)).toBe(false)
    expect(stripScheme('https://shop.example.com')).toBe('shop.example.com')
    expect(stripScheme(' shop.example.com ')).toBe('shop.example.com')
  })
})

describe('request form', () => {
  const base = (): NewRequestForm => ({
    blueprint: 'standard',
    tenant: 'Team-A',
    projects: [{ ...emptyProject('silver', 'postgres'), name: 'shop' }],
  })

  it('accepts a minimal valid form', () => {
    expect(hasErrors(validate(base()))).toBe(false)
  })

  it('explains each problem next to its field', () => {
    const f = base()
    f.blueprint = ''
    f.tenant = ''
    f.projects = [
      { ...emptyProject(), name: 'a_b', host: 'not a host', port: '70000', useDb: true, dbType: '' },
      { ...emptyProject(), name: 'A_B'.toLowerCase() },
    ]
    const e = validate(f)
    expect(e.blueprint).toBeTruthy()
    expect(e.tenant).toBeTruthy()
    expect(e.projects[0]).toMatchObject({ name: expect.any(String), host: expect.any(String), port: expect.any(String), dbType: expect.any(String) })
    expect(hasErrors(e)).toBe(true)
  })

  it('catches duplicate projects after normalization', () => {
    const f = base()
    f.projects = [
      { ...emptyProject(), name: 'shop' },
      { ...emptyProject(), name: ' Shop ' },
    ]
    expect(validate(f).projects[1]?.name).toMatch(/Same project/)
  })

  it('rejects names that make the namespace too long', () => {
    const f = base()
    f.tenant = 'x'.repeat(40)
    f.projects = [{ ...emptyProject(), name: 'y'.repeat(30) }]
    expect(validate(f).projects[0]?.name).toMatch(/63/)
  })

  it('needs at least one project', () => {
    const f = base()
    f.projects = []
    expect(validate(f).general).toBeTruthy()
  })

  it('builds the API body and leaves out empty fields', () => {
    const f = base()
    f.projects = [
      { ...emptyProject('silver', 'postgres'), name: ' shop ', host: 'https://shop.example.com', port: '8080', useDb: true },
      { ...emptyProject(), name: 'blog', description: '  Notes  ', isPublic: true },
    ]
    expect(toPayload(f)).toEqual({
      blueprint: 'standard',
      tenant: 'Team-A',
      projects: [
        { name: 'shop', quota: 'silver', host_url: 'shop.example.com', application_port: 8080, use_db: true, db_type: 'postgres' },
        { name: 'blog', quota: 'bronze', description: 'Notes', public: true },
      ],
    })
  })
})

const record = (over: Partial<RequestRecord> = {}): RequestRecord => ({
  id: 'r1',
  blueprint: 'standard',
  tenant: 'team-a',
  requested_by: 'alice@example.test',
  payload: { tenant: 'team-a', projects: [{ name: 'shop' }, { name: 'blog' }] },
  status: 'running',
  state: { steps: {}, outputs: {} },
  plan: [
    'git/group',
    'git/repo/shop', 'git/vars/shop', 'registry/repo/shop', 'cluster/shop/dev', 'cluster/shop/prod', 'registry/credential/shop', 'database/shop/dev',
    'git/repo/blog', 'git/vars/blog',
  ],
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  ...over,
})

describe('steps', () => {
  it('describes keys in words', () => {
    expect(describeStep('git/group').label).toBe('Team group')
    expect(describeStep('cluster/shop/dev')).toMatchObject({ label: 'Namespace', project: 'shop', env: 'dev' })
    expect(describeStep('database/shop/dev')).toMatchObject({ label: 'Database', project: 'shop', env: 'dev' })
    expect(describeStep('registry/credential/shop')).toMatchObject({ label: 'Registry access', project: 'shop' })
    expect(describeStep('something/odd').label).toBe('something/odd')
  })

  it('groups into a team section and one per project, in plan order', () => {
    const s = buildSections(record())
    expect(s.map((x) => x.title)).toEqual(['Team', 'shop', 'blog'])
    expect(s[1]?.steps.map((x) => x.label)).toEqual(['Repository', 'Repository variables', 'Registry project', 'Namespace', 'Namespace', 'Registry access', 'Database'])
    expect(s[1]?.steps[3]?.env).toBe('dev')
  })

  it('shows steps that have not started as waiting, and as not run once the request ended', () => {
    const running = buildSections(record({ state: { steps: { 'git/group': { status: 'succeeded', attempts: 1 } }, outputs: {} } }))
    expect(running[0]?.steps[0]?.status).toBe('succeeded')
    expect(running[1]?.steps[0]?.status).toBe('waiting')

    const failed = buildSections(
      record({ status: 'failed', state: { steps: { 'git/group': { status: 'failed', attempts: 3, error: 'boom' } }, outputs: {} } }),
    )
    expect(failed[0]?.steps[0]).toMatchObject({ status: 'failed', attempts: 3, error: 'boom' })
    expect(failed[1]?.steps[0]?.status).toBe('not-run')
  })

  it('still orders steps sensibly when the server sent no plan', () => {
    const r = record({
      plan: undefined,
      state: {
        steps: {
          'database/shop/dev': { status: 'succeeded', attempts: 1 },
          'git/repo/blog': { status: 'succeeded', attempts: 1 },
          'git/group': { status: 'succeeded', attempts: 1 },
          'git/repo/shop': { status: 'succeeded', attempts: 1 },
        },
        outputs: {},
      },
    })
    expect(orderedKeys(r)).toEqual(['git/group', 'git/repo/shop', 'database/shop/dev', 'git/repo/blog'])
  })

  it('counts progress', () => {
    const r = record({
      state: {
        steps: {
          'git/group': { status: 'succeeded', attempts: 1 },
          'git/repo/shop': { status: 'succeeded', attempts: 1 },
          'git/vars/shop': { status: 'running', attempts: 1 },
          'registry/repo/shop': { status: 'failed', attempts: 2 },
        },
        outputs: {},
      },
    })
    expect(progress(r)).toEqual({ done: 2, total: 10, failed: 1, running: 1 })
  })
})

describe('format', () => {
  const now = Date.parse('2026-01-10T12:00:00Z')
  it('describes time plainly', () => {
    expect(relativeTime('2026-01-10T11:59:50Z', now)).toBe('just now')
    expect(relativeTime('2026-01-10T11:55:00Z', now)).toBe('5 min ago')
    expect(relativeTime('2026-01-10T09:00:00Z', now)).toBe('3 h ago')
    expect(relativeTime('2026-01-08T12:00:00Z', now)).toBe('2 d ago')
    expect(relativeTime('nonsense', now)).toBe('')
  })
  it('lists projects', () => {
    expect(listProjects([])).toBe('no projects')
    expect(listProjects(['a'])).toBe('a')
    expect(listProjects(['a', 'b'])).toBe('a and b')
    expect(listProjects(['a', 'b', 'c', 'd'])).toBe('a, b and 2 more')
  })
})

describe('steps for requests that need approval', () => {
  it('shows every planned step as waiting until approved', () => {
    const s = buildSections(record({ status: 'awaiting_approval' }))
    expect(s.flatMap((x) => x.steps).every((x) => x.status === 'waiting')).toBe(true)
  })
  it('shows nothing as run for rejected and withdrawn requests', () => {
    for (const status of ['rejected', 'cancelled'] as const) {
      const s = buildSections(record({ status }))
      expect(s.flatMap((x) => x.steps).every((x) => x.status === 'not-run')).toBe(true)
    }
  })
  it('uses plain words for the states', () => {
    expect(statusLabel.awaiting_approval).toBe('Needs approval')
    expect(statusLabel.rejected).toBe('Rejected')
    expect(statusLabel.cancelled).toBe('Withdrawn')
  })
})
