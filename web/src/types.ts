// Shapes returned by the Plinth API (see docs/api.md).

export type Role = 'admin' | 'user'

export interface Me {
  id: string
  email: string
  role: Role
}

export type StepStatus = 'pending' | 'running' | 'succeeded' | 'failed'

export interface StepResult {
  status: StepStatus
  error?: string
  attempts: number
}

export interface RequestState {
  steps: Record<string, StepResult>
  outputs: Record<string, string>
}

export interface ProjectInput {
  name: string
  description?: string
  public?: boolean
  host_url?: string
  application_port?: number
  quota?: string
  use_db?: boolean
  db_type?: string
}

export interface RequestPayload {
  tenant: string
  projects: ProjectInput[]
}

export type RequestStatus = 'pending' | 'running' | 'succeeded' | 'failed'

export interface RequestRecord {
  id: string
  blueprint: string
  tenant: string
  requested_by: string
  payload: RequestPayload
  status: RequestStatus
  error?: string
  state: RequestState
  plan?: string[]
  created_at: string
  updated_at: string
}

export interface BlueprintSummary {
  name: string
  environments: string[]
  has_database: boolean
  database_types: string[]
  has_registry: boolean
}

export interface BlueprintEnv {
  name: string
  cluster: string
  database?: string
  access_prefix?: string
  ingress_namespace?: string
}

export interface Blueprint {
  name: string
  git_source: string
  registry?: string
  git_parent?: string
  template_repo?: string
  template_vars?: Record<string, string>
  extra_branches?: string[]
  environments: BlueprintEnv[]
  updated_at?: string
}

export type IntegrationKind = 'gitsource' | 'registry' | 'cluster' | 'database'

export interface Integration {
  id: string
  kind: IntegrationKind
  driver: string
  name: string
  config_keys: string[]
  created_at: string
  updated_at: string
}

export interface DriverInfo {
  kind: IntegrationKind
  driver: string
}

export interface AuditEntry {
  id: number
  at: string
  actor: string
  action: string
  target: string
  detail?: unknown
}

export interface QuotaTier {
  pods: number
  requests: { cpu: string; memory: string }
  limits: { cpu: string; memory: string }
}
