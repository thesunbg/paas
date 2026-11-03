// Typed client for the PaaS REST API (/api/v1). Field names mirror the Go JSON tags.

export type ServiceType = 'web' | 'worker' | 'static' | 'cron';
export type ServiceStatus = 'created' | 'deploying' | 'running' | 'sleeping' | 'stopped' | 'failed';
export type DeploymentStatus = 'queued' | 'building' | 'deploying' | 'active' | 'superseded' | 'failed' | 'cancelled';
export type DatabaseStatus = 'provisioning' | 'ready' | 'failed' | 'upgrading' | 'deleting' | 'importing';
export type Engine = 'postgres' | 'mysql' | 'mariadb' | 'redis' | 'mongo';
export type QoS = 'guaranteed' | 'burstable' | 'best-effort';
export type Role = 'owner' | 'admin' | 'developer' | 'viewer';

export interface User {
  id: string;
  email: string;
  name: string;
  is_admin: boolean;
  totp_enabled?: boolean;
  github_login?: string;
  created_at: string;
}

export interface UserSummary extends User {
  teams: number;
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
  /** Administrators only: the first-run setup wizard is neither finished nor dismissed. */
  setup_pending?: boolean;
}

export interface SetupStatus {
  needs_setup: boolean;
  allow_signup: boolean;
  base_domain: string;
  github_login?: boolean;
  /** Enabled single sign-on providers ("Sign in with <name>"). */
  sso?: { slug: string; name: string }[];
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
  installation_id?: number;
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

export interface Autoscale {
  enabled: boolean;
  min?: number;
  max?: number;
  target_cpu_percent?: number;
  target_rps?: number;
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
  volume_backup_hours?: number; // 0 = every 24 h, negative = off
  command?: string[];
  image?: string;
  spa?: boolean;
  env?: Record<string, string>;
  language?: string;
  dns_aliases?: string[];
  previews?: boolean;
  autoscale?: Autoscale;
  schedule?: string;
  job_timeout_sec?: number;
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
  version: number;
  node_id: string | null;
  current_deployment_id: string | null;
  parent_id?: string | null;
  preview_pr?: number | null;
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
  version?: string;
  mode: 'shared' | 'dedicated';
  server_service_id: string | null;
  db_name: string;
  username: string;
  size_preset: string;
  status: DatabaseStatus;
  error: string;
  backup_schedule_hours: number;
  last_backup_at: string | null;
  options?: DBOptions;
  options_version: number;
  created_at: string;
}

export interface DBOptions {
  public_port?: number;
  allowed_cidrs?: string[];
  quota_mb?: number;
  read_only?: boolean;
  pooler?: boolean;
  pooler_service_id?: string;
  pitr?: boolean;
  pitr_retention_days?: number;
  ha?: boolean;
  replica_service_id?: string;
}

export interface ExternalAccess {
  host: string;
  port: number;
  tls: boolean;
  url: string;
  allowed_cidrs: string[];
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

export type ScanSeverity = 'CRITICAL' | 'HIGH' | 'MEDIUM' | 'LOW' | 'UNKNOWN';
export type ScanMode = 'off' | 'warn' | 'block';

export interface ScanPolicy {
  mode: ScanMode;
  threshold: Exclude<ScanSeverity, 'UNKNOWN'>;
}

export interface ScanFinding {
  id: string;
  severity: ScanSeverity;
  pkg: string;
  installed: string;
  fixed?: string;
  title?: string;
  target?: string;
  url?: string;
}

/** Vulnerability scan of one deployment's image. */
export interface ImageScan {
  deployment_id: string;
  service_id: string;
  image: string;
  status: 'scanned' | 'unavailable';
  scanner: string;
  scanner_version: string;
  db_updated_at: string | null;
  os: string;
  critical: number;
  high: number;
  medium: number;
  low: number;
  unknown: number;
  findings: ScanFinding[];
  policy_mode: ScanMode;
  threshold: string;
  blocked: boolean;
  message: string;
  scanned_at: string;
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
  /** Provider of the repository's host ('' when unknown: the GitHub format is shown). */
  git_provider?: GitProvider | '';
  internal_host: string;
  recommendation: Recommendation | null;
  tuning_env: Record<string, string> | null;
  previews?: Service[];
  next_run?: string | null;
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
  volume_backup_hours?: number;
  command?: string[];
  spa?: boolean;
  env?: Record<string, string>;
  git_token?: string;
  domain?: string;
  auto_deploy?: boolean;
  deploy?: boolean;
  previews?: boolean;
  autoscale?: Autoscale;
  schedule?: string;
  job_timeout_sec?: number;
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

export interface Diagnosis {
  deployment_id: string;
  summary: string;
  likely_cause: string;
  suggested_fixes: string[];
  provider: string;
  model: string;
  created_at: string;
  cached: boolean;
}

export interface AISettings {
  enabled: boolean;
  provider: '' | 'anthropic' | 'openai';
  model: string;
  base_url: string;
  has_key: boolean;
  default_model: string;
}

export interface AIInput {
  provider: '' | 'anthropic' | 'openai';
  model: string;
  base_url: string;
  api_key: string;
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
  pooled_url?: string;
}

export interface DBUserInfo {
  username: string;
  role: 'readonly' | 'readwrite';
  password: string;
  url: string;
  created_at?: string;
}

export interface HAInfo {
  capable: boolean;
  enabled: boolean;
  alias?: string;
  primary?: { service: string; node: string; status: string };
  standby?: { service: string; node?: string; status: string };
  replication?: string;
}

export interface PITRInfo {
  capable: boolean;
  enabled: boolean;
  retention_days: number;
  earliest?: string;
  base_backups: Backup[] | null;
}

export interface DBMetricsResponse {
  points: { ts: string; size_bytes: number; connections: number }[];
  quota_mb: number;
  read_only: boolean;
  max_connections: number;
}

export interface DatabaseServer {
  service_id: string;
  status: string;
  memory_limit_mb: number;
  memory_usage_mb?: number;
  node_online?: boolean; // absent when the server has no node yet
}

export interface DatabaseDetail {
  database: Database;
  version?: string;
  version_status?: 'ok' | 'ending' | 'eol';
  version_eol?: string;
  server_version?: string;
  image_digest?: string; // platform manifest digest the server runs; absent when the node does not report it
  image_pinned?: boolean; // false: the server still follows the tag ("not pinned yet")
  connection?: Connection;
  external?: ExternalAccess;
  server?: DatabaseServer;
  links: DatabaseLink[];
  backup_health?: BackupHealth;
}

export interface BackupHealth {
  state: 'ok' | 'stale' | 'failing' | 'none' | 'off';
  last_success?: string;
  last_verified?: string;
  verify_ok?: boolean;
  verify_note?: string;
  offsite: boolean;
  message?: string;
}

export type SetupStatusLevel = 'ok' | 'warn' | 'fail';
export type SetupStepId = 'admin' | 'dns' | 'https' | 'ports' | 'github' | 'backups' | 'notifications' | 'first_app';

/** One step of the first-run setup checklist with the result of its check. */
export interface SetupStep {
  id: SetupStepId;
  title: string;
  howto: string;
  status: SetupStatusLevel;
  summary: string;
  details?: string[];
  records?: { name: string; type: string; value: string }[];
  info?: Record<string, unknown>;
  checked_at: string;
  skipped: boolean;
  completed: boolean;
}

export interface SetupProgress {
  dismissed: boolean;
  finished: boolean;
  completed: SetupStepId[];
  skipped: SetupStepId[];
  updated_at?: string;
  pending: boolean;
}

export interface SetupChecklist extends SetupProgress {
  steps: SetupStep[];
}

export interface StorageSettings {
  enabled: boolean;
  from_env: boolean;
  endpoint: string;
  region: string;
  bucket: string;
  access_key: string;
  has_secret: boolean;
  prefix: string;
  virtual_host: boolean;
}

export interface StorageInput {
  endpoint: string;
  region: string;
  bucket: string;
  access_key: string;
  secret_key: string;
  prefix: string;
  virtual_host: boolean;
}

export interface StateBackupRun {
  at: string;
  name?: string;
  size?: number;
  offsite: boolean;
  error?: string;
  duration?: string;
}

export interface StateBackups {
  last: StateBackupRun | null;
  backups: { name: string; local: boolean; offsite: boolean }[] | null;
  offsite_error: string;
  every: string;
}

export interface VolumeBackup {
  id: string;
  service_id: string;
  node_id: string | null;
  status: string;
  s3_key: string;
  size_bytes: number;
  error: string;
  created_at: string;
  finished_at: string | null;
}

export interface VolumeBackups {
  backups: VolumeBackup[] | null;
  offsite: boolean;
  every_hours: number;
  enabled: boolean;
  node_online: boolean;
}

export type VolumeMovePhase = 'pending' | 'stopping' | 'copying' | 'verifying' | 'switching' | 'starting' | 'done' | 'failed' | 'aborted';

/** A move of a service's volumes to another node (kind move), or a restore from a backup (kind restore). */
export interface VolumeMove {
  id: string;
  service_id: string;
  kind: 'move' | 'restore';
  from_node: string | null;
  to_node: string | null;
  phase: VolumeMovePhase;
  prev_status: string;
  abort_requested: boolean;
  backup_id: string | null;
  rpo_at: string | null; // restore: time of the backup used (later writes are lost)
  bytes: number;
  files: number;
  digest: string;
  retain_days: number;
  error: string;
  requested_by: string;
  created_at: string;
  updated_at: string;
  finished_at: string | null;
}

export interface VolumeMoveStatus {
  active: VolumeMove | null;
  history: VolumeMove[];
  movable: boolean;
  reason: string;
  node?: { id: string; name: string; online: boolean };
  targets: { id: string; name: string; memory_available_mb: number }[]; // empty unless team admin
}

export interface DrainResult {
  status: string;
  moving: number;
  pinned_stateful: string[];
  moving_stateful: string[];
}

export interface RecoverNodeResult {
  started: { service: string; id: string; move: string; node: string; rpo_at: string | null }[];
  skipped: { kind: string; name: string; id: string; reason: string }[];
}

export interface StuckItem {
  kind: 'service' | 'database' | 'shared-database';
  id: string;
  name: string;
  project: string;
  project_id: string;
  backup?: string; // newest off-site backup it can be recovered from
}

export interface CreateDatabaseInput {
  name: string;
  engine: Engine;
  version?: string;
  mode?: 'shared' | 'dedicated';
  size?: string;
  link_services?: string[];
  env_name?: string;
  backup_schedule_hours?: number;
}

export interface Backup {
  s3_key?: string;
  kind?: 'dump' | 'base';
  pinned?: boolean;
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
  net_rx_bps?: number; // physical interfaces, since the previous heartbeat
  net_tx_bps?: number;
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
  traffic?: TrafficSummary;
  ssh_host_key?: string; // accepted when it was installed over SSH
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

export interface JoinToken {
  token: string;
  expires_at: string;
  command: string;
}

export interface SSHHostKey {
  address: string;
  ips: string[] | null;
  key_type: string;
  fingerprint: string;
  host_key: string;
  control_plane: boolean;
}

export interface PreflightCheck {
  id: string;
  title: string;
  status: 'ok' | 'warn' | 'fail';
  detail: string;
}

export interface SSHInstallRequest {
  host: string;
  port: number;
  user: string;
  auth: 'key' | 'password';
  private_key?: string;
  passphrase?: string;
  password?: string;
  roles: string[];
  name?: string;
  fingerprint: string;
  reinstall?: boolean;
  preflight_only?: boolean;
  allow_control_plane?: boolean;
}

export interface SSHInstall {
  id: string;
  host: string;
  port: number;
  user: string;
  auth_method: 'key' | 'password';
  roles: string[];
  node_name: string;
  host_key: string;
  host_key_fingerprint: string;
  reinstall: boolean;
  preflight_only: boolean;
  status: 'running' | 'succeeded' | 'failed';
  step: 'connect' | 'preflight' | 'install' | 'enroll' | 'done';
  preflight: { checks?: PreflightCheck[]; hostname?: string; os?: string; arch?: string; memory_mb?: number; disk_free_mb?: number };
  error: string;
  node_id: string | null;
  server_name: string;
  created_by_email: string;
  created_at: string;
  finished_at: string | null;
  log_tail?: string[];
}

export interface Size {
  name: string;
  memory_limit_mb: number;
  cpu_limit_millis: number;
  memory_request_mb: number;
  cpu_request_millis: number;
}

export interface DatabaseSize {
  name: string;
  memory_mb: number;
  cpu_millis: number;
  max_connections: number;
}

export interface EngineInfo {
  name: string;
  image: string;
  port: number;
  shared: boolean;
  default_env: string;
  min_memory_mb: number;
  versions?: VersionInfo[];
  default_version?: string;
}

// status: ok | ending (EOL within 180 days) | eol. Redis "8" has no eol date.
export interface VersionInfo {
  name: string;
  eol?: string;
  status: 'ok' | 'ending' | 'eol';
}

export interface SizesResponse {
  sizes: Size[];
  database_sizes?: DatabaseSize[];
  engines?: Record<string, EngineInfo>;
}

export interface JobRun {
  id: string;
  service_id: string;
  node_id: string | null;
  container_id: string;
  status: 'running' | 'succeeded' | 'failed';
  exit_code: number | null;
  output: string;
  trigger: 'schedule' | 'manual' | string;
  started_at: string;
  finished_at: string | null;
}

export interface MetricPoint {
  ts: string;
  instances: number;
  memory_mb: number;
  cpu_millis: number;
  requests: number;
  errors_5xx: number;
  net_rx_bytes: number; // received during the minute
  net_tx_bytes: number; // sent during the minute
}

export interface TrafficSummary {
  daily: { day: string; rx_bytes: number; tx_bytes: number }[]; // last 30 days (UTC), oldest first
  rx_30d: number;
  tx_30d: number;
  rx_today: number;
  tx_today: number;
  since?: string; // first day with data, when less than 30
  tz: string;
}

export interface ServiceMetrics {
  points: MetricPoint[] | null;
  rps_5m: number;
  errors_5m: number;
  traffic?: TrafficSummary;
}

export interface NodeMetricPoint {
  ts: string;
  cpu_percent: number;
  memory_used_mb: number;
  memory_committed_mb: number;
  net_rx_bytes: number;
  net_tx_bytes: number;
}

export interface TemplateService {
  name: string;
  image: string;
  type: string;
  port?: number;
  env?: Record<string, string>;
  volumes?: Volume[];
  command?: string[];
  size?: string;
  health_path?: string;
  inputs?: EnvInput[];
}

export interface Template {
  id: string;
  title: string;
  description: string;
  category: string;
  website?: string;
  services: TemplateService[];
  databases?: { name: string; engine: string; mode?: string }[];
}

export interface BatchEstimate {
  per_copy_request_mb: number;
  per_copy_limit_mb: number;
  need_request_mb: number;
  need_limit_mb: number;
  free_schedulable_mb: number;
  free_memory_mb: number;
  fits: boolean;
  warning?: string;
}

export interface BatchItem {
  index: number;
  label: string;
  email?: string;
  password?: string;
  project_id: string | null;
  service_id: string | null;
  status: string;
  url?: string;
  error?: string;
}

export interface Batch {
  id: string;
  name: string;
  template: string;
  mode: 'instances' | 'accounts';
  project_id: string | null;
  status: 'creating' | 'ready' | 'deleting';
  created_at: string;
  items?: BatchItem[];
  counts: Record<string, number>;
}

export interface BatchInput {
  name: string;
  template: string;
  mode: 'instances' | 'accounts';
  count?: number;
  learners?: { name: string; email: string }[];
  team_id?: string;
  prefix?: string;
  env?: Record<string, Record<string, string>>;
  force?: boolean;
}

export interface InstallResponse {
  services: Service[] | null;
  databases?: Database[] | null;
  warnings?: string[] | null;
}

export type GitProvider = 'github' | 'gitlab' | 'gitea' | 'bitbucket';

/** A team's access token for one git host (the token itself is never returned). */
export interface GitCredential {
  id: string;
  team_id: string;
  provider: GitProvider;
  host: string;
  username: string;
  name: string;
  created_at: string;
  updated_at: string;
}

export interface GitCredentialInput {
  provider: GitProvider;
  host?: string;
  username?: string;
  token: string;
  name?: string;
}

export type ChannelKind = 'webhook' | 'slack' | 'discord' | 'telegram';

export interface Channel {
  id: string;
  team_id: string;
  name: string;
  kind: ChannelKind;
  events: string[];
  created_at: string;
}

export interface ChannelsResponse {
  channels: Channel[] | null;
  events: string[];
}

export type AlertMetric = 'cpu_percent' | 'memory_percent' | 'restarts' | 'error_rate' | 'instances';
export type AlertComparator = '>' | '>=' | '<' | '<=';

export interface AlertState {
  rule_id: string;
  service_id: string;
  service_name: string;
  state: 'ok' | 'pending' | 'firing';
  value: number | null;
  pending_since: string | null;
  firing_since: string | null;
  resolved_at: string | null;
  last_notified_at: string | null;
  notified: boolean;
  updated_at: string;
}

export interface AlertRule {
  id: string;
  project_id: string;
  service_id: string | null;
  name: string;
  metric: AlertMetric;
  comparator: AlertComparator;
  threshold: number;
  for_seconds: number;
  cooldown_seconds: number;
  channel_ids: string[];
  enabled: boolean;
  created_at: string;
  updated_at: string;
  states?: AlertState[];
}

export interface AlertsResponse {
  rules: AlertRule[];
  metrics: Record<AlertMetric, string>;
  comparators: AlertComparator[];
}

export interface AlertInput {
  name?: string;
  service_id?: string;
  metric?: AlertMetric;
  comparator?: AlertComparator;
  threshold?: number;
  for_seconds?: number;
  cooldown_seconds?: number;
  channel_ids?: string[];
  enabled?: boolean;
}

export interface UptimeStats {
  uptime_24h: number | null;
  uptime_7d: number | null;
  avg_latency_ms_24h: number | null;
  p95_latency_ms_24h: number | null;
  checks_24h: number;
}

export interface UptimeCheck {
  id: string;
  project_id: string;
  service_id: string | null;
  name: string;
  url: string;
  method: 'GET' | 'HEAD' | 'POST';
  expected_status: number;
  keyword: string;
  timeout_seconds: number;
  interval_seconds: number;
  failure_threshold: number;
  channel_ids: string[];
  enabled: boolean;
  state: 'unknown' | 'up' | 'down';
  consecutive_failures: number;
  last_checked_at: string | null;
  last_error: string;
  state_changed_at: string | null;
  cert_expires_at: string | null;
  created_at: string;
  updated_at: string;
  stats?: UptimeStats;
}

export interface UptimeResult {
  ts: string;
  ok: boolean;
  status_code: number;
  latency_ms: number;
  error?: string;
}

export interface UptimeInput {
  name?: string;
  service_id?: string;
  url?: string;
  method?: string;
  expected_status?: number;
  keyword?: string;
  timeout_seconds?: number;
  interval_seconds?: number;
  failure_threshold?: number;
  channel_ids?: string[];
  enabled?: boolean;
}

export interface TwoFactorSetup {
  secret: string;
  otpauth_url: string;
}

export interface AddMemberResponse {
  email: string;
  role: Role;
  temporary_password?: string;
}

/** Error carrying the HTTP status and the server's `{"error"}` message. */
export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public data: Record<string, unknown> | null = null,
  ) {
    super(message);
  }
}

/** Called on any 401 so the app can drop back to the login screen. */
let onUnauthorized: (() => void) | null = null;
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

const BASE = '/api/v1';

async function request<T>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
  const init: RequestInit = { method, credentials: 'same-origin', headers: { ...headers } };
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
      data && typeof data === 'object' && 'error' in data
        ? String((data as { error: unknown }).error)
        : res.status >= 502
          ? `The control plane is unavailable (HTTP ${res.status}); try again shortly`
          : `${res.statusText || 'Request failed'} (HTTP ${res.status})`;
    if (res.status === 401 && !path.startsWith('/auth/') && onUnauthorized) onUnauthorized();
    throw new ApiError(res.status, msg, data && typeof data === 'object' ? (data as Record<string, unknown>) : null);
  }
  return data as T;
}

