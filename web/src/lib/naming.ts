// Mirrors internal/naming so the form can show, before submitting, exactly
// which names a request will create. The server is the authority: it
// validates again, and these rules are checked against the same test vectors.

/** Lowercase, drop all whitespace, drop everything outside [a-z0-9_-]. */
export function normalize(s: string): string {
  return s.toLowerCase().replace(/\s+/g, '').replace(/[^a-z0-9_-]/g, '')
}

export const MAX_NAMESPACE = 63

/** A DNS-1123 label, which is what a Kubernetes namespace must be. */
export function isDnsLabel(s: string): boolean {
  return s.length <= MAX_NAMESPACE && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(s)
}

/** The name shared by the namespace and the registry project. */
export function namespaceOf(tenant: string, project: string): string {
  return `${normalize(tenant)}-${normalize(project)}`
}

/** The database name for one environment (same name for the database user). */
export function databaseOf(tenant: string, project: string, env: string): string {
  return normalize(`${normalize(tenant)}_${normalize(project)}_${env}`)
}

/** True for a plausible host name (no scheme, no port, no IP literal). */
export function isHostname(h: string): boolean {
  if (!h || h.length > 253 || /^\d+(\.\d+){3}$/.test(h)) return false
  return h.split('.').every((l) => l.length <= 63 && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(l))
}

/** Remove a leading scheme such as https:// (the server does the same). */
export function stripScheme(u: string): string {
  const t = u.trim()
  const i = t.indexOf('://')
  return i >= 0 ? t.slice(i + 3) : t
}
