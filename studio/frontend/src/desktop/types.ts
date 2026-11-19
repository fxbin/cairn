/**
 * [INPUT]: Studio Go 请求和响应合同。
 * [OUTPUT]: 传输 DTO 与权威实例状态形状。
 * [POS]: WebView 透传 Journal 和实例视图。
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 * Author: fxbin
 */

export interface ErrorBody {
  code: string;
  message: string;
  run_id?: string;
  operation_id?: string;
  fingerprint?: string;
  summary?: string;
}

export interface OKResult {
  ok: boolean;
}

export interface HealthResult {
  ok: boolean;
  app_name: string;
  version: string;
  go_version: string;
  user_root?: string;
}

export interface WorkspaceStatus {
  path: string;
  exists: boolean;
  is_workspace: boolean;
  has_agent_dir: boolean;
  has_config: boolean;
  git_root?: string;
  branch?: string;
  dirty: boolean;
  state_dir: string;
  config_path: string;
  config_loaded: boolean;
  config_error?: string;
  config_source?: string;
  config_fingerprint?: string;
  active_run_id?: string;
  active_run_note?: string;
}

export interface RecentProject {
  path: string;
  name: string;
  last_used: string;
  pinned: boolean;
  archived: boolean;
}

export interface RunHistoryView {
  run_id: string;
  session_id?: string;
  prev_run_id?: string;
  task: string;
  state: string;
  phase: string;
  created_at: string;
  updated_at: string;
  title: string;
  pinned: boolean;
  archived: boolean;
  waiting_approval: boolean;
  waiting_reconcile: boolean;
  /** 阶段展示词，由 Go 侧统一词表下发；前端只渲染。 */
  phase_label?: string;
}

export interface StartRunRequest {
  workspace: string;
  state_dir?: string;
  task: string;
  model?: string;
  session_id?: string;
  prev_run_id?: string;
  fork_source?: string;
  fork_source_run_id?: string;
  mode?: string;
  require_session_tail?: boolean;
}

export interface StartRunAck {
  run_id: string;
  session_id?: string;
  state: string;
}

export interface ResumeRequest {
  workspace: string;
  state_dir?: string;
  run_id: string;
  approve: boolean;
  fingerprint: string;
  reason?: string;
  model?: string;
  mode?: string;
}

export interface AbortRequest {
  workspace: string;
  state_dir?: string;
  run_id: string;
  reason: string;
}

export interface ResolveRequest {
  workspace: string;
  state_dir?: string;
  run_id: string;
  operation_id: string;
  verdict: string;
  note: string;
  evidence: string;
}

export interface CancelRequest {
  run_id: string;
  reason?: string;
}

export interface WorkspaceRequest {
  workspace: string;
  state_dir?: string;
  query?: string;
  include_archived?: boolean;
}

export interface InspectRequest {
  workspace: string;
  state_dir?: string;
  run_id: string;
  include_events?: boolean;
}

export interface RunStatusView {
  run_id: string;
  session_id?: string;
  state: string;
  phase?: string;
  waiting_approval: boolean;
  waiting_reconcile: boolean;
  response_preview?: string;
  error_code?: string;
  error_message?: string;
  /** 阶段与状态的展示词，由 Go 侧统一词表下发；前端只渲染。 */
  phase_label?: string;
  state_label?: string;
}

export interface DomainEvent {
  schema_version: number;
  event_id: string;
  run_id: string;
  workspace_hash: string;
  seq: number;
  occurred_at: string;
  type: string;
  turn_id?: string;
  operation_id?: string;
  attempt_id?: string;
  payload: unknown;
  prev_hash: string;
  event_hash: string;
}

export interface StreamEvent {
  workflow_id?: string;
  workflow?: WorkflowInstanceView;
  next_action?: string;
  workspace?: string;
  kind: string;
  run_id?: string;
  session_id?: string;
  seq?: number;
  occurred_at?: string;
  event?: DomainEvent;
  status?: RunStatusView;
  error?: ErrorBody;
}

export interface FileNode {
  name: string;
  relative: string;
  path: string;
  kind: string;
  size?: number;
  children?: FileNode[];
  reason?: string;
}

