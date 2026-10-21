/**
 * 对 Wails 生成 bindings 的薄封装。生成物无完整 .d.ts，此处以本地类型收窄。
 * 组件不得直接依赖生成文件。
 */
// @ts-expect-error 生成的 JS bindings 无独立声明文件
import * as RawAPI from "../../bindings/github.com/fxbin/cairn/internal/desktop/service.js";
import type {
  AbortRequest,
  CancelRequest,
  ChangeSet,
  FileNode,
  FilePreview,
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
  WorkspaceRequest,
  WorkspaceStatus,
} from "./types";

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
  EvidenceFor(request: InspectRequest): Promise<unknown>;
  StartRun(request: StartRunRequest): Promise<StartRunAck>;
  ResumeRun(request: ResumeRequest): Promise<StartRunAck>;
  AbortRun(request: AbortRequest): Promise<unknown>;
  ResolveReconcile(request: ResolveRequest): Promise<unknown>;
  CancelRun(request: CancelRequest): Promise<OKResult>;
  Doctor(request: WorkspaceRequest): Promise<unknown>;
  MCPStatus(request: WorkspaceRequest): Promise<unknown>;
  ListSkills(request: WorkspaceRequest): Promise<unknown>;
  ListFileTree(workspace: string): Promise<FileNode>;
  PreviewFile(workspace: string, relative: string): Promise<FilePreview>;
  ListChanges(workspace: string): Promise<ChangeSet>;
  DiffFile(workspace: string, relative: string): Promise<string>;
  OpenPathInEditor(path: string): Promise<OKResult>;
}

export const Bindings = RawAPI as DesktopBindings;
