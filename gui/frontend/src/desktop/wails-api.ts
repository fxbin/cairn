/**
 * 对 Wails 生成 bindings 的薄封装。
 * 桌面 WebView 只使用真实 bindings；模拟桥仅允许显式演示/测试入口启用。
 */
// @ts-ignore 生成的 JS bindings 无独立声明文件
import * as RawAPI from "../../bindings/github.com/fxbin/cairn/internal/desktop/service.js";
import type {
  ProviderProbeView,
  ProviderAdmissionRequest,
  AbortRequest,
  CancelRequest,
  ChangeSet,
  EvidenceView,
  FileNode,
  FilePreview,
  HealthResult,
  InspectRequest,
  OKResult,
  QueueActionRequest,
  QueueItem,
  QueueSubmitRequest,
  RecentProject,
  ResolveRequest,
  ResumeRequest,
  RunDetail,
  RunHistoryView,
  SessionTreeView,
  StartRunAck,
  StartRunRequest,
  WorkspaceRequest,
  WorkspaceStatus,
} from "./types";
import { bindBrowserBridge, isDemoBridgeActive } from "./browserBridge";

interface DesktopBindings {
  Health(): Promise<HealthResult>;
  ObserveWorkspace(workspace: string): Promise<WorkspaceStatus>;
  PickDirectory(title: string, defaultPath: string): Promise<string>;
  ListRecentProjects(includeArchived: boolean): Promise<RecentProject[]>;
  RememberProject(workspace: string): Promise<OKResult>;
  ArchiveProject(workspace: string, archived: boolean): Promise<OKResult>;
  ListRuns(request: WorkspaceRequest): Promise<RunHistoryView[]>;
  RenameSession(sessionID: string, title: string): Promise<OKResult>;
  SetSessionFlags(sessionID: string, pinned: boolean, archived: boolean): Promise<OKResult>;
  InspectRun(request: InspectRequest): Promise<RunDetail>;
  Evidence(request: InspectRequest): Promise<EvidenceView>;
  EvidenceFor(request: InspectRequest): Promise<unknown>;
  StartRun(request: StartRunRequest): Promise<StartRunAck>;
  ResumeRun(request: ResumeRequest): Promise<StartRunAck>;
  AbortRun(request: AbortRequest): Promise<unknown>;
  ResolveReconcile(request: ResolveRequest): Promise<unknown>;
  CancelRun(request: CancelRequest): Promise<OKResult>;
  ListQueue(request: WorkspaceRequest): Promise<QueueItem[]>;
  SubmitQueue(request: QueueSubmitRequest): Promise<QueueItem>;
  ConfirmQueue(request: QueueActionRequest): Promise<StartRunAck>;
  CancelQueue(request: QueueActionRequest): Promise<QueueItem>;
  SessionTree(request: WorkspaceRequest): Promise<SessionTreeView>;
  Doctor(request: WorkspaceRequest): Promise<unknown>;
  MCPStatus(request: WorkspaceRequest): Promise<unknown>;
  ListSkills(request: WorkspaceRequest): Promise<unknown>;
  ListFileTree(workspace: string): Promise<FileNode>;
  PreviewFile(workspace: string, relative: string): Promise<FilePreview>;
  ListChanges(workspace: string): Promise<ChangeSet>;
  DiffFile(workspace: string, relative: string, scope: string): Promise<string>;
  OpenPathInEditor(workspace: string, relative: string): Promise<OKResult>;
  ListOpenLocations(workspace: string): Promise<Array<{ id: string; label: string; available: boolean }>>;
  OpenInLocationWithTarget(workspace: string, relative: string, target: string): Promise<OKResult>;
  SubmitSteering(request: { run_id: string; input_id?: string; content: string }): Promise<OKResult>;
  ListHooks(workspace: string): Promise<Array<{ id: string; event: string; enabled: boolean; actions: Array<{ kind: string; content?: string }> }>>;
  UpsertHook(rule: { id?: string; workspace: string; event?: string; enabled?: boolean; actions: Array<{ kind: string; content?: string }> }): Promise<OKResult>;
  DisableHook(id: string): Promise<OKResult>;
  ListProjectInstructions(workspace: string): Promise<{ documents: Array<{ source?: string; path: string; sha256?: string; bytes?: number; excerpt?: string }> }>;
  ListWorkflows(workspace: string): Promise<Array<{ id: string; name: string; steps: Array<{ task: string }> }>>;
  SaveWorkflow(def: { id?: string; workspace: string; name: string; steps: Array<{ task: string; model?: string; mode?: string }> }): Promise<OKResult>;
  RunWorkflow(workspace: string, workflowID: string, dryRun: boolean): Promise<{ workflow_id: string; run_ids?: string[]; state: string }>;
  ListMemories(request: WorkspaceRequest): Promise<Array<{ id: string; kind: string; run_id?: string; outcome: string; status: string; trusted_verification: boolean; created_at?: string }>>;
  ListProviderPresets(request: WorkspaceRequest): Promise<Array<{ name: string; protocol: string; model: string; base_url: string; api_key_env: string; admission: string; active: boolean }>>;
  UseProviderPreset(workspace: string, name: string): Promise<OKResult>;
  RunProviderSetup(request: { base_url: string; model: string; api_key_env: string; workspace: string; admission?: string; overwrite?: boolean; user_scope?: boolean }): Promise<{ config_path: string; api_key_env: string; catalog_matched: boolean; probe_ran: boolean; notes?: string[] }>;
  RevokeMemory(request: { workspace: string; candidate_id: string; reason: string }): Promise<unknown>;
  ListCommands(): Promise<Array<{ name: string; description: string; scope: string }>>;
  InstallSkill(workspace: string, scope: string, source: string): Promise<{ name: string }>;
  SetSkillEnabled(workspace: string, scope: string, name: string, enabled: boolean): Promise<OKResult>;
  RemoveSkill(workspace: string, scope: string, name: string): Promise<OKResult>;
  ConsolidateMemories(request: WorkspaceRequest): Promise<number>;
  SaveReviewNote(note: { workspace: string; relative: string; line?: string; body: string }): Promise<OKResult>;
  ListReviewNotes(workspace: string, relative: string): Promise<Array<{ id: string; relative: string; line?: string; body: string }>>;
  ProbeProvider(request: WorkspaceRequest): Promise<ProviderProbeView>;
  ConfirmProviderAdmission(request: ProviderAdmissionRequest): Promise<OKResult>;
  WriteProjectInstructions(workspace: string, content: string): Promise<OKResult>;
  InspectMemory(workspace: string, candidateID: string): Promise<{ candidate: { id: string; run_id?: string; outcome: string; status: string }; audit?: string[] }>;
}

