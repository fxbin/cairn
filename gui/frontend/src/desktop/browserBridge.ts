/**
 * 浏览器验收桥：在无 Wails WebView 时提供可点击验证的后端替身。
 * 仅用于开发/验收；桌面正式包走真实 bindings。
 */
import type {
  ChangeSet,
  EvidenceView,
  FileNode,
  HealthResult,
  OKResult,
  QueueItem,
  RecentProject,
  RunDetail,
  RunHistoryView,
  SessionTreeView,
  StartRunAck,
  StreamEvent,
  WorkspaceStatus,
} from "./types";

interface CallLog {
  method: string;
  args: unknown[];
}

const log: CallLog[] = [];
const handlers: Array<(event: StreamEvent) => void> = [];

const workspace: WorkspaceStatus = {
  path: "/Users/fxbin/Desktop/Project/AIProject/Cairn",
  exists: true,
  is_workspace: true,
  has_agent_dir: true,
  has_config: true,
  dirty: true,
  state_dir: "~/Library/Application Support/Cairn",
  config_path: "/Users/fxbin/Desktop/Project/AIProject/Cairn/.agent/config.yaml",
  config_loaded: true,
  branch: "main",
  git_root: "/Users/fxbin/Desktop/Project/AIProject/Cairn",
  config_fingerprint: "demo",
};

let runs: RunHistoryView[] = [
  {
    run_id: "run-demo-1",
    session_id: "s1",
    task: "修复登录问题",
    state: "completed",
    phase: "completed",
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    title: "修复登录问题",
    pinned: false,
    archived: false,
    waiting_approval: false,
    waiting_reconcile: false,
  },
];

const eventsByRun = new Map<string, RunDetail["events"]>([
  [
    "run-demo-1",
    [
      {
        schema_version: 1,
        event_id: "e1",
        run_id: "run-demo-1",
        workspace_hash: "w",
        seq: 1,
        occurred_at: new Date().toISOString(),
        type: "run_created",
        payload: { task: "修复登录问题" },
        prev_hash: "",
        event_hash: "",
      },
      {
        schema_version: 1,
        event_id: "e2",
        run_id: "run-demo-1",
        workspace_hash: "w",
        seq: 2,
        occurred_at: new Date().toISOString(),
        type: "assistant_message_appended",
        payload: { text: "我会先定位登录校验逻辑。" },
        prev_hash: "",
        event_hash: "",
      },
      {
        schema_version: 1,
        event_id: "e3",
        run_id: "run-demo-1",
        workspace_hash: "w",
        seq: 3,
        occurred_at: new Date().toISOString(),
        type: "tool_call_proposed",
        operation_id: "op1",
        payload: { tool: "read_file", summary: "internal/auth/login.go" },
        prev_hash: "",
        event_hash: "",
      },
      {
        schema_version: 1,
        event_id: "e4",
        run_id: "run-demo-1",
        workspace_hash: "w",
        seq: 4,
        occurred_at: new Date().toISOString(),
        type: "operation_succeeded",
        operation_id: "op1",
        payload: {},
        prev_hash: "",
        event_hash: "",
      },
    ],
  ],
]);

function record(method: string, args: unknown[]) {
  log.push({ method, args });
  (window as unknown as { __CAIRN_CALLS__?: CallLog[] }).__CAIRN_CALLS__ = log;
}

function emit(event: StreamEvent) {
  for (const handler of handlers) {
    handler(event);
  }
}

function ok(): OKResult {
  return { ok: true };
}

