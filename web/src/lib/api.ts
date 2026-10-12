// Typed client for the PaaS REST API (/api/v1). Field names mirror the Go JSON tags.

export type ServiceType = 'web' | 'worker' | 'static';
export type ServiceStatus = 'created' | 'deploying' | 'running' | 'sleeping' | 'stopped' | 'failed';
export type DeploymentStatus = 'queued' | 'building' | 'deploying' | 'active' | 'superseded' | 'failed' | 'cancelled';
export type DatabaseStatus = 'provisioning' | 'ready' | 'failed' | 'upgrading' | 'deleting';
export type Engine = 'postgres' | 'mysql' | 'redis' | 'mongo';
export type QoS = 'guaranteed' | 'burstable' | 'best-effort';
export type Role = 'owner' | 'admin' | 'developer' | 'viewer';

export interface User {
  id: string;
  email: string;
  name: string;
  is_admin: boolean;
  created_at: string;
}

export interface Team {
  id: string;
  name: string;
  slug: string;
  role?: Role;
  created_at: string;
}

export interface Me {
  user: User;
  teams: Team[];
}

export interface SetupStatus {
  needs_setup: boolean;
  allow_signup: boolean;
  base_domain: string;
}

export interface AuthResponse {
  user: User;
  token: string;
}

export interface Overview {
  projects: number;
  services: number;
  running: number;
  sleeping: number;
  databases: number;
  nodes?: number;
}

export interface APIToken {
  id: string;
  name: string;
  last_used_at: string | null;
  created_at: string;
}

export interface Member {
  user_id: string;
  email: string;
  name: string;
  role: Role;
}

export interface Project {
  id: string;
  team_id: string;
  name: string;
  slug: string;
  net_index: number;
  created_at: string;
}

export interface ProjectListItem extends Project {
  services: number;
  databases: number;
  running: number;
}

export interface ServiceSource {
  kind?: 'git' | 'image';
  repo?: string;
  branch?: string;
  root_dir?: string;
  dockerfile?: string;
  image?: string;
  auto_deploy?: boolean;
}

export interface BuildConfig {
  builder?: string;
  install_cmd?: string;
  build_cmd?: string;
  start_cmd?: string;
  output_dir?: string;
}

export interface Resources {
  cpu_request_millis: number;
  cpu_limit_millis: number;
  memory_request_mb: number;
  memory_limit_mb: number;
}

export interface HealthCheck {
  type?: 'http' | 'tcp' | 'none' | '';
  path?: string;
  timeout_sec?: number;
}

export interface Volume {
  name: string;
  path: string;
}

export interface SleepPolicy {
  enabled: boolean;
  idle_minutes?: number;
}

export interface ServiceSpec {
  port?: number;
  replicas: number;
  resources: Resources;
  qos?: QoS;
  runtime_class?: 'standard' | 'sandboxed' | '';
  health_check: HealthCheck;
  sleep: SleepPolicy;
  volumes?: Volume[];
  command?: string[];
  image?: string;
  spa?: boolean;
  env?: Record<string, string>;
  language?: string;
  dns_aliases?: string[];
}

