/**
 * 浏览器验收桥：在无 Wails WebView 时提供可点击验证的后端替身。
 * 仅用于开发/验收；桌面正式包走真实 bindings。
 * Author: fxbin
 */
import type {
  ChangeSet,
  DomainEvent,
  EvidenceView,
  FileNode,
  HealthResult,
  ModelCatalogEntryView,
  ModelCatalogProviderView,
  ModelCatalogRequest,
  ModelCatalogView,
  OKResult,
  ProviderAdmissionRequest,
  ProviderPresetView,
  ProviderProbeView,
  ProviderSetupRequest,
  ProviderSetupView,
  QueueItem,
  RecentProject,
  RunDetail,
  RunHistoryView,
  SaveProviderPresetRequest,
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

/**
 * 浏览器演示桥接：它代替 Go 桥接下发展示词，因此在这里复刻一份运行展示
 * 词表。真实桌面桥接的展示词由 Go 侧 contract 词表产出，前端只渲染。
 */
const DEMO_PHASE_LABELS: Record<string, string> = {
  created: "已创建",
  running: "执行中",
  cancelling: "收尾取消中",
  completed: "运行完成",
  failed: "运行失败",
  cancelled: "已取消",
  waiting_approval: "待审批",
  waiting_reconcile: "待对账",
};

const demoPhaseLabel = (phase: string): string => DEMO_PHASE_LABELS[phase] ?? "未知";

let runs: RunHistoryView[] = [
  {
    run_id: "run-demo-1",
    session_id: "s1",
    task: "修复登录问题",
    state: "completed",
    phase: "completed",
    phase_label: "运行完成",
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

/** 演示桥离线 models.dev 供应商摘要；仅包含非敏感环境变量名建议。 */
const DEMO_CATALOG_PROVIDERS: readonly ModelCatalogProviderView[] = [
  {
    id: "openai",
    name: "OpenAI",
    api_base_url: "https://api.openai.com/v1",
    env_names: ["OPENAI_API_KEY"],
    model_count: 3,
  },
  {
    id: "anthropic",
    name: "Anthropic",
    api_base_url: "https://api.anthropic.com",
    env_names: ["ANTHROPIC_API_KEY"],
    model_count: 2,
  },
  {
    id: "deepseek",
    name: "DeepSeek",
    api_base_url: "https://api.deepseek.com/v1",
    env_names: ["DEEPSEEK_API_KEY"],
    model_count: 2,
  },
  {
    id: "moonshotai",
    name: "Moonshot AI",
    api_base_url: "https://api.moonshot.cn/v1",
    env_names: ["MOONSHOT_API_KEY"],
    model_count: 2,
  },
  {
    id: "openrouter",
    name: "OpenRouter",
    api_base_url: "https://openrouter.ai/api/v1",
    env_names: ["OPENROUTER_API_KEY"],
    model_count: 2,
  },
  {
    id: "zhipuai",
    name: "Zhipu AI",
    api_base_url: "https://open.bigmodel.cn/api/paas/v4",
    env_names: ["ZHIPU_API_KEY"],
    model_count: 2,
  },
];

/** 演示桥离线 models.dev 模型建议条目。 */
const DEMO_CATALOG_MODELS: readonly ModelCatalogEntryView[] = [
  {
    provider: "anthropic",
    id: "claude-opus-4-20250514",
    name: "Claude Opus 4",
    family: "claude-4",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 200000,
    output_tokens: 32000,
    input_usd_per_million: 15,
    output_usd_per_million: 75,
  },
  {
    provider: "anthropic",
    id: "claude-sonnet-4-20250514",
    name: "Claude Sonnet 4",
    family: "claude-4",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 200000,
    output_tokens: 64000,
    input_usd_per_million: 3,
    output_usd_per_million: 15,
  },
  {
    provider: "deepseek",
    id: "deepseek-chat",
    name: "DeepSeek V3",
    family: "deepseek-v3",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 65536,
    output_tokens: 8192,
    input_usd_per_million: 0.27,
    output_usd_per_million: 1.1,
  },
  {
    provider: "deepseek",
    id: "deepseek-reasoner",
    name: "DeepSeek R1",
    family: "deepseek-r1",
    tool_call: true,
    reasoning: true,
    temperature: false,
    context_tokens: 65536,
    output_tokens: 32768,
    input_usd_per_million: 0.55,
    output_usd_per_million: 2.19,
  },
  {
    provider: "moonshotai",
    id: "kimi-k2-0711-preview",
    name: "Kimi K2",
    family: "kimi-k2",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 131072,
    output_tokens: 16384,
    input_usd_per_million: 0.6,
    output_usd_per_million: 2.5,
  },
  {
    provider: "moonshotai",
    id: "moonshot-v1-128k",
    name: "Moonshot v1 128K",
    family: "moonshot-v1",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 131072,
    output_tokens: 8192,
    input_usd_per_million: 0.8,
    output_usd_per_million: 0.8,
  },
  {
    provider: "openai",
    id: "gpt-4.1",
    name: "GPT-4.1",
    family: "gpt-4.1",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 1047576,
    output_tokens: 32768,
    input_usd_per_million: 2,
    output_usd_per_million: 8,
  },
  {
    provider: "openai",
    id: "gpt-4.1-mini",
    name: "GPT-4.1 mini",
    family: "gpt-4.1",
    tool_call: true,
    reasoning: false,
    temperature: true,
    context_tokens: 1047576,
    output_tokens: 32768,
    input_usd_per_million: 0.4,
    output_usd_per_million: 1.6,
  },
  {
    provider: "openai",
    id: "o4-mini",
    name: "o4-mini",
    family: "o4",
    tool_call: true,
    reasoning: true,
    temperature: false,
    context_tokens: 200000,
    output_tokens: 100000,
    input_usd_per_million: 1.1,
    output_usd_per_million: 4.4,
  },
  {
    provider: "openrouter",
    id: "anthropic/claude-sonnet-4",
    name: "Anthropic: Claude Sonnet 4",
    family: "claude-4",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 200000,
    output_tokens: 64000,
    input_usd_per_million: 3,
    output_usd_per_million: 15,
  },
  {
    provider: "openrouter",
    id: "deepseek/deepseek-r1",
    name: "DeepSeek: R1",
    family: "deepseek-r1",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 163840,
    output_tokens: 32768,
    input_usd_per_million: 0.55,
    output_usd_per_million: 2.19,
  },
  {
    provider: "zhipuai",
    id: "glm-4.5",
    name: "GLM-4.5",
    family: "glm-4.5",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 131072,
    output_tokens: 16384,
    input_usd_per_million: 0.6,
    output_usd_per_million: 2.2,
  },
  {
    provider: "zhipuai",
    id: "glm-4.5-air",
    name: "GLM-4.5-Air",
    family: "glm-4.5",
    tool_call: true,
    reasoning: true,
    temperature: true,
    context_tokens: 131072,
    output_tokens: 16384,
    input_usd_per_million: 0.2,
    output_usd_per_million: 1.1,
  },
];

let demoPresets: ProviderPresetView[] = [
  {
    name: "deepseek-main",
    protocol: "openai-chat-completions",
    model: "deepseek-chat",
    base_url: "https://api.deepseek.com/v1",
    api_key_env: "DEEPSEEK_API_KEY",
    admission: "required",
    active: true,
  },
];

/** 校验演示桥输入的大写环境变量名格式，拒绝疑似明文密钥。 */
function isValidDemoEnvName(value: string): boolean {
  return /^[A-Z_][A-Z0-9_]*$/.test(value);
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
        phase_label: "运行完成",
        state_label: "运行完成",
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
        state_label: "运行完成",
        outcome_label: "运行完成",
        trust_label: "可信",
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
          phase_label: demoPhaseLabel("running"),
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
      // 演示事件按 Journal 事实追加，保证 InspectRun 投影与流视图一致。
      const appendEvent = (event: DomainEvent) => {
        const current = eventsByRun.get(runId) || [];
        current.push(event);
        eventsByRun.set(runId, current);
        emit({ kind: "run.event", run_id: runId, seq: event.seq, event });
      };
      const patchRun = (patch: Partial<RunHistoryView>) => {
        runs = runs.map((r) => (r.run_id === runId ? { ...r, ...patch, updated_at: new Date().toISOString() } : r));
      };
      // 模拟流式推进
      setTimeout(() => {
        appendEvent({
          ...created,
          event_id: "e2",
          seq: 2,
          type: "assistant_message_appended",
          payload: { text: "开始处理任务：" + request.task },
        });
        appendEvent({
          ...created,
          event_id: "e3",
          seq: 3,
          type: "tool_call_proposed",
          operation_id: "op-new",
          payload: { tool: "list_dir", summary: "." },
        });
      }, 200);
      // 审批停点：真实闭环在等待人工批准时停在 waiting_approval，演示桥同样
      // 停在这里，直到 ResumeRun 给出裁决。
      setTimeout(() => {
        appendEvent({
          ...created,
          event_id: "e4",
          seq: 4,
          type: "approval_requested",
          operation_id: "op-write",
          payload: { fingerprint: `demo-fp-${runId}`, summary: "写入工作区文件 demo/out.txt" },
        });
        patchRun({ state: "running", phase: "waiting_approval", phase_label: demoPhaseLabel("waiting_approval"), waiting_approval: true });
        emit({
          kind: "run.waiting_approval",
          run_id: runId,
          status: {
            run_id: runId,
            state: "running",
            phase: "waiting_approval",
            phase_label: demoPhaseLabel("waiting_approval"),
            state_label: demoPhaseLabel("running"),
            waiting_approval: true,
            waiting_reconcile: false,
          },
        });
      }, 600);
      return { run_id: runId, session_id: runs[0].session_id, state: "running" };
    },
    ResumeRun: async (request: { run_id: string; approve: boolean; fingerprint: string }) => {
      record("ResumeRun", [request]);
      const runId = request.run_id;
      const base = {
        schema_version: 1,
        event_id: "ap-ok",
        run_id: runId,
        workspace_hash: "w",
        occurred_at: new Date().toISOString(),
        payload: {},
        prev_hash: "",
        event_hash: "",
      };
      const appendEvent = (event: DomainEvent) => {
        const current = eventsByRun.get(runId) || [];
        current.push(event);
        eventsByRun.set(runId, current);
        emit({ kind: "run.event", run_id: runId, seq: event.seq, event });
      };
      const patchRun = (patch: Partial<RunHistoryView>) => {
        runs = runs.map((r) => (r.run_id === runId ? { ...r, ...patch, updated_at: new Date().toISOString() } : r));
      };
      appendEvent({ ...base, event_id: "ap-ok", seq: 90, type: request.approve ? "approval_granted" : "approval_denied" });
      patchRun({ waiting_approval: false, phase: "running", phase_label: demoPhaseLabel("running") });
      if (request.approve) {
        setTimeout(() => {
          appendEvent({
            ...base,
            event_id: "op-ok",
            seq: 91,
            type: "operation_succeeded",
            operation_id: "op-write",
            payload: { summary: "demo/out.txt" },
          });
          appendEvent({
            ...base,
            event_id: "done",
            seq: 92,
            type: "run_completed",
            payload: { final_response: "演示任务已完成，产物写入 demo/out.txt" },
          });
          patchRun({ state: "completed", phase: "completed", phase_label: demoPhaseLabel("completed") });
          emit({
            kind: "run.finished",
            run_id: runId,
            status: {
              run_id: runId,
              state: "completed",
              phase: "completed",
              phase_label: demoPhaseLabel("completed"),
              state_label: demoPhaseLabel("completed"),
              waiting_approval: false,
              waiting_reconcile: false,
              response_preview: "演示任务已完成，产物写入 demo/out.txt",
            },
          });
        }, 500);
      } else {
        setTimeout(() => {
          appendEvent({
            ...base,
            event_id: "denied",
            seq: 91,
            type: "run_failed",
            payload: { error: "操作被拒绝", failure_class: "approval_denied" },
          });
          patchRun({ state: "failed", phase: "failed", phase_label: demoPhaseLabel("failed") });
          emit({
            kind: "run.finished",
            run_id: runId,
            status: {
              run_id: runId,
              state: "failed",
              phase: "failed",
              phase_label: demoPhaseLabel("failed"),
              state_label: demoPhaseLabel("failed"),
              waiting_approval: false,
              waiting_reconcile: false,
            },
          });
        }, 500);
      }
      return { run_id: runId, state: "resuming" };
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
        entries: [{ relative: "studio/frontend/src/App.tsx", status: "modified", agent_owned: false, evidence: "git_status_only" }],
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
    ListOpenLocations: async () => {
      record("ListOpenLocations", []);
      return [
        { id: "finder", label: "Finder", available: true },
        { id: "terminal", label: "Terminal", available: true },
        { id: "vscode", label: "VS Code", available: true },
        { id: "cursor", label: "Cursor", available: false },
        { id: "idea", label: "IntelliJ IDEA", available: false },
        { id: "ghostty", label: "Ghostty", available: true },
      ];
    },
    OpenInLocationWithTarget: async (_w: string, _r: string, target: string) => {
      record("OpenInLocationWithTarget", [target]);
      return { ok: true };
    },
    OpenPathInEditor: async () => {
      record("OpenPathInEditor", []);
      return ok();
    },
    ListModelCatalog: async (request: ModelCatalogRequest = {}): Promise<ModelCatalogView> => {
      record("ListModelCatalog", [request]);
      const rawLimit = request.limit ?? 200;
      const limit = rawLimit <= 0 ? 200 : Math.min(rawLimit, 500);
      const providerFilter = (request.provider || "").trim();
      const query = (request.query || "").trim().toLowerCase();
      const filtered = DEMO_CATALOG_MODELS.filter((entry) => {
        if (providerFilter && entry.provider !== providerFilter) {
          return false;
        }
        if (request.tool_call_only && !entry.tool_call) {
          return false;
        }
        if (request.reasoning_only && !entry.reasoning) {
          return false;
        }
        if (!query) {
          return true;
        }
        return (
          entry.id.toLowerCase().includes(query) ||
          entry.name.toLowerCase().includes(query) ||
          Boolean(entry.family && entry.family.toLowerCase().includes(query))
        );
      });
      return {
        providers: DEMO_CATALOG_PROVIDERS.map((item) => ({ ...item, env_names: item.env_names ? [...item.env_names] : undefined })),
        models: filtered.slice(0, limit).map((item) => ({ ...item })),
        fetched_at: "2026-11-15T12:00:00Z",
        from_stale_cache: false,
        total_providers: DEMO_CATALOG_PROVIDERS.length,
        total_models: filtered.length,
      };
    },
    ListProviderPresets: async (request: { workspace: string }): Promise<ProviderPresetView[]> => {
      record("ListProviderPresets", [request]);
      return [...demoPresets].sort((a, b) => a.name.localeCompare(b.name));
    },
    UseProviderPreset: async (ws: string, name: string): Promise<OKResult> => {
      record("UseProviderPreset", [ws, name]);
      const trimmed = name.trim();
      if (!demoPresets.some((item) => item.name === trimmed)) {
        throw new Error(`preset "${trimmed}" is not defined in provider.presets`);
      }
      demoPresets = demoPresets.map((item) => ({ ...item, active: item.name === trimmed }));
      return ok();
    },
    SaveProviderPreset: async (request: SaveProviderPresetRequest): Promise<OKResult> => {
      record("SaveProviderPreset", [request]);
      const name = request.name.trim();
      const baseURL = request.base_url.trim();
      const model = request.model.trim();
      const apiKeyEnv = request.api_key_env.trim();
      if (!name || !baseURL || !model || !apiKeyEnv) {
        throw new Error("name, base_url, model and api_key_env are required");
      }
      if (!isValidDemoEnvName(apiKeyEnv)) {
        throw new Error("invalid api_key_env reference");
      }
      const existingIndex = demoPresets.findIndex((item) => item.name === name);
      if (existingIndex >= 0 && !request.overwrite) {
        throw new Error(`preset "${name}" already exists`);
      }
      const next: ProviderPresetView = {
        name,
        protocol: (request.protocol || "").trim() || "openai-chat-completions",
        base_url: baseURL,
        model,
        api_key_env: apiKeyEnv,
        admission: (request.admission || "").trim() || "required",
        active: existingIndex >= 0 ? demoPresets[existingIndex].active : false,
      };
      if (existingIndex >= 0) {
        demoPresets = demoPresets.map((item, idx) => (idx === existingIndex ? next : item));
      } else {
        demoPresets = [...demoPresets, next];
      }
      return ok();
    },
    RemoveProviderPreset: async (ws: string, name: string): Promise<OKResult> => {
      record("RemoveProviderPreset", [ws, name]);
      const trimmed = name.trim();
      const target = demoPresets.find((item) => item.name === trimmed);
      if (!target) {
        throw new Error(`preset "${trimmed}" is not defined in provider.presets`);
      }
      if (target.active) {
        throw new Error(`preset "${trimmed}" is active (switch first)`);
      }
      demoPresets = demoPresets.filter((item) => item.name !== trimmed);
      return ok();
    },
    RunProviderSetup: async (request: ProviderSetupRequest): Promise<ProviderSetupView> => {
      record("RunProviderSetup", [request]);
      const baseURL = request.base_url.trim();
      const model = request.model.trim();
      const apiKeyEnv = request.api_key_env.trim();
      if (!baseURL || !model || !apiKeyEnv || !isValidDemoEnvName(apiKeyEnv)) {
        throw new Error("base_url, model and valid api_key_env are required");
      }
      const matched = DEMO_CATALOG_MODELS.find(
        (entry) => entry.id.toLowerCase() === model.toLowerCase() || entry.name.toLowerCase() === model.toLowerCase(),
      );
      const notes: string[] = matched
        ? [`目录命中: ${matched.name}（context ${matched.context_tokens} / output ${matched.output_tokens}）`]
        : ["目录未命中该模型，写入 Cairn 生产所需能力声明"];
      const presetName = (request.save_preset || "").trim();
      if (presetName) {
        const existingIndex = demoPresets.findIndex((item) => item.name === presetName);
        if (existingIndex >= 0 && !request.overwrite_preset) {
          throw new Error(`preset "${presetName}" already exists`);
        }
        const saved: ProviderPresetView = {
          name: presetName,
          protocol: "openai-chat-completions",
          base_url: baseURL,
          model,
          api_key_env: apiKeyEnv,
          admission: (request.admission || "").trim() || "required",
          active: existingIndex >= 0 ? demoPresets[existingIndex].active : false,
        };
        if (existingIndex >= 0) {
          demoPresets = demoPresets.map((item, idx) => (idx === existingIndex ? saved : item));
        } else {
          demoPresets = [...demoPresets, saved];
        }
        notes.push(`saved preset ${presetName}`);
      }
      const configPath = request.user_scope ? "~/.cairn/config.yaml" : `${request.workspace || workspace.path}/.agent/config.yaml`;
      return {
        config_path: configPath,
        api_key_env: apiKeyEnv,
        catalog_matched: Boolean(matched),
        probe_ran: false,
        notes,
      };
    },
    ProbeProvider: async (request: { workspace: string }): Promise<ProviderProbeView> => {
      record("ProbeProvider", [request]);
      const active = demoPresets.find((item) => item.active);
      return {
        result: "pass",
        provider_kind: "openai-compatible",
        model: active?.model || "deepseek-chat",
        tool_roundtrip: "ok",
        usage_streamed: true,
        evidence_fingerprint: "demo-ev-sha256",
        config_fingerprint: "demo-cfg-sha256",
        request_profile_fingerprint: "demo-profile-sha256",
      };
    },
    ConfirmProviderAdmission: async (request: ProviderAdmissionRequest): Promise<OKResult> => {
      record("ConfirmProviderAdmission", [request]);
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