/**
 * 后端连接模式：原生 Wails、显式演示桥、HTTP 传输通道的浏览器客户端、或不可用。
 *
 * "http" 表示前端有真实绑定可用、经 runtime 的 HTTP 传输通道调用真实后端；
 * 该后端是否真的可达由调用结果决定，模式本身不作断言。
 */
export type BridgeMode = "wails" | "demo" | "http" | "unavailable";

interface WailsGlobals {
  _wails?: { dispatchWailsEvent?: unknown };
  wails?: { invokeAsync?: unknown };
  __CAIRN_HOST__?: "wails" | string;
  __CAIRN_BRIDGE__?: DesktopBindings;
  __CAIRN_FORCE_DEMO__?: boolean;
}

function hostWindow(): (Window & typeof globalThis & WailsGlobals) | null {
  return typeof window === "undefined" ? null : (window as Window & typeof globalThis & WailsGlobals);
}

/**
 * detectWailsRuntime 判断是否运行在真实 Wails WebView。
 * 不能仅凭 window._wails：@wailsio/runtime 在纯浏览器也会写入该对象。
 * 采用宿主注入标记 __CAIRN_HOST__ 或原生 invokeAsync 桥作为真实信号。
 */
export function detectWailsRuntime(): boolean {
  const win = hostWindow();
  if (!win) {
    return false;
  }
  if (win.__CAIRN_HOST__ === "wails") {
    return true;
  }
  return typeof win.wails?.invokeAsync === "function";
}

/**
 * detectDemoBridge 判断是否显式启用了演示/测试桥。
 * 仅接受预注入的 __CAIRN_BRIDGE__/__CAIRN_FORCE_DEMO__，
 * 或 URL 查询参数 cairn-demo=1。生产桌面包不得静默落到模拟后端。
 */
export function detectDemoBridge(): boolean {
  const win = hostWindow();
  if (!win) {
    return false;
  }
  if (win.__CAIRN_BRIDGE__ || win.__CAIRN_FORCE_DEMO__) {
    return true;
  }
  try {
    const params = new URLSearchParams(win.location?.search || "");
    return params.get("cairn-demo") === "1";
  } catch {
    return false;
  }
}

/**
 * hasRawBindings 判断真实绑定是否可用。
 *
 * 绑定的存在只说明前端有真实调用通道；后端是否真的连得上，由调用结果决定。
 */
function hasRawBindings(): boolean {
  const raw = RawAPI as unknown as Record<string, unknown>;
  return typeof raw.Health === "function";
}

/**
 * detectHttpTransport 判断当前只能经 HTTP 传输通道访问真实后端。
 *
 * 浏览器里的客户端没有宿主注入标记，但 @wailsio/runtime 默认就走
 * POST {origin}/wails/runtime 的 fetch 通道：wails3 的 server 模式把这条传输
 * 挂在同一端口上，因此浏览器也能驱动真实后端。这里只判断「有没有真实绑定
 * 可用」，可达性交给调用结果。
 */
