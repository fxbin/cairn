/**
 * 统一 desktop 桥接。所有后端调用经此模块；组件不得直接 import bindings。
 */
import { Bindings as DesktopAPI } from "./wails-api";
import type {
  AbortRequest,
  CancelRequest,
  ErrorBody,
  HealthResult,
  InspectRequest,
  OKResult,
  RecentProject,
  ResolveRequest,
  ResumeRequest,
  RunDetail,
  RunHistoryView,
  StartRunAck,
  StartRunRequest,
  StreamEvent,
  WorkspaceRequest,
  WorkspaceStatus,
} from "./types";
import { subscribeStream } from "./events";

export class DesktopError extends Error {
  readonly body: ErrorBody;

  constructor(body: ErrorBody) {
    super(body.message || body.code);
    this.name = "DesktopError";
    this.body = body;
  }
}

function toErrorBody(error: unknown): ErrorBody {
  if (error && typeof error === "object") {
    const anyErr = error as Record<string, unknown>;
    if (typeof anyErr.code === "string" && typeof anyErr.message === "string") {
      return anyErr as unknown as ErrorBody;
    }
    if (typeof anyErr.Error === "function") {
      // Wails 可能包装为 { Error(): string }
      return { code: "internal", message: String((anyErr.Error as () => string)()) };
    }
    if (typeof anyErr.message === "string") {
      return { code: "internal", message: anyErr.message };
    }
  }
  return { code: "internal", message: String(error) };
}

async function call<T>(fn: () => Promise<T>): Promise<T> {
  try {
    return await fn();
  } catch (error) {
    throw new DesktopError(toErrorBody(error));
  }
}

export const desktop = {
  health: (): Promise<HealthResult> => call(() => DesktopAPI.Health()),
  observeWorkspace: (workspace: string): Promise<WorkspaceStatus> =>
    call(() => DesktopAPI.ObserveWorkspace(workspace)),
  pickDirectory: (title: string, defaultPath = ""): Promise<string> =>
    call(() => DesktopAPI.PickDirectory(title, defaultPath)),
  listRecentProjects: (includeArchived = false): Promise<RecentProject[]> =>
    call(() => DesktopAPI.ListRecentProjects(includeArchived)),
  rememberProject: (workspace: string): Promise<OKResult> =>
    call(() => DesktopAPI.RememberProject(workspace)),
  archiveProject: (workspace: string, archived: boolean): Promise<OKResult> =>
    call(() => DesktopAPI.ArchiveProject(workspace, archived)),
  listRuns: (request: WorkspaceRequest): Promise<RunHistoryView[]> =>
    call(() => DesktopAPI.ListRuns(request)),
  renameSession: (sessionId: string, title: string): Promise<OKResult> =>
    call(() => DesktopAPI.RenameSession(sessionId, title)),
  setSessionFlags: (sessionId: string, pinned: boolean, archived: boolean): Promise<OKResult> =>
    call(() => DesktopAPI.SetSessionFlags(sessionId, pinned, archived)),
  inspectRun: (request: InspectRequest): Promise<RunDetail> =>
    call(() => DesktopAPI.InspectRun(request)),
  startRun: (request: StartRunRequest): Promise<StartRunAck> =>
    call(() => DesktopAPI.StartRun(request)),
  resumeRun: (request: ResumeRequest): Promise<StartRunAck> =>
    call(() => DesktopAPI.ResumeRun(request)),
  abortRun: (request: AbortRequest): Promise<unknown> =>
    call(() => DesktopAPI.AbortRun(request)),
  resolveReconcile: (request: ResolveRequest): Promise<unknown> =>
    call(() => DesktopAPI.ResolveReconcile(request)),
  cancelRun: (request: CancelRequest): Promise<OKResult> =>
    call(() => DesktopAPI.CancelRun(request)),
  doctor: (request: WorkspaceRequest): Promise<unknown> =>
    call(() => DesktopAPI.Doctor(request)),
  mcpStatus: (request: WorkspaceRequest): Promise<unknown> =>
    call(() => DesktopAPI.MCPStatus(request)),
  listSkills: (request: WorkspaceRequest): Promise<unknown> =>
    call(() => DesktopAPI.ListSkills(request)),
  listFileTree: (workspace: string): Promise<import("./types").FileNode> =>
    call(() => DesktopAPI.ListFileTree(workspace)),
  previewFile: (workspace: string, relative: string): Promise<import("./types").FilePreview> =>
    call(() => DesktopAPI.PreviewFile(workspace, relative)),
  listChanges: (workspace: string): Promise<import("./types").ChangeSet> =>
    call(() => DesktopAPI.ListChanges(workspace)),
  diffFile: (workspace: string, relative: string): Promise<string> =>
    call(() => DesktopAPI.DiffFile(workspace, relative)),
  openPathInEditor: (path: string): Promise<OKResult> =>
    call(() => DesktopAPI.OpenPathInEditor(path)),
  onStream: (handler: (event: StreamEvent) => void): (() => void) => subscribeStream(handler),
};

export type DesktopClient = typeof desktop;
