/**
 * 桥接层共享类型。与 internal/desktop DTO 字段名保持一致；
 * 运行事实来自 Go/Journal，此处仅描述传输结构。
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
}

export interface ChangeSet {
  root: string;
  branch?: string;
  entries: ChangeEntry[];
  added: number;
  deleted: number;
  modified: number;
  untracked: number;
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