const get = <T>(p: string) => request<T>('GET', p);
const post = <T>(p: string, b: unknown = {}, h?: Record<string, string>) => request<T>('POST', p, b, h);
const put = <T>(p: string, b: unknown) => request<T>('PUT', p, b);
const patch = <T>(p: string, b: unknown, h?: Record<string, string>) => request<T>('PATCH', p, b, h);

// If-Match header for a row version: the write is refused (412) if the row changed since it was read.
const ifMatch = (version?: number): Record<string, string> | undefined => (version === undefined ? undefined : { 'If-Match': `"${version}"` });
const del = <T = { status: string }>(p: string) => request<T>('DELETE', p);
const e = encodeURIComponent;

export interface GitHubAppStatus {
  configured: boolean;
  can_configure: boolean;
  base_url: string;
  id?: number;
  slug?: string;
  name?: string;
  html_url?: string;
  owner?: string;
}

export interface GitHubInstallation {
  installation_id: number;
  team_id: string;
  account_login: string;
  account_type: string;
  created_at: string;
}

export interface TeamGitHub {
  configured: boolean;
  app_slug?: string;
  install_url?: string;
  installations: GitHubInstallation[];
}

export interface GitHubRepo {
  id: number;
  full_name: string;
  private: boolean;
  default_branch: string;
  clone_url: string;
  html_url: string;
  description: string;
  language: string;
  pushed_at: string;
  installation_id: number;
  account: string;
}