export interface FilePreview {
  relative: string;
  path: string;
  language?: string;
  size: number;
  truncated: boolean;
  binary: boolean;
  text?: string;
  reason?: string;
}

export interface ChangeEntry {
  relative: string;
  status: string;
  agent_owned: boolean;
  evidence?: string;
  additions?: number;
  deletions?: number;
}

export interface ChangeSet {
  root: string;
  branch?: string;
  base_branch?: string;
  entries: ChangeEntry[];
  added: number;
  deleted: number;
  modified: number;
  untracked: number;
  total_additions?: number;
  total_deletions?: number;
}

export interface QueueItem {
  input_id: string;
  state: string;
  target_run_id?: string;
  reason?: string;
  content_preview?: string;
}

export interface QueueSubmitRequest {
  workspace: string;
  state_dir?: string;
  content: string;
  input_id?: string;
}

export interface QueueActionRequest {
  model?: string;
  mode?: string;
  workspace: string;
  state_dir?: string;
  input_id: string;
  reason?: string;
}

export interface SessionNode {
  run_id: string;
  session_id?: string;
  prev_run_id?: string;
  task: string;
  state: string;
  phase?: string;
  children?: string[];
}

export interface SessionTreeView {
  nodes: Record<string, SessionNode>;
  roots: string[];
}

export interface EvidenceVerification {
  verification_seq?: number;
  verification_ok: boolean;
}

export interface EvidenceMetrics {
  turns: number;
  tool_calls: number;
  input_tokens: number;
  output_tokens: number;
  usage_estimated: boolean;
  context_compactions: number;
  wall_time_ms: number;
}

export interface EvidenceView {
  state: string;
  outcome: string;
  failure_class?: string;
  stop_reason?: string;
  trusted: boolean;
  verification: EvidenceVerification;
  metrics: EvidenceMetrics;
  /** 状态、结果与完成证据可信度的展示词，由 Go 侧统一词表下发；前端只渲染。 */
  state_label?: string;
  outcome_label?: string;
  trust_label?: string;
}

export interface RunSummary {
  // 与 app.RunSummary 对齐的宽松投影；未知字段在桥接层省略。
  [key: string]: unknown;
}

export interface RunDetail {
  run_id: string;
  session_id?: string;
  state: string;
  phase?: string;
  task?: string;
  summary: RunSummary;
  events?: DomainEvent[];
  /** 阶段与状态的展示词，由 Go 侧统一词表下发；前端只渲染。 */
  phase_label?: string;
  state_label?: string;
}

export interface SteeringRequest {
  run_id: string;
  input_id?: string;
  content: string;
}

/** Provider 探测回执保留非敏感身份绑定，供用户明确确认准入。 */
export interface ProviderProbeView {
 result: string;
 provider_kind?: string;
 model?: string;
 error?: string;
 evidence_fingerprint?: string;
 config_fingerprint?: string;
 request_profile_fingerprint?: string;
 tool_roundtrip?: string;
 usage_streamed: boolean;
}

/** 准入确认必须携带所见配置与证据，不能由模型或展示状态推导授权。 */
export interface ProviderAdmissionRequest {
 workspace: string;
 state_dir?: string;
 evidence_fingerprint: string;
 config_fingerprint: string;
 request_profile_fingerprint: string;
 reason: string;
 supersedes?: string;
}

/**
 * models.dev 模型目录查询与过滤条件。
 */
export interface ModelCatalogRequest {
  provider?: string;
  query?: string;
  tool_call_only?: boolean;
  reasoning_only?: boolean;
  refresh?: boolean;
  limit?: number;
}

/**
 * models.dev 目录中单个供应商的非敏感摘要投影；仅供预填参考。
 */
export interface ModelCatalogProviderView {
  id: string;
  name: string;
  api_base_url?: string;
  env_names?: string[];
  model_count: number;
}

/**
 * models.dev 目录中单个模型的元数据建议投影；目录建议不构成已验证能力。
 */
export interface ModelCatalogEntryView {
  provider: string;
  id: string;
  name: string;
  family?: string;
  tool_call: boolean;
  reasoning: boolean;
  temperature: boolean;
  context_tokens: number;
  output_tokens: number;
  input_usd_per_million: number;
  output_usd_per_million: number;
}

