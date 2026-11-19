// Shapes returned by the Plinth API (see docs/api.md).

export type Role = 'admin' | 'user'

export interface Me {
  id: string
  email: string
  role: Role
  /** True when Plinth can send e-mail, so invitations are mailed. */
  notify_email?: boolean
  /** "local" for password accounts, "oidc" for people signed in through an identity provider. */
  auth_source?: 'local' | 'oidc'
  /** A password somebody else chose has to be replaced before anything else. */
  must_change_password?: boolean
}

export interface SignInProvider {
  id: string
  name: string
  display_name: string
  issuer: string
  client_id: string
  scopes: string
}

/** What the sign-in page offers: GET /api/v1/config. */
export interface SignInOptions {
  local_login: boolean
  providers: SignInProvider[]
}

export interface LocalAccount {
  id: string
  username: string
  email: string
  role: Role
  disabled: boolean
  must_change_password: boolean
  locked_until?: string
  last_login?: string
  created_at: string
  /** Shown once, when Plinth chose the password. */
  temporary_password?: string
}

export interface OidcProvider {
  id: string
  name: string
  display_name: string
  issuer: string
  client_id: string
  scopes: string
  groups_claim: string
  admin_group: string
  trust_unverified_email: boolean
  enabled: boolean
  read_only: boolean
}

export interface AuthSettings {
  local_login_enabled: boolean
  /** PLINTH_FORCE_LOCAL_LOGIN keeps password sign-in on whatever is set. */
  forced: boolean
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

export type RequestStatus = 'awaiting_approval' | 'pending' | 'running' | 'succeeded' | 'failed' | 'rejected' | 'cancelled' | 'abandoned'

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
  decided_by?: string
  decided_at?: string
  decision_note?: string
  created_at: string
  updated_at: string
}

export interface BlueprintSummary {
  name: string
  environments: string[]
  has_database: boolean
  database_types: string[]
  has_registry: boolean
  requires_approval: boolean
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
  requires_approval?: boolean
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

export interface Resources {
  cpu: string
  memory: string
}

export interface NotificationEntry {
  id: number
  channel: 'email' | 'webhook'
  recipient?: string
  event: string
  state: 'waiting' | 'sent' | 'failed'
  attempts: number
  last_error?: string
  created_at: string
}

export interface NotificationStatus {
  email: boolean
  webhook: boolean
  recent: NotificationEntry[]
}

export interface QuotaTier {
  pods: number
  requests: Resources
  limits: Resources
  limitRange: { min: Resources; max: Resources; default: Resources; defaultRequest: Resources }
}

export type TeamRole = 'owner' | 'member'

export interface TeamMember {
  user_id: string
  email: string
  role: TeamRole
  added_by?: string
  added_at: string
}

export interface Invitation {
  id: string
  email: string
  role: TeamRole
  invited_by?: string
  created_at: string
  expires_at: string
}

export interface Team {
  name: string
  /** The viewer's role. Empty for an administrator who is not in the team. */
  role: TeamRole | ''
  member_count: number
  members?: TeamMember[]
  /** Open invitations. Only owners and administrators receive them. */
  invitations?: Invitation[]
  projects: string[]
  created_at: string
}