export interface Service {
  id: string;
  project_id: string;
  name: string;
  slug: string;
  type: ServiceType | 'database';
  source: ServiceSource;
  build: BuildConfig;
  spec: ServiceSpec;
  has_git_token: boolean;
  node_id: string | null;
  current_deployment_id: string | null;
  status: ServiceStatus;
  last_request_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface ProjectService extends Service {
  url?: string;
  instances: number;
  memory_mb: number;
}

export interface Database {
  id: string;
  project_id: string;
  name: string;
  engine: Engine;
  mode: 'shared' | 'dedicated';
  server_service_id: string | null;
  db_name: string;
  username: string;
  size_preset: string;
  status: DatabaseStatus;
  error: string;
  backup_schedule_hours: number;
  last_backup_at: string | null;
  created_at: string;
}

export interface ProjectDetail {
  project: Project;
  services: ProjectService[];
  databases: Database[];
}

export interface Domain {
  id: string;
  service_id: string;
  hostname: string;
  is_generated: boolean;
  created_at: string;
}

export interface Instance {
  id: string;
  service_id: string;
  deployment_id: string | null;
  node_id: string;
  container_id: string;
  ip: string;
  port: number;
  status: string;
  memory_request_mb: number;
  cpu_request_millis: number;
  memory_usage_mb: number;
  memory_peak_mb: number;
  cpu_usage_millis: number;
  restarts: number;
  created_at: string;
  updated_at: string;
  node_name?: string;
}

export interface Deployment {
  id: string;
  service_id: string;
  status: DeploymentStatus;
  trigger: string;
  commit_sha: string;
  commit_message: string;
  branch: string;
  image: string;
  static_path: string;
  build_node_id: string | null;
  detected: string;
  error: string;
  created_by: string | null;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface DatabaseLink {
  service_id: string;
  database_id: string;
  env_name: string;
}

export interface LinkedDatabase extends DatabaseLink {
  name: string;
  engine: Engine;
}

export interface Recommendation {
  current_limit_mb: number;
  peak_mb: number;
  suggested_limit_mb: number;
  suggested_request_mb: number;
  savings_mb: number;
  message: string;
}

export interface ServiceDetail {
  service: Service;
  project: Project;
  domains: Domain[];
  urls: string[];
  instances: Instance[];
  deployments: Deployment[];
  databases: LinkedDatabase[];
  webhook_url: string;
  webhook_secret: string;
  internal_host: string;
  recommendation: Recommendation | null;
  tuning_env: Record<string, string> | null;
}

export interface ServiceInput {
  name?: string;
  type?: ServiceType;
  source?: ServiceSource;
  build?: BuildConfig;
  size?: string;
  qos?: QoS;
  port?: number;
  replicas?: number;
  resources?: Resources;
  sleep?: SleepPolicy;
  health_check?: HealthCheck;
  runtime_class?: string;
  volumes?: Volume[];
  command?: string[];
  spa?: boolean;
  env?: Record<string, string>;
  git_token?: string;
  domain?: string;
  auto_deploy?: boolean;
  deploy?: boolean;
}

export interface CreateServiceResponse {
  service: Service;
  deployment?: Deployment;
}

export interface UpdateServiceResponse {
  service: Service;
  deployment?: Deployment;
}

export interface EnvResponse {
  vars: Record<string, string>;
  linked: Record<string, string>;
}

export interface PutEnvResponse {
  status: string;
  deployment?: Deployment;
}

export interface InstanceLogs {
  instance: string;
  lines: string[];
  error?: string;
}

export interface ExecResult {
  exit_code: number;
  stdout: string;
  stderr: string;
  stdout_bytes?: number;
  error?: string;
}

export interface LogLine {
  id: number;
  ts: string;
  line: string;
}

export interface DeploymentDetail {
  deployment: Deployment;
  service: Service;
}

export interface Connection {
  host: string;
  port: number;
  database: string;
  username: string;
  password: string;
  url: string;
}

export interface DatabaseServer {
  service_id: string;
  status: string;
  memory_limit_mb: number;
  memory_usage_mb?: number;
}

export interface DatabaseDetail {
  database: Database;
  connection?: Connection;
  server?: DatabaseServer;
  links: DatabaseLink[];
}

export interface CreateDatabaseInput {
  name: string;
  engine: Engine;
  mode?: 'shared' | 'dedicated';
  size?: string;
  link_services?: string[];
  env_name?: string;
  backup_schedule_hours?: number;
}

export interface Backup {
  id: string;
  database_id: string;
  node_id: string | null;
  status: string;
  path: string;
  size_bytes: number;
  error: string;
  created_at: string;
  finished_at: string | null;
}

export interface NodeUsage {
  memory_used_mb: number;
  memory_available_mb: number;
  cpu_percent: number;
  load1: number;
  psi_memory_some10: number;
  disk_used_gb: number;
  disk_total_gb: number;
}

export interface ClusterNode {
  id: string;
  name: string;
  address: string;
  roles: string[];
  labels: Record<string, string>;
  status: string;
  cpu_cores: number;
  memory_mb: number;
  reserved_mb: number;
  overcommit_memory: number;
  overcommit_cpu: number;
  capabilities: Record<string, string>;
  usage: NodeUsage;
  version: string;
  last_seen_at: string;
  created_at: string;
  committed_memory_mb: number;
  committed_cpu_millis: number;
  capacity_memory_mb: number;
  capacity_cpu_millis: number;
  instances: number;
  used_by_apps_mb: number;
  overcommit_ratio: number;
}

export interface NodesResponse {
  nodes: ClusterNode[];
  sleeping_services: number;
}

export interface NodeUpdate {
  status?: 'online' | 'cordoned';
  overcommit_memory?: number;
  overcommit_cpu?: number;
  reserved_mb?: number;
  labels?: Record<string, string>;
}

export interface JoinInfo {
  nats_url: string;
  token: string;
  command: string;
}

export interface Size {
  name: string;
  memory_limit_mb: number;
  cpu_limit_millis: number;
  memory_request_mb: number;
  cpu_request_millis: number;
}

export interface AddMemberResponse {
  user: User;
  temporary_password?: string;
}

/** Error carrying the HTTP status and the server's `{"error"}` message. */
export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

/** Called on any 401 so the app can drop back to the login screen. */
let onUnauthorized: (() => void) | null = null;
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

const BASE = '/api/v1';

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: 'same-origin', headers: {} };
  if (body !== undefined) {
    (init.headers as Record<string, string>)['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let res: Response;
  try {
    res = await fetch(BASE + path, init);
  } catch {
    throw new ApiError(0, 'network error: the server is unreachable');
  }
  const text = await res.text();
  let data: unknown = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = text;
    }
  }
  if (!res.ok) {
    const msg =
      data && typeof data === 'object' && 'error' in data ? String((data as { error: unknown }).error) : res.statusText || `HTTP ${res.status}`;
    if (res.status === 401 && !path.startsWith('/auth/') && onUnauthorized) onUnauthorized();
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

const get = <T>(p: string) => request<T>('GET', p);
const post = <T>(p: string, b: unknown = {}) => request<T>('POST', p, b);
const put = <T>(p: string, b: unknown) => request<T>('PUT', p, b);
const patch = <T>(p: string, b: unknown) => request<T>('PATCH', p, b);
const del = <T = { status: string }>(p: string) => request<T>('DELETE', p);
const e = encodeURIComponent;

export const api = {
  // Auth
  setup: () => get<SetupStatus>('/setup'),
  register: (email: string, password: string, name: string) => post<AuthResponse>('/auth/register', { email, password, name }),
  login: (email: string, password: string) => post<AuthResponse>('/auth/login', { email, password }),
  logout: () => post<{ status: string }>('/auth/logout'),
  me: () => get<Me>('/me'),
  changePassword: (current: string, next: string) => post<{ status: string }>('/me/password', { current, new: next }),
  tokens: () => get<APIToken[] | null>('/tokens'),
  createToken: (name: string) => post<{ token: string; info: APIToken }>('/tokens', { name }),
  deleteToken: (id: string) => del(`/tokens/${e(id)}`),
  overview: () => get<Overview>('/overview'),

  // Teams
  teams: () => get<Team[]>('/teams'),
  createTeam: (name: string) => post<Team>('/teams', { name }),
  members: (team: string) => get<Member[] | null>(`/teams/${e(team)}/members`),
  addMember: (team: string, email: string, role: Role) => post<AddMemberResponse>(`/teams/${e(team)}/members`, { email, role }),
  removeMember: (team: string, user: string) => del(`/teams/${e(team)}/members/${e(user)}`),
  projects: (team: string) => get<ProjectListItem[] | null>(`/teams/${e(team)}/projects`),
  createProject: (team: string, name: string) => post<Project>(`/teams/${e(team)}/projects`, { name }),

  // Projects
  project: (id: string) => get<ProjectDetail>(`/projects/${e(id)}`),
  deleteProject: (id: string) => del(`/projects/${e(id)}`),
  createService: (project: string, input: ServiceInput) => post<CreateServiceResponse>(`/projects/${e(project)}/services`, input),
  createDatabase: (project: string, input: CreateDatabaseInput) => post<Database>(`/projects/${e(project)}/databases`, input),

  // Services
  service: (id: string) => get<ServiceDetail>(`/services/${e(id)}`),
  updateService: (id: string, input: ServiceInput) => patch<UpdateServiceResponse>(`/services/${e(id)}`, input),
  deleteService: (id: string) => del(`/services/${e(id)}`),
  deploy: (id: string, commit_sha?: string) => post<Deployment>(`/services/${e(id)}/deploy`, commit_sha ? { commit_sha } : {}),
  restart: (id: string) => post<Deployment>(`/services/${e(id)}/restart`),
  stop: (id: string) => post<{ status: string }>(`/services/${e(id)}/stop`),
  start: (id: string) => post<{ status: string }>(`/services/${e(id)}/start`),
  scale: (id: string, replicas: number) => post<{ replicas: number }>(`/services/${e(id)}/scale`, { replicas }),
  deployments: (id: string) => get<Deployment[] | null>(`/services/${e(id)}/deployments`),
  env: (id: string) => get<EnvResponse>(`/services/${e(id)}/env`),
  putEnv: (id: string, vars: Record<string, string>, opts: { replace?: boolean; restart?: boolean } = {}) =>
    put<PutEnvResponse>(`/services/${e(id)}/env`, { vars, ...opts }),
  deleteEnv: (id: string, key: string) => del(`/services/${e(id)}/env/${e(key)}`),
  domains: (id: string) => get<Domain[] | null>(`/services/${e(id)}/domains`),
  addDomain: (id: string, hostname: string) => post<Domain>(`/services/${e(id)}/domains`, { hostname }),
  deleteDomain: (id: string, domain: string) => del(`/services/${e(id)}/domains/${e(domain)}`),
  logs: (id: string, tail = 300) => get<InstanceLogs[]>(`/services/${e(id)}/logs?tail=${tail}`),
  exec: (id: string, command: string) => post<ExecResult>(`/services/${e(id)}/exec`, { command }),
  serviceLogStreamURL: (id: string) => `${BASE}/services/${e(id)}/logs/stream`,

  // Deployments
  deployment: (id: string) => get<DeploymentDetail>(`/deployments/${e(id)}`),
  deploymentLogs: (id: string, after = 0) => get<LogLine[]>(`/deployments/${e(id)}/logs?after=${after}`),
  deploymentLogStreamURL: (id: string) => `${BASE}/deployments/${e(id)}/logs/stream`,
  rollback: (id: string) => post<Deployment>(`/deployments/${e(id)}/rollback`),
  cancelDeployment: (id: string) => post<{ status: string }>(`/deployments/${e(id)}/cancel`),

  // Databases
  database: (id: string) => get<DatabaseDetail>(`/databases/${e(id)}`),
  deleteDatabase: (id: string) => del(`/databases/${e(id)}`),
  linkDatabase: (id: string, service_id: string, env_name?: string, restart = true) =>
    post<{ status: string; env_name: string }>(`/databases/${e(id)}/links`, { service_id, env_name: env_name || undefined, restart }),
  unlinkDatabase: (id: string, service: string) => del(`/databases/${e(id)}/links/${e(service)}`),
  backups: (id: string) => get<Backup[] | null>(`/databases/${e(id)}/backups`),
  backupNow: (id: string) => post<Backup>(`/databases/${e(id)}/backups`),
  restoreBackup: (id: string, backup: string) => post<{ status: string }>(`/databases/${e(id)}/backups/${e(backup)}/restore`),
  upgradeDatabase: (id: string, size?: string) => post<{ status: string }>(`/databases/${e(id)}/upgrade`, size ? { size } : {}),
  retryDatabase: (id: string) => post<{ status: string }>(`/databases/${e(id)}/retry`),

  // Cluster
  nodes: () => get<NodesResponse>('/nodes'),
  updateNode: (id: string, input: NodeUpdate) => patch<ClusterNode>(`/nodes/${e(id)}`, input),
  deleteNode: (id: string) => del(`/nodes/${e(id)}`),
  joinInfo: () => get<JoinInfo>('/cluster/join'),
  sizes: () => get<{ sizes: Size[] }>('/sizes'),
};

/** Default env var injected into services linked to a database of this engine. */
export const defaultEnvName: Record<Engine, string> = {
  postgres: 'DATABASE_URL',
  mysql: 'DATABASE_URL',
  redis: 'REDIS_URL',
  mongo: 'MONGODB_URL',
};

/** Database sizing presets (internal/databases/engines.go; not exposed by the API). */
export const databaseSizes = [
  { name: 'nano', memory_mb: 128, cpu_millis: 250 },
  { name: 'micro', memory_mb: 256, cpu_millis: 500 },
  { name: 'small', memory_mb: 512, cpu_millis: 1000 },
  { name: 'medium', memory_mb: 1024, cpu_millis: 2000 },
  { name: 'large', memory_mb: 2048, cpu_millis: 4000 },
  { name: 'xlarge', memory_mb: 4096, cpu_millis: 8000 },
];

export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