export function createBrowserBridge() {
  return {
    Health: async (): Promise<HealthResult> => {
      record("Health", []);
      return { ok: true, app_name: "Cairn", version: "0.1.0-dev", go_version: "browser", user_root: "~/.cairn" };
    },
    ObserveWorkspace: async (path: string): Promise<WorkspaceStatus> => {
      record("ObserveWorkspace", [path]);
      return { ...workspace, path: path || workspace.path };
    },
    PickDirectory: async (title: string): Promise<string> => {
      record("PickDirectory", [title]);
      return workspace.path;
    },
    ListRecentProjects: async (includeArchived: boolean): Promise<RecentProject[]> => {
      record("ListRecentProjects", [includeArchived]);
      return [
        {
          path: workspace.path,
          name: "Cairn",
          last_used: new Date().toISOString(),
          pinned: false,
          archived: false,
        },
      ];
    },
    RememberProject: async (path: string) => {
      record("RememberProject", [path]);
      return ok();
    },
    ArchiveProject: async (path: string, archived: boolean) => {
      record("ArchiveProject", [path, archived]);
      return ok();
    },
    ListRuns: async (request: { query?: string }) => {
      record("ListRuns", [request]);
      const q = (request.query || "").toLowerCase();
      return q ? runs.filter((r) => r.title.toLowerCase().includes(q)) : runs;
    },
    RenameSession: async (sessionID: string, title: string) => {
      record("RenameSession", [sessionID, title]);
      runs = runs.map((r) => (r.session_id === sessionID ? { ...r, title } : r));
      return ok();
    },
    SetSessionFlags: async (sessionID: string, pinned: boolean, archived: boolean) => {
      record("SetSessionFlags", [sessionID, pinned, archived]);
      return ok();
    },
    InspectRun: async (request: { run_id: string; include_events?: boolean }): Promise<RunDetail> => {
      record("InspectRun", [request]);
      return {
        run_id: request.run_id,
        session_id: "s1",
        state: "completed",
        phase: "completed",
        task: "修复登录问题",
        summary: {},
        events: request.include_events ? eventsByRun.get(request.run_id) || [] : [],
      };
    },
    Evidence: async (): Promise<EvidenceView> => {
      record("Evidence", []);
      return {
        state: "completed",
        outcome: "completed",
        trusted: true,
        verification: { verification_seq: 2, verification_ok: true },
        metrics: {
          turns: 3,
          tool_calls: 1,
          input_tokens: 1200,
          output_tokens: 400,
          usage_estimated: false,
          context_compactions: 0,
          wall_time_ms: 3200,
        },
      };
    },
    EvidenceFor: async () => {
      record("EvidenceFor", []);
      return {};
    },
    StartRun: async (request: { task: string; mode?: string; model?: string }): Promise<StartRunAck> => {
      record("StartRun", [request]);
      const runId = `run-${Date.now()}`;
      const created = {
        schema_version: 1,
        event_id: "e-create",
        run_id: runId,
        workspace_hash: "w",
        seq: 1,
        occurred_at: new Date().toISOString(),
        type: "run_created",
        payload: { task: request.task },
        prev_hash: "",
        event_hash: "",
      };
      eventsByRun.set(runId, [created]);
      runs = [
        {
          run_id: runId,
          session_id: `s-${Date.now()}`,
          task: request.task,
          state: "running",
          phase: "running",
          created_at: new Date().toISOString(),
          updated_at: new Date().toISOString(),
          title: request.task.slice(0, 24),
          pinned: false,
          archived: false,
          waiting_approval: false,
          waiting_reconcile: false,
        },
        ...runs,
      ];
      // 模拟流式推进
      setTimeout(() => {
        emit({
          kind: "run.event",
          run_id: runId,
          seq: 2,
          event: {
            ...created,
            event_id: "e2",
            seq: 2,
            type: "assistant_message_appended",
            payload: { text: "开始处理任务：" + request.task },
          },
        });
        emit({
          kind: "run.event",
          run_id: runId,
          seq: 3,
          event: {
            ...created,
            event_id: "e3",
            seq: 3,
            type: "tool_call_proposed",
            operation_id: "op-new",
            payload: { tool: "list_dir", summary: "." },
          },
        });
      }, 200);
      setTimeout(() => {
        emit({
          kind: "run.finished",
          run_id: runId,
          status: {
            run_id: runId,
            state: "completed",
            waiting_approval: false,
            waiting_reconcile: false,
            response_preview: "已完成",
          },
        });
      }, 600);
      return { run_id: runId, session_id: runs[0].session_id, state: "running" };
    },
    ResumeRun: async (request: { run_id: string; approve: boolean; fingerprint: string }) => {
      record("ResumeRun", [request]);
      emit({
        kind: "run.event",
        run_id: request.run_id,
        seq: 99,
        event: {
          schema_version: 1,
          event_id: "ap-ok",
          run_id: request.run_id,
          workspace_hash: "w",
          seq: 99,
          occurred_at: new Date().toISOString(),
          type: request.approve ? "approval_granted" : "approval_denied",
          payload: {},
          prev_hash: "",
          event_hash: "",
        },
      });
      return { run_id: request.run_id, state: "resuming" };
    },
    AbortRun: async (request: { run_id: string }) => {
      record("AbortRun", [request]);
      return { run_id: request.run_id, state: "cancelled", waiting_approval: false, waiting_reconcile: false };
    },
    ResolveReconcile: async (request: unknown) => {
      record("ResolveReconcile", [request]);
      return { run_id: "x", state: "completed", waiting_approval: false, waiting_reconcile: false };
    },
    CancelRun: async (request: unknown) => {
      record("CancelRun", [request]);
      return ok();
    },
    ListQueue: async (): Promise<QueueItem[]> => {
      record("ListQueue", []);
      return [{ input_id: "q-1", state: "pending", content_preview: "示例后续任务" }];
    },
    SubmitQueue: async (request: { content: string }): Promise<QueueItem> => {
      record("SubmitQueue", [request]);
      return { input_id: `q-${Date.now()}`, state: "pending" };
    },
    ConfirmQueue: async (request: unknown) => {
      record("ConfirmQueue", [request]);
      return { run_id: "q-run", state: "running" };
    },
    CancelQueue: async (request: unknown) => {
      record("CancelQueue", [request]);
      return { input_id: "q-1", state: "cancelled" };
    },
    SessionTree: async (): Promise<SessionTreeView> => {
      record("SessionTree", []);
      return { nodes: {}, roots: [] };
    },
    Doctor: async () => {
      record("Doctor", []);
      return { ready: true, config_valid: true, preset: "default", profile: "default", config_fingerprint: "demo" };
    },
    MCPStatus: async () => {
      record("MCPStatus", []);
      return [{ name: "example", state: "declared" }];
    },
    ListSkills: async () => {
      record("ListSkills", []);
      return [{ name: "virtual-intelligent-dev-team", description: "交付编排", scope: "user", disabled: false, manual_only: false }];
    },
    ListFileTree: async (): Promise<FileNode> => {
      record("ListFileTree", []);
      return {
        name: "Cairn",
        relative: ".",
        path: workspace.path,
        kind: "dir",
        children: [
          { name: "AGENTS.md", relative: "AGENTS.md", path: workspace.path + "/AGENTS.md", kind: "file", size: 10 },
          {
            name: "internal",
            relative: "internal",
            path: workspace.path + "/internal",
            kind: "dir",
            children: [
              { name: "app.go", relative: "internal/app.go", path: workspace.path + "/internal/app.go", kind: "file", size: 20 },
            ],
          },
        ],
      };
    },
    PreviewFile: async () => {
      record("PreviewFile", []);
      return { relative: "AGENTS.md", path: "AGENTS.md", language: "markdown", size: 10, truncated: false, binary: false, text: "# Cairn" };
    },
    ListChanges: async (): Promise<ChangeSet> => {
      record("ListChanges", []);
      return {
        root: workspace.path,
        branch: "main",
        entries: [{ relative: "gui/frontend/src/App.tsx", status: "modified", agent_owned: false, evidence: "git_status_only" }],
        added: 0,
        deleted: 0,
        modified: 1,
        untracked: 0,
      };
    },
    DiffFile: async () => {
      record("DiffFile", []);
      return "--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new\n";
    },
    OpenPathInEditor: async () => {
      record("OpenPathInEditor", []);
      return ok();
    },
  };
}

/** isDemoBridgeActive 表示模拟桥是否已被显式安装。 */
export function isDemoBridgeActive(): boolean {
  return typeof window !== "undefined" && Boolean((window as unknown as { __CAIRN_BRIDGE__?: unknown }).__CAIRN_BRIDGE__);
}

export function bindBrowserBridge() {
  const bridge = createBrowserBridge();
  const hook = (window as unknown as { __CAIRN_BRIDGE_HOOK__?: (b: unknown) => void }).__CAIRN_BRIDGE_HOOK__;
  if (hook) hook(bridge);
  (window as unknown as { __CAIRN_BRIDGE__?: unknown }).__CAIRN_BRIDGE__ = bridge;
  (window as unknown as { __CAIRN_SUBSCRIBE__?: (fn: (e: StreamEvent) => void) => () => void }).__CAIRN_SUBSCRIBE__ = (
    fn: (e: StreamEvent) => void,
  ) => {
    handlers.push(fn);
    return () => {
      const idx = handlers.indexOf(fn);
      if (idx >= 0) {
        handlers.splice(idx, 1);
      }
    };
  };
  return bridge;
}
