import type { IntegrationKind } from '../types'

/** A setting an administrator enters for an integration. */
export interface Field {
  key: string
  label: string
  help?: string
  placeholder?: string
  required?: boolean
  /** Never shown after saving, and typed into a masked input. */
  secret?: boolean
  multiline?: boolean
}

const adopt: Field = {
  key: 'adopt_existing',
  label: 'Take over existing objects',
  help: 'Type true once when migrating teams that were set up before Plinth. Leave empty otherwise.',
  placeholder: 'true',
}

const caCert: Field = {
  key: 'ca_cert',
  label: 'CA certificate',
  help: 'PEM text. Only needed when the system uses a private certificate authority.',
  secret: false,
  multiline: true,
}

/** Known settings for the drivers that ship with Plinth. */
export const driverFields: Record<string, Field[]> = {
  kubernetes: [
    { key: 'server', label: 'API server URL', placeholder: 'https://k8s.example.com:6443', required: true },
    { ...caCert, required: true, help: 'PEM text of the cluster CA. TLS verification cannot be turned off.' },
    { key: 'token', label: 'Service account token', secret: true, required: true, help: 'Token of the "plinth" service account. Use a short-lived one.' },
    adopt,
  ],
  postgres: [
    { key: 'host', label: 'Host', placeholder: 'db.example.com', required: true },
    { key: 'port', label: 'Port', placeholder: '5432' },
    { key: 'admin_user', label: 'Admin user', required: true, help: 'Needs CREATEDB and CREATEROLE.' },
    { key: 'admin_password', label: 'Admin password', secret: true, required: true },
    { key: 'admin_database', label: 'Maintenance database', placeholder: 'postgres' },
    { key: 'sslmode', label: 'TLS mode', placeholder: 'verify-full', help: 'verify-full (default), require or disable.' },
    caCert,
    adopt,
  ],
  gitea: [
    { key: 'base_url', label: 'URL', placeholder: 'https://git.example.com', required: true },
    { key: 'token', label: 'Access token', secret: true, required: true, help: 'Needs permission to create organizations and repositories.' },
    caCert,
    adopt,
  ],
  harbor: [
    { key: 'base_url', label: 'URL', placeholder: 'https://harbor.example.com', required: true },
    { key: 'username', label: 'Username', required: true, help: 'A dedicated account, for example "plinth". Projects it creates are the ones Plinth manages.' },
    { key: 'password', label: 'Password', secret: true, required: true },
    { key: 'robot_name', label: 'Robot name', placeholder: 'ci' },
    { key: 'robot_ttl_days', label: 'Robot lifetime (days)', placeholder: '0 means never expires' },
    caCert,
    adopt,
  ],
}

export function fieldsFor(driver: string): Field[] {
  return driverFields[driver] ?? []
}

/** Names that look like secrets get a masked input even for unknown drivers. */
export function looksSecret(key: string): boolean {
  return /token|password|secret|passphrase|credential|api[_-]?key/i.test(key)
}

export const kindLabel: Record<IntegrationKind, string> = {
  gitsource: 'Git hosting',
  registry: 'Container registry',
  cluster: 'Kubernetes cluster',
  database: 'Database server',
}

export const kindOrder: IntegrationKind[] = ['gitsource', 'registry', 'cluster', 'database']

const driverNames: Record<string, string> = {
  gitea: 'Gitea',
  harbor: 'Harbor',
  kubernetes: 'Kubernetes',
  postgres: 'PostgreSQL',
  demo: 'Demo (simulated)',
}

export function driverTitle(driver: string): string {
  return driverNames[driver] ?? driver
}

/** How a database driver is named for developers in the request form. */
export function databaseTitle(driver: string): string {
  return driverTitle(driver)
}