export interface DetectInput {
  repo: string;
  branch?: string;
  root_dir?: string;
  git_token?: string;
  dockerfile?: string;
  installation_id?: number;
}

export interface DetectResult {
  language: string;
  framework: string;
  static: boolean;
  port: number;
  suggested_type: ServiceType;
  dockerfile: boolean;
  commit_sha: string;
  commit_message: string;
  summary: string;
  prisma?: { provider: string; url_env?: string; migrations: boolean };
  compose?: { path: string; services: string[] };
  compose_plan?: ComposePlan;
  env?: EnvHint[];
  env_file?: string;
  error?: string;
}

export interface EnvHint {
  key: string;
  kind: 'value' | 'generated' | 'required' | 'optional' | 'database' | 'url';
  value?: string;
  hint?: string;
}

export interface EnvInput {
  key: string;
  required: boolean;
  placeholder?: string;
  hint?: string;
  from?: string;
}

export interface ComposePlan {
  services: {
    name: string;
    public: boolean;
    port?: number;
    dockerfile?: string;
    image?: string;
    links?: Record<string, string>;
    depends_on?: string[];
    inputs: EnvInput[];
    env_count: number;
    taken?: boolean;
    suggested?: string;
    reached_by?: string[];
  }[];
  databases: {
    name: string;
    engine: string;
    version?: string;
    taken?: boolean;
    suggested?: string;
    existing?: { id: string; name: string; engine: string; version: string };
  }[];
  warnings: string[];
}

