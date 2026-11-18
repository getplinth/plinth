/** "just now", "5 min ago", "3 h ago", "2 d ago", or a date for older items. */
export function relativeTime(iso: string, now: number = Date.now()): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return ''
  const s = Math.max(0, Math.round((now - t) / 1000))
  if (s < 45) return 'just now'
  const m = Math.round(s / 60)
  if (m < 60) return `${m} min ago`
  const h = Math.round(m / 60)
  if (h < 24) return `${h} h ago`
  const d = Math.round(h / 24)
  if (d < 14) return `${d} d ago`
  return new Date(t).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

export function fullTime(iso: string): string {
  const t = Date.parse(iso)
  return Number.isNaN(t) ? '' : new Date(t).toLocaleString()
}

/** Server messages are plain lowercase sentences; start them with a capital for display. */
export function sentence(s: string): string {
  return s ? s.charAt(0).toUpperCase() + s.slice(1) : s
}

/** "shop", "shop and blog", "shop, blog and 2 more" */
export function listProjects(names: string[], max = 2): string {
  if (names.length === 0) return 'no projects'
  if (names.length <= max) return names.length === 2 ? `${names[0]} and ${names[1]}` : names.join(', ')
  return `${names.slice(0, max).join(', ')} and ${names.length - max} more`
}