export function detectHttpTransport(): boolean {
  return !detectWailsRuntime() && !detectDemoBridge() && hasRawBindings();
}

/** bridgeMode 返回当前后端连接模式，供 UI 标识演示身份。 */
export function bridgeMode(): BridgeMode {
  if (detectWailsRuntime()) {
    return "wails";
  }
  if (detectDemoBridge()) {
    return "demo";
  }
  if (hasRawBindings()) {
    return "http";
  }
  return "unavailable";
}

/** BridgeUnavailableError 表示缺少真实桌面后端且未显式启用演示桥。 */
export class BridgeUnavailableError extends Error {
  readonly code = "bridge_unavailable";
  constructor(method: string, detail = "") {
    super(
      detail
        ? `桌面后端不可用：${method} ${detail}`
        : `桌面后端不可用：${method} 需要 Wails 运行时或显式演示入口（?cairn-demo=1）`,
    );
    this.name = "BridgeUnavailableError";
  }
}

function failClosed(method: string): Promise<never> {
  return Promise.reject(new BridgeUnavailableError(method));
}

/**
 * isBackendError 判断错误是否来自桌面后端自身的响应。
 *
 * @wailsio/runtime 解析后端的 JSON 错误信封时会挂上 cause 字段；响应不是
 * 协议格式时它把响应正文原样作为错误消息抛出，没有这个字段。据此把
 * 「后端答错了」与「没连上后端」分开：前者原样透传，后者转成
 * BridgeUnavailableError。
 */
function isBackendError(error: unknown): boolean {
  if (error instanceof TypeError) {
    // 网络层失败一律是 TypeError：浏览器报「Failed to fetch」，Node 报
    // 「fetch failed」（后者还带 cause，不能只看 cause 判断）。
    return false;
  }
  return typeof error === "object" && error !== null && "cause" in error;
}

/**
 * withTransportGuard 给 HTTP 传输通道的调用补上失败关闭语义。
 *
 * 传输层不可达时原始报错是浏览器的网络错误或一段非协议响应正文，对使用者
 * 没有意义；这里统一转成 BridgeUnavailableError 说明桌面后端连不上。后端
 * 自己返回的错误原样透传，不改写内容。
 */
function withTransportGuard(bindings: Record<string, unknown>): Record<string, unknown> {
  const guarded: Record<string, unknown> = {};
  for (const [name, value] of Object.entries(bindings)) {
    if (typeof value !== "function") {
      guarded[name] = value;
      continue;
    }
    const method = value as (...args: unknown[]) => unknown;
    guarded[name] = (...args: unknown[]) => {
      const result = method.apply(bindings, args);
      if (!result || typeof (result as Promise<unknown>).catch !== "function") {
        return result;
      }
      return (result as Promise<unknown>).catch((error: unknown) => {
        if (!isBackendError(error)) {
          throw new BridgeUnavailableError(String(name), "无法通过 HTTP 传输通道连接 /wails/runtime");
        }
        throw error;
      });
    };
  }
  return guarded;
}

let cachedDemo: DesktopBindings | null = null;

function resolveBindings(): DesktopBindings {
  const raw = RawAPI as unknown as Record<string, unknown>;
  const hasRaw = typeof raw.Health === "function";
  if (detectWailsRuntime() && hasRaw) {
    return raw as unknown as DesktopBindings;
  }
  if (detectDemoBridge()) {
    if (!cachedDemo) {
      const win = hostWindow();
      if (win?.__CAIRN_BRIDGE__) {
        cachedDemo = win.__CAIRN_BRIDGE__;
      } else {
        cachedDemo = bindBrowserBridge() as unknown as DesktopBindings;
      }
    }
    return cachedDemo;
  }
  if (hasRaw) {
    // 浏览器客户端：真实绑定经 runtime 的 HTTP 传输通道调用真实后端。
    return withTransportGuard(raw) as unknown as DesktopBindings;
  }
  return new Proxy({} as DesktopBindings, {
    get(_target, prop: string) {
      return () => failClosed(String(prop));
    },
  });
}

const impl: DesktopBindings = new Proxy({} as DesktopBindings, {
  get(_target, prop: string) {
    const bindings = resolveBindings() as unknown as Record<string, unknown>;
    const fn = bindings[prop];
    if (typeof fn === "function") {
      return fn.bind(bindings);
    }
    return () => failClosed(String(prop));
  },
});

export const Bindings = impl;

/** demoBridgeActive 供 UI 显示演示身份条。 */
export function demoBridgeActive(): boolean {
  return bridgeMode() === "demo" || isDemoBridgeActive();
}
