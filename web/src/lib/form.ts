import type { ProjectInput, RequestPayload } from '../types'
import { MAX_NAMESPACE, isDnsLabel, isHostname, namespaceOf, normalize, stripScheme } from './naming'

export interface ProjectForm {
  name: string
  description: string
  quota: string
  host: string
  port: string
  isPublic: boolean
  useDb: boolean
  dbType: string
}

export interface NewRequestForm {
  blueprint: string
  tenant: string
  projects: ProjectForm[]
}

export function emptyProject(quota = 'bronze', dbType = ''): ProjectForm {
  return { name: '', description: '', quota, host: '', port: '', isPublic: false, useDb: false, dbType }
}

export interface ProjectErrors {
  name?: string
  host?: string
  port?: string
  dbType?: string
}

export interface FormErrors {
  blueprint?: string
  tenant?: string
  projects: ProjectErrors[]
  general?: string
}

export function hasErrors(e: FormErrors): boolean {
  return !!(e.blueprint || e.tenant || e.general || e.projects.some((p) => Object.keys(p).length > 0))
}

const LETTERS = 'Use lowercase letters, numbers and dashes. Underscores and spaces are not allowed.'

/** Client-side checks that mirror the server's rules, to fail fast with a hint. */
export function validate(f: NewRequestForm): FormErrors {
  const errors: FormErrors = { projects: f.projects.map(() => ({})) }
  if (!f.blueprint) errors.blueprint = 'Choose a blueprint.'

  const tenant = normalize(f.tenant)
  if (!f.tenant.trim()) errors.tenant = 'Enter a team name.'
  else if (!isDnsLabel(tenant)) errors.tenant = LETTERS

  if (f.projects.length === 0) errors.general = 'Add at least one project.'

  const seen = new Map<string, number>()
  f.projects.forEach((p, i) => {
    const e = errors.projects[i]!
    const name = normalize(p.name)
    if (!p.name.trim()) e.name = 'Enter a project name.'
    else if (!isDnsLabel(name)) e.name = LETTERS
    else if (tenant && namespaceOf(tenant, name).length > MAX_NAMESPACE)
      e.name = `Too long: "${namespaceOf(tenant, name)}" must be ${MAX_NAMESPACE} characters or fewer.`
    else if (seen.has(name)) e.name = `Same project as number ${seen.get(name)! + 1}.`
    if (name) seen.set(name, seen.get(name) ?? i)

    const host = stripScheme(p.host)
    if (p.host.trim() && !isHostname(host)) e.host = 'Enter a host name such as shop.example.com.'

    if (p.port.trim()) {
      const n = Number(p.port)
      if (!Number.isInteger(n) || n < 1 || n > 65535) e.port = 'Enter a port from 1 to 65535.'
    }
    if (p.useDb && !p.dbType) e.dbType = 'Choose a database type.'
  })
  return errors
}

/** Build the API body. The server normalizes names again. */
export function toPayload(f: NewRequestForm): RequestPayload & { blueprint: string } {
  return {
    blueprint: f.blueprint,
    tenant: f.tenant.trim(),
    projects: f.projects.map((p): ProjectInput => {
      const out: ProjectInput = { name: p.name.trim(), quota: p.quota }
      if (p.description.trim()) out.description = p.description.trim()
      if (p.isPublic) out.public = true
      if (p.host.trim()) out.host_url = stripScheme(p.host)
      if (p.port.trim()) out.application_port = Number(p.port)
      if (p.useDb) {
        out.use_db = true
        out.db_type = p.dbType
      }
      return out
    }),
  }
}