export interface StudioState {
  service_id: string;
  status: 'building' | 'starting' | 'ready' | 'failed';
  url?: string;
  deployment?: { id: string; status: string; error?: string };
}

export interface ImportRepoInput {
  repo: string;
  branch: string;
  installation_id?: number;
  git_token?: string;
  compose_path?: string;
  domain?: string;
  env?: Record<string, Record<string, string>>;
  rename?: Record<string, string>;
  existing?: Record<string, string>;
}

export interface TeamLimits {
  max_projects: number;
  max_services: number;
  max_databases: number;
  max_memory_mb: number;
  max_replicas: number;
  max_signups_per_ip_per_day: number;
  max_teams: number;
}

export interface SystemSettings {
  allow_signup: boolean;
  limits: TeamLimits;
  /** Default team quota (teams without an override). */
  quota: TeamQuota;
}

/** A team's budget; 0 = unlimited. Memory and CPU count what instances reserve (requests). */
export interface TeamQuota {
  max_projects: number;
  max_services: number;
  max_databases: number;
  max_memory_mb: number;
  max_memory_limit_mb: number;
  max_cpu_millis: number;
  max_disk_mb: number;
  max_previews: number;
  max_domains: number;
}

export interface TeamUsage {
  projects: number;
  services: number;
  databases: number;
  memory_mb: number;
  memory_limit_mb: number;
  cpu_millis: number;
  disk_mb: number;
  previews: number;
  domains: number;
}