/**
 * models.dev 模型目录查询的完整返回结果。
 */
export interface ModelCatalogView {
  providers: ModelCatalogProviderView[];
  models: ModelCatalogEntryView[];
  fetched_at?: string;
  from_stale_cache: boolean;
  total_providers: number;
  total_models: number;
}

/**
 * 已保存命名 Provider 连接预设的非敏感投影。
 */
export interface ProviderPresetView {
  name: string;
  protocol: string;
  model: string;
  base_url: string;
  api_key_env: string;
  admission: string;
  active: boolean;
}

/**
 * 保存命名 Provider 连接预设的显式请求；仅携带环境变量标识符。
 */
export interface SaveProviderPresetRequest {
  workspace: string;
  name: string;
  base_url: string;
  model: string;
  api_key_env: string;
  protocol?: string;
  admission?: string;
  overwrite?: boolean;
  user_scope?: boolean;
}

/**
 * Provider 连接配置向导的显式输入；可选同步保存为命名预设。
 */
export interface ProviderSetupRequest {
  base_url: string;
  model: string;
  api_key_env: string;
  workspace: string;
  admission?: string;
  overwrite?: boolean;
  user_scope?: boolean;
  catalog_url?: string;
  catalog_cache?: string;
  save_preset?: string;
  overwrite_preset?: boolean;
}

/**
 * Provider 连接配置向导生成结果的非敏感投影。
 */
export interface ProviderSetupView {
  config_path: string;
  api_key_env: string;
  catalog_matched: boolean;
  probe_ran: boolean;
  notes?: string[];
}

export type WorkflowNodeKind = "execute" | "script" | "verify" | "accept";

export interface WorkflowStepSpec {
  task: string;
  id?: string;
  kind?: WorkflowNodeKind;
  model?: string;
  mode?: string;
  argv?: string[];
  inputs?: string[];
  max_attempts?: number;
  retry_from?: string;
}

export interface WorkflowDefinitionItem {
  id: string;
  workspace?: string;
  name: string;
  steps: WorkflowStepSpec[];
  max_steps?: number;
  revision?: number;
  legacy?: boolean;
}

export interface WorkflowAttemptView {
  number: number;
  started_seq: number;
  run_id?: string;
  state: string;
  error_class?: string;
  decision?: string;
  reason?: string;
  terminal?: { seq: number; event_hash: string };
  output?: { ref?: string };
}

export interface WorkflowInstanceStep {
  id: string;
  kind: string;
  state: string;
  run_id?: string;
  run_phase?: string;
  attempts?: WorkflowAttemptView[];
}

export interface WorkflowInstanceView {
  workflow_id: string;
  definition?: { id: string; revision: number };
  definition_name?: string;
  session_id?: string;
  sequence?: number;
  phase?: string;
  created_at: string;
  finished: boolean;
  finish_state?: string;
  finish_reason?: string;
  resume_index: number;
  paused?: boolean;
  retry_targets?: string[];
  pending_approval?: { run_id: string; operation_id: string; fingerprint: string; summary?: string };
  pending_reconcile?: { run_id: string; operation_id: string; tool_name: string; side_effect_class: string };
  steps: WorkflowInstanceStep[];
}

export type WorkflowAction = "advance" | "pause" | "resume" | "cancel" | "retry" | "approve" | "reject" | "reconcile";

export interface WorkflowControlRequest extends WorkspaceRequest {
  workflow_id: string;
  action: WorkflowAction;
  sequence: number;
  step_id?: string;
  run_id?: string;
  operation_id?: string;
  fingerprint?: string;
  reason?: string;
  verdict?: string;
  evidence?: string;
}

export interface WorkflowStartRequest extends WorkspaceRequest {
  definition_id: string;
  revision: number;
  dry_run?: boolean;
}

export interface WorkflowRunAck {
  workflow_id: string;
  state: string;
  run_ids?: string[];
  session_id?: string;
  status?: WorkflowInstanceView;
}

export interface WorkflowControlResult {
  view: WorkflowInstanceView;
  next_action?: string;
  error?: ErrorBody;
}
