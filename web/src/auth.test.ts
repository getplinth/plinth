import { describe, expect, it } from 'vitest'

// auth.ts talks to the identity provider at import time only when used, so the
// pure helper can be tested directly.
import { safeReturnPath } from './auth'

describe('safeReturnPath', () => {
  it('keeps ordinary in-app paths', () => {
    for (const p of ['/', '/new', '/requests/abc?x=1', '/admin/audit']) expect(safeReturnPath(p)).toBe(p)
  })
  it('refuses anything that could leave the site', () => {
    for (const p of ['//evil.example', '/\\evil.example', 'https://evil.example', 'javascript:alert(1)', 'evil', '', '/a\\b', '/a\nb']) {
      expect(safeReturnPath(p)).toBe('/')
    }
    expect(safeReturnPath(undefined)).toBe('/')
    expect(safeReturnPath(42)).toBe('/')
  })
})
