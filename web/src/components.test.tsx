import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Courses, ErrorBanner, StackGlyph } from './components'
import type { RequestRecord } from './types'

const failed: RequestRecord = {
  id: 'r1',
  blueprint: 'standard',
  tenant: 'payments',
  requested_by: 'alice@example.test',
  payload: { tenant: 'payments', projects: [{ name: 'checkout' }] },
  status: 'failed',
  state: {
    steps: {
      'git/group': { status: 'succeeded', attempts: 1 },
      'git/repo/checkout': { status: 'succeeded', attempts: 1 },
      'cluster/checkout/prod': { status: 'failed', attempts: 3, error: 'cluster unreachable' },
    },
    outputs: {},
  },
  plan: ['git/group', 'git/repo/checkout', 'cluster/checkout/prod', 'registry/credential/checkout'],
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

describe('Courses', () => {
  it('shows every planned step in words, grouped by project, with the reason for a failure', () => {
    render(<Courses rec={failed} />)
    const project = screen.getByRole('region', { name: 'Project checkout' })
    const steps = within(project).getAllByRole('listitem')
    expect(steps.map((s) => s.getAttribute('data-status'))).toEqual(['succeeded', 'failed', 'not-run'])
    expect(within(steps[1]!).getByText('Namespace')).toBeInTheDocument()
    expect(within(steps[1]!).getByText('prod')).toBeInTheDocument()
    expect(within(steps[1]!).getByText(/cluster unreachable/)).toBeInTheDocument()
    expect(within(steps[1]!).getByText(/tried 3 times/)).toBeInTheDocument()
    expect(within(steps[2]!).getByText('Not run')).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Team' })).toBeInTheDocument()
  })

  it('announces progress changes politely to assistive technology', () => {
    const { container } = render(<Courses rec={failed} />)
    expect(container.firstElementChild).toHaveAttribute('aria-live', 'polite')
  })
})

describe('StackGlyph', () => {
  it('fills the courses from the bottom and marks where it stopped', () => {
    const { container } = render(<StackGlyph rec={failed} />)
    const rects = [...container.querySelectorAll('rect')]
    expect(rects).toHaveLength(5)
    expect(rects.filter((r) => r.classList.contains('on')).length).toBe(2)
    expect(rects.filter((r) => r.classList.contains('fail')).length).toBe(1)
    // decorative: the status is also given in words next to it
    expect(container.querySelector('svg')).toHaveAttribute('aria-hidden', 'true')
  })

  it('is completely filled for a finished request', () => {
    const done: RequestRecord = { ...failed, status: 'succeeded' }
    const { container } = render(<StackGlyph rec={done} />)
    expect(container.querySelectorAll('rect.on')).toHaveLength(5)
  })
})

describe('ErrorBanner', () => {
  it('lists the problems the server reported and is announced as an alert', () => {
    render(<ErrorBanner message="validation failed" details={['project "a_b" must be a valid DNS label', 'unknown quota tier']} />)
    expect(screen.getByRole('alert')).toHaveTextContent('Fix these and try again')
    expect(screen.getAllByRole('listitem')).toHaveLength(2)
  })
  it('renders nothing without a message', () => {
    const { container } = render(<ErrorBanner />)
    expect(container).toBeEmptyDOMElement()
  })
})
