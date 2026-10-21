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
  waiting_approval: boolean;
  waiting_reconcile: boolean;
}

export interface StartRunRequest {
  workspace: string;
  state_dir?: string;
  task: string;
  model?: string;
  session_id?: string;
  prev_run_id?: string;
  fork_source?: string;
  mode?: string;
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
  kind: string;
  run_id?: string;
  session_id?: string;
  seq?: number;
  occurred_at?: string;
  event?: DomainEvent;
  status?: RunStatusView;
  error?: ErrorBody;
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
}