export interface TeamQuotaStatus {
  quota: TeamQuota;
  default: TeamQuota;
  override: Partial<TeamQuota>;
  usage: TeamUsage;
}

export interface OIDCGroupRole {
  group: string;
  team_id: string;
  role: Exclude<Role, 'owner'>;
}

export interface OIDCProvider {
  id: string;
  slug: string;
  name: string;
  issuer: string;
  client_id: string;
  scopes: string[];
  enabled: boolean;
  trusted_domains: string[];
  allowed_domains: string[];
  enforce_domains: string[];
  auto_provision: boolean;
  default_team_id: string | null;
  default_role: Exclude<Role, 'owner'>;
  groups_claim: string;
  group_roles: OIDCGroupRole[];
  skip_totp: boolean;
  has_secret?: boolean;
  created_at: string;
}

/** Fields sent to create or update a provider; client_secret is write-only ("" keeps it). */
export type OIDCProviderInput = Partial<Omit<OIDCProvider, 'id' | 'has_secret' | 'created_at' | 'default_team_id'>> & {
  client_secret?: string;
  default_team_id?: string;
};

export interface OIDCIdentity {
  provider_id: string;
  provider_slug: string;
  provider_name: string;
  email: string;
  created_at: string;
}

export const api = {
  users: () => get<UserSummary[]>('/users'),
  createUser: (u: { email: string; name?: string; password?: string; is_admin?: boolean }) => post<{ user: User; password?: string }>('/users', u),
  updateUser: (id: string, u: { name?: string; is_admin?: boolean; password?: string; reset_password?: boolean; disable_2fa?: boolean }) =>
    patch<{ user: User; password?: string }>(`/users/${e(id)}`, u),
  deleteUser: (id: string) => del(`/users/${e(id)}`),
  unlinkMyGitHub: () => del('/me/github'),
  githubLogin2FA: (pending: string, code: string) => post<AuthResponse>('/auth/github/2fa', { pending, code }),
  /** Completes a GitHub or SSO sign-in that needs the two-factor code. */
  login2FA: (pending: string, code: string) => post<AuthResponse>('/auth/2fa', { pending, code }),
  myOIDC: () => get<OIDCIdentity[]>('/me/oidc'),
  unlinkMyOIDC: (slug: string) => del(`/me/oidc/${e(slug)}`),
  oidcProviders: () => get<{ providers: OIDCProvider[]; redirect_uri: string }>('/admin/oidc'),
  createOIDCProvider: (p: OIDCProviderInput) => post<{ provider: OIDCProvider }>('/admin/oidc', p),
  updateOIDCProvider: (id: string, p: OIDCProviderInput) => patch<{ provider: OIDCProvider }>(`/admin/oidc/${e(id)}`, p),
  deleteOIDCProvider: (id: string) => del(`/admin/oidc/${e(id)}`),
  systemSettings: () => get<SystemSettings>('/admin/settings'),
  putSystemSettings: (s: Partial<SystemSettings>) => put<SystemSettings>('/admin/settings', s),
  // GitHub App
  githubApp: () => get<GitHubAppStatus>('/github/app'),
  githubManifest: (organization?: string) => post<{ action_url: string; manifest: string }>('/github/app/manifest', organization ? { organization } : {}),
  deleteGitHubApp: () => del('/github/app'),
  teamGitHub: (team: string, ret?: string) => get<TeamGitHub>(`/teams/${e(team)}/github${ret ? '?return=' + e(ret) : ''}`),
  githubRepos: (team: string) => get<{ repos: GitHubRepo[]; errors: string[] | null }>(`/teams/${e(team)}/github/repos`),
  githubBranches: (team: string, installation: number, repo: string) =>
    get<{ branches: string[] }>(`/teams/${e(team)}/github/branches?installation_id=${installation}&repo=${e(repo)}`),
  unlinkGitHub: (team: string, installation: number) => del(`/teams/${e(team)}/github/installations/${installation}`),
  detect: (project: string, input: DetectInput) => post<DetectResult>(`/projects/${e(project)}/detect`, input),

  // Auth
  setup: () => get<SetupStatus>('/setup'),
  register: (email: string, password: string, name: string) => post<AuthResponse>('/auth/register', { email, password, name }),
  login: (email: string, password: string, code?: string) => post<AuthResponse>('/auth/login', code ? { email, password, code } : { email, password }),
  twoFactorSetup: () => post<TwoFactorSetup>('/me/2fa/setup'),
  twoFactorEnable: (code: string) => post<{ status: string }>('/me/2fa/enable', { code }),
  twoFactorDisable: (code: string, password: string) => post<{ status: string }>('/me/2fa/disable', { code, password }),
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
  teamQuota: (team: string) => get<TeamQuotaStatus>(`/teams/${e(team)}/quota`),
  /** Replaces the team's override (administrators); keys left out use the default. */
  putTeamQuota: (team: string, quota: Partial<TeamQuota>) => put<TeamQuotaStatus>(`/teams/${e(team)}/quota`, { quota }),
  channels: (team: string) => get<ChannelsResponse>(`/teams/${e(team)}/notifications`),
  createChannel: (team: string, input: { name: string; kind: ChannelKind; target: string; events: string[] }) =>
    post<Channel>(`/teams/${e(team)}/notifications`, input),
  deleteChannel: (team: string, id: string) => del(`/teams/${e(team)}/notifications/${e(id)}`),
  testChannel: (team: string, id: string) => post<{ status: string }>(`/teams/${e(team)}/notifications/${e(id)}/test`),
  gitCredentials: (team: string) => get<{ credentials: GitCredential[]; providers: GitProvider[] }>(`/teams/${e(team)}/git-credentials`),
  setGitCredential: (team: string, input: GitCredentialInput) => post<GitCredential>(`/teams/${e(team)}/git-credentials`, input),
  deleteGitCredential: (team: string, id: string) => del(`/teams/${e(team)}/git-credentials/${e(id)}`),
  projects: (team: string) => get<ProjectListItem[] | null>(`/teams/${e(team)}/projects`),
  createProject: (team: string, name: string) => post<Project>(`/teams/${e(team)}/projects`, { name }),

  // Projects
  project: (id: string) => get<ProjectDetail>(`/projects/${e(id)}`),
  deleteProject: (id: string) => del(`/projects/${e(id)}`),
  openStudio: (project: string, database_id?: string) => post<StudioState>(`/projects/${e(project)}/studio`, database_id ? { database_id } : {}),
  renameProject: (id: string, name: string) => patch<Project>(`/projects/${e(id)}`, { name }),
  createService: (project: string, input: ServiceInput) => post<CreateServiceResponse>(`/projects/${e(project)}/services`, input),
  createDatabase: (project: string, input: CreateDatabaseInput) => post<Database>(`/projects/${e(project)}/databases`, input),
  templates: () => get<Template[] | null>('/templates'),
  batches: () => get<Batch[]>('/batches'),
  batch: (id: string) => get<Batch>(`/batches/${e(id)}`),
  batchEstimate: (template: string, count: number) => get<BatchEstimate>(`/batches/estimate?template=${e(template)}&count=${count}`),
  createBatch: (input: BatchInput) => post<{ id: string; items: number; estimate: BatchEstimate }>('/batches', input),
  deleteBatch: (id: string) => del(`/batches/${e(id)}`),
  batchCSVURL: (id: string) => `${BASE}/batches/${e(id)}/csv`,
  installTemplate: (project: string, template: string, domain?: string, env?: Record<string, Record<string, string>>, name?: string) =>
    post<InstallResponse>(`/projects/${e(project)}/templates/${e(template)}`, { ...(domain ? { domain } : {}), ...(env ? { env } : {}), ...(name ? { name } : {}) }),
  importCompose: (project: string, compose: string) => post<InstallResponse>(`/projects/${e(project)}/compose`, { compose }),
  importRepo: (project: string, input: ImportRepoInput) => post<InstallResponse>(`/projects/${e(project)}/import-repo`, input),

  // Services
  service: (id: string) => get<ServiceDetail>(`/services/${e(id)}`),
  updateService: (id: string, input: ServiceInput, version?: number) => patch<UpdateServiceResponse>(`/services/${e(id)}`, input, ifMatch(version)),
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
  runJob: (id: string) => post<JobRun>(`/services/${e(id)}/run`),
  runs: (id: string) => get<JobRun[] | null>(`/services/${e(id)}/runs`),
  metrics: (id: string, hours: number) => get<ServiceMetrics>(`/services/${e(id)}/metrics?hours=${hours}`),

  // Alerts and uptime checks
  alerts: (project: string, service?: string) => get<AlertsResponse>(`/projects/${e(project)}/alerts${service ? '?service=' + e(service) : ''}`),
  createAlert: (project: string, input: AlertInput) => post<AlertRule>(`/projects/${e(project)}/alerts`, input),
  updateAlert: (id: string, input: AlertInput) => patch<AlertRule>(`/alerts/${e(id)}`, input),
  deleteAlert: (id: string) => del(`/alerts/${e(id)}`),
  uptimeChecks: (project: string, service?: string) =>
    get<{ checks: UptimeCheck[] }>(`/projects/${e(project)}/uptime${service ? '?service=' + e(service) : ''}`),
  uptimeCheck: (id: string, limit = 60) => get<{ check: UptimeCheck; results: UptimeResult[] }>(`/uptime/${e(id)}?limit=${limit}`),
  createUptime: (project: string, input: UptimeInput) => post<UptimeCheck>(`/projects/${e(project)}/uptime`, input),
  updateUptime: (id: string, input: UptimeInput) => patch<UptimeCheck>(`/uptime/${e(id)}`, input),
  deleteUptime: (id: string) => del(`/uptime/${e(id)}`),

  // Deployments
  deployment: (id: string) => get<DeploymentDetail>(`/deployments/${e(id)}`),
  deploymentLogs: (id: string, after = 0) => get<LogLine[]>(`/deployments/${e(id)}/logs?after=${after}`),
  deploymentLogStreamURL: (id: string) => `${BASE}/deployments/${e(id)}/logs/stream`,
  rollback: (id: string) => post<Deployment>(`/deployments/${e(id)}/rollback`),
  cancelDeployment: (id: string) => post<{ status: string }>(`/deployments/${e(id)}/cancel`),
  diagnosis: (id: string) => get<{ enabled: boolean; diagnosis: Diagnosis | null }>(`/deployments/${e(id)}/diagnosis`),
  diagnose: (id: string, refresh = false) => post<Diagnosis>(`/deployments/${e(id)}/diagnose`, { refresh }),
  deploymentScan: (id: string) => get<ImageScan>(`/deployments/${e(id)}/scan`),
  serviceScan: (id: string) => get<{ scan: ImageScan | null; policy: ScanPolicy }>(`/services/${e(id)}/scan`),
  setScanPolicy: (projectId: string, p: ScanPolicy) => put<ScanPolicy>(`/projects/${e(projectId)}/scan-policy`, p),

  // Databases
  database: (id: string) => get<DatabaseDetail>(`/databases/${e(id)}`),
  deleteDatabase: (id: string) => del(`/databases/${e(id)}`),
  linkDatabase: (id: string, service_id: string, env_name?: string, restart = true) =>
    post<{ status: string; env_name: string }>(`/databases/${e(id)}/links`, { service_id, env_name: env_name || undefined, restart }),
  unlinkDatabase: (id: string, service: string) => del(`/databases/${e(id)}/links/${e(service)}`),
  backups: (id: string) => get<Backup[] | null>(`/databases/${e(id)}/backups`),
  backupNow: (id: string) => post<Backup>(`/databases/${e(id)}/backups`),
  verifyBackup: (id: string) => post<Backup>(`/databases/${e(id)}/backups/verify`),
  recoverDatabase: (id: string, allowEmpty: boolean) => post<{ status: string }>(`/databases/${e(id)}/recover`, { allow_empty: allowEmpty }),
  volumeBackups: (id: string) => get<VolumeBackups>(`/services/${e(id)}/volume-backups`),
  volumeBackupNow: (id: string) => post<VolumeBackup>(`/services/${e(id)}/volume-backups`),
  recoverService: (id: string, allowEmpty: boolean, backupId?: string, node?: string) =>
    post<{ node: string; backup: VolumeBackup | null; move: VolumeMove; rpo_at: string | null }>(`/services/${e(id)}/recover`, {
      allow_empty: allowEmpty,
      backup_id: backupId,
      node,
    }),
  volumeMove: (id: string) => get<VolumeMoveStatus>(`/services/${e(id)}/move`),
  startVolumeMove: (id: string, node: string, retainDays?: number) =>
    post<VolumeMove>(`/services/${e(id)}/move`, { node, retain_days: retainDays, confirm_downtime: true }),
  abortVolumeMove: (id: string) => del(`/services/${e(id)}/move`),
  aiSettings: () => get<AISettings>('/admin/ai'),
  putAISettings: (input: AIInput) => put<AISettings>('/admin/ai', input),
  storage: () => get<StorageSettings>('/admin/storage'),
  putStorage: (input: StorageInput) => put<StorageSettings>('/admin/storage', input),
  setupChecklist: (refresh = false) => get<SetupChecklist>(`/admin/setup-checklist${refresh ? '?refresh=1' : ''}`),
  checkSetupStep: (step: SetupStepId) => post<SetupStep>(`/admin/setup-checklist/${e(step)}/check`),
  putSetupChecklist: (input: { dismissed?: boolean; finished?: boolean; completed?: SetupStepId[]; skipped?: SetupStepId[] }) =>
    put<SetupProgress>('/admin/setup-checklist', input),
  stateBackups: () => get<StateBackups>('/admin/state-backups'),
  stateBackupNow: () => post<StateBackupRun>('/admin/state-backups'),
  stateBackupURL: (name: string) => `${BASE}/admin/state-backups/${e(name)}`,
  restoreBackup: (id: string, backup: string) => post<{ status: string }>(`/databases/${e(id)}/backups/${e(backup)}/restore`),
  upgradeDatabase: (id: string, size?: string) => post<{ status: string }>(`/databases/${e(id)}/upgrade`, size ? { size } : {}),
  /** One-way major upgrade: stops the linked services until it is done (confirm_downtime). */
  upgradeVersion: (id: string, version: string) =>
    post<{ status: string }>(`/databases/${e(id)}/version`, { version, confirm_downtime: true }),
  updateMinor: (id: string, confirm_shared = false) => post<{ status: string }>(`/databases/${e(id)}/update-minor`, { confirm_shared }),
  retryDatabase: (id: string) => post<{ status: string }>(`/databases/${e(id)}/retry`),
  myIP: () => get<{ ip: string }>('/me/ip'),
  databaseMetrics: (id: string, hours = 24) => get<DBMetricsResponse>(`/databases/${e(id)}/metrics?hours=${hours}`),
  databaseQuota: (id: string, quota_mb: number) => put<{ quota_mb: number }>(`/databases/${e(id)}/quota`, { quota_mb }),
  resizeDatabase: (id: string, size: string) => put<{ status: string }>(`/databases/${e(id)}/size`, { size }),
  dbUsers: (id: string) => get<DBUserInfo[]>(`/databases/${e(id)}/users`),
  createDBUser: (id: string, name: string, role: 'readonly' | 'readwrite') => post<DBUserInfo>(`/databases/${e(id)}/users`, { name, role }),
  deleteDBUser: (id: string, user: string) => del(`/databases/${e(id)}/users/${e(user)}`),
  rotateDBUser: (id: string, user: string) => post<{ username: string; password: string }>(`/databases/${e(id)}/users/${e(user)}/password`),
  pitr: (id: string) => get<PITRInfo>(`/databases/${e(id)}/pitr`),
  setPITR: (id: string, enabled: boolean, retention_days: number) => post<{ enabled: boolean }>(`/databases/${e(id)}/pitr`, { enabled, retention_days }),
  pitrRestore: (id: string, target_time?: string, name?: string) => post<Database>(`/databases/${e(id)}/pitr/restore`, { target_time, name }),
  ha: (id: string) => get<HAInfo>(`/databases/${e(id)}/ha`),
  setHA: (id: string, enabled: boolean) => post<{ ha: boolean }>(`/databases/${e(id)}/ha`, { enabled }),
  switchover: (id: string) => post<{ status: string }>(`/databases/${e(id)}/ha/switchover`),
  importDatabase: (id: string, source_url: string) => post<{ status: string }>(`/databases/${e(id)}/import`, { source_url }),
  databasePooler: (id: string, enabled: boolean) => post<{ pooler: boolean }>(`/databases/${e(id)}/pooler`, { enabled }),
  databasePublic: (id: string, enabled: boolean, allowed_cidrs: string[] = [], version?: number) =>
    post<{ public_port: number; allowed_cidrs: string[] }>(`/databases/${e(id)}/public`, { enabled, allowed_cidrs }, ifMatch(version)),

  // Cluster
  nodes: () => get<NodesResponse>('/nodes'),
  updateNode: (id: string, input: NodeUpdate) => patch<ClusterNode>(`/nodes/${e(id)}`, input),
  deleteNode: (id: string, force = false) => del(`/nodes/${e(id)}${force ? '?force=true' : ''}`),
  nodeStuck: (id: string) => get<StuckItem[] | null>(`/nodes/${e(id)}/stuck`),
  drainNode: (id: string, moveVolumes: boolean) => post<DrainResult>(`/nodes/${e(id)}/drain`, { move_volumes: moveVolumes }),
  recoverNode: (id: string, allowEmpty: boolean, node?: string) =>
    post<RecoverNodeResult>(`/nodes/${e(id)}/recover`, { allow_empty: allowEmpty, node, confirm_data_loss: true }),
  nodeMetrics: (id: string) => get<NodeMetricPoint[] | null>(`/nodes/${e(id)}/metrics`),
  createJoinToken: (roles: string[]) => post<JoinToken>('/cluster/join-tokens', { roles }),
  sshHostKey: (host: string, port: number) => post<SSHHostKey>('/cluster/ssh-host-key', { host, port }),
  sshInstalls: () => get<SSHInstall[]>('/cluster/ssh-installs'),
  sshInstall: (id: string) => get<SSHInstall>(`/cluster/ssh-installs/${e(id)}`),
  createSSHInstall: (req: SSHInstallRequest) => post<SSHInstall>('/cluster/ssh-installs', req),
  cancelSSHInstall: (id: string) => post<{ status: string }>(`/cluster/ssh-installs/${e(id)}/cancel`),
  sshInstallLogStreamURL: (id: string) => `${BASE}/cluster/ssh-installs/${e(id)}/logs/stream`,
  sizes: () => get<SizesResponse>('/sizes'),
};

/** Fallback env var names when /sizes does not list engines. */
const fallbackEnv: Record<string, string> = { postgres: 'DATABASE_URL', mysql: 'DATABASE_URL', mariadb: 'DATABASE_URL', redis: 'REDIS_URL', mongo: 'MONGODB_URL' };

let sizesCache: Promise<SizesResponse> | null = null;
/** /sizes is static per server; fetch it once per page load. */
export function catalog(): Promise<SizesResponse> {
  if (!sizesCache) sizesCache = api.sizes().catch((err) => ((sizesCache = null), Promise.reject(err)));
  return sizesCache;
}

export function defaultEnvFor(engine: string, cat?: SizesResponse | null): string {
  return cat?.engines?.[engine]?.default_env || fallbackEnv[engine] || 'DATABASE_URL';
}

export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
