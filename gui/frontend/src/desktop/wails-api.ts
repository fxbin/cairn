/**
 * 对 Wails 生成 bindings 的薄封装。
 * 桌面 WebView 使用真实 bindings；浏览器验收使用 window.__CAIRN_BRIDGE__。
 */
// @ts-ignore 生成的 JS bindings 无独立声明文件
import * as RawAPI from "../../bindings/github.com/fxbin/cairn/internal/desktop/service.js";
import type {
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
import { bindBrowserBridge } from "./browserBridge";

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
  DiffFile(workspace: string, relative: string): Promise<string>;
  OpenPathInEditor(path: string): Promise<OKResult>;
}

function inWailsRuntime(): boolean {
  return typeof window !== "undefined" && Boolean((window as unknown as { runtime?: unknown }).runtime);
}

function bridge(): DesktopBindings {
  const existing = (window as unknown as { __CAIRN_BRIDGE__?: DesktopBindings }).__CAIRN_BRIDGE__;
  if (existing) {
    return existing;
  }
  return bindBrowserBridge() as unknown as DesktopBindings;
}

const impl: DesktopBindings = new Proxy({} as DesktopBindings, {
  get(_target, prop: string) {
    const fallback = bridge() as unknown as Record<string, unknown>;
    const raw = (RawAPI as unknown as Record<string, unknown>)[prop];
    if (inWailsRuntime() && typeof raw === "function") {
      return raw.bind(RawAPI);
    }
    const fn = fallback[prop];
    if (typeof fn === "function") {
      return fn.bind(fallback);
    }
    return raw;
  },
});

export const Bindings = impl;
