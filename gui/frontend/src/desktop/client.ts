/**
 * 统一 desktop 桥接。所有后端调用经此模块；组件不得直接 import bindings。
 */
import { Bindings as DesktopAPI } from "./wails-api";
import type {
  ProviderProbeView,
  ProviderAdmissionRequest,
  AbortRequest,
  SteeringRequest,
  CancelRequest,
  ChangeSet,
  ErrorBody,
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
  evidence: (request: InspectRequest): Promise<EvidenceView> =>
    call(() => DesktopAPI.Evidence(request)),
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
  listQueue: (request: WorkspaceRequest): Promise<QueueItem[]> =>
    call(() => DesktopAPI.ListQueue({ workspace: request.workspace, state_dir: request.state_dir })),
  submitQueue: (request: QueueSubmitRequest): Promise<QueueItem> =>
    call(() => DesktopAPI.SubmitQueue(request)),
  confirmQueue: (request: QueueActionRequest): Promise<StartRunAck> =>
    call(() => DesktopAPI.ConfirmQueue(request)),
  cancelQueue: (request: QueueActionRequest): Promise<QueueItem> =>
    call(() => DesktopAPI.CancelQueue(request)),
  sessionTree: (request: WorkspaceRequest): Promise<SessionTreeView> =>
    call(() => DesktopAPI.SessionTree({ workspace: request.workspace, state_dir: request.state_dir })),
  doctor: (request: WorkspaceRequest): Promise<unknown> =>
    call(() => DesktopAPI.Doctor(request)),
  mcpStatus: (request: WorkspaceRequest): Promise<unknown> =>
    call(() => DesktopAPI.MCPStatus(request)),
  listSkills: (request: WorkspaceRequest): Promise<unknown> =>
    call(() => DesktopAPI.ListSkills(request)),
  listFileTree: (workspace: string): Promise<FileNode> =>
    call(() => DesktopAPI.ListFileTree(workspace)),
  previewFile: (workspace: string, relative: string): Promise<FilePreview> =>
    call(() => DesktopAPI.PreviewFile(workspace, relative)),
  listChanges: (workspace: string): Promise<ChangeSet> =>
    call(() => DesktopAPI.ListChanges(workspace)),
  diffFile: (workspace: string, relative: string, scope = "all"): Promise<string> =>
    call(() => DesktopAPI.DiffFile(workspace, relative, scope)),
  openPathInEditor: (workspace: string, relative: string): Promise<OKResult> =>
    call(() => DesktopAPI.OpenPathInEditor(workspace, relative)),
  submitSteering: (request: SteeringRequest): Promise<OKResult> =>
    call(() => DesktopAPI.SubmitSteering(request)),
  listHooks: (workspace: string): Promise<Array<{ id: string; event: string; enabled: boolean; actions: Array<{ kind: string; content?: string }> }>> =>
    call(() => DesktopAPI.ListHooks(workspace)),
  upsertHook: (rule: { id?: string; workspace: string; event?: string; enabled?: boolean; actions: Array<{ kind: string; content?: string }> }): Promise<OKResult> =>
    call(() => DesktopAPI.UpsertHook(rule)),
  disableHook: (id: string): Promise<OKResult> =>
    call(() => DesktopAPI.DisableHook(id)),
  listProjectInstructions: (workspace: string): Promise<{ documents: Array<{ source?: string; path: string; sha256?: string; bytes?: number; excerpt?: string }> }> =>
    call(() => DesktopAPI.ListProjectInstructions(workspace)),
  listWorkflows: (workspace: string): Promise<Array<{ id: string; name: string; steps: Array<{ task: string }> }>> =>
    call(() => DesktopAPI.ListWorkflows(workspace)),
  saveWorkflow: (def: { id?: string; workspace: string; name: string; steps: Array<{ task: string; model?: string; mode?: string }> }): Promise<OKResult> =>
    call(() => DesktopAPI.SaveWorkflow(def)),
  runWorkflow: (workspace: string, workflowID: string, dryRun: boolean): Promise<{ workflow_id: string; run_ids?: string[]; state: string }> =>
    call(() => DesktopAPI.RunWorkflow(workspace, workflowID, dryRun)),
  listMemories: (request: WorkspaceRequest): Promise<Array<{ id: string; kind: string; run_id?: string; outcome: string; status: string; trusted_verification: boolean; created_at?: string }>> =>
    call(() => DesktopAPI.ListMemories(request)),
  listProviderPresets: (request: WorkspaceRequest): Promise<Array<{ name: string; protocol: string; model: string; base_url: string; api_key_env: string; admission: string; active: boolean }>> =>
    call(() => DesktopAPI.ListProviderPresets(request)),
  useProviderPreset: (workspace: string, name: string): Promise<OKResult> =>
    call(() => DesktopAPI.UseProviderPreset(workspace, name)),
  runProviderSetup: (request: { base_url: string; model: string; api_key_env: string; workspace: string; admission?: string; overwrite?: boolean; user_scope?: boolean }): Promise<{ config_path: string; api_key_env: string; catalog_matched: boolean; probe_ran: boolean; notes?: string[] }> =>
    call(() => DesktopAPI.RunProviderSetup(request)),
  revokeMemory: (request: { workspace: string; candidate_id: string; reason: string }): Promise<unknown> =>
    call(() => DesktopAPI.RevokeMemory(request)),
  listCommands: (): Promise<Array<{ name: string; description: string; scope: string }>> =>
    call(() => DesktopAPI.ListCommands()),
  installSkill: (workspace: string, scope: string, source: string): Promise<{ name: string }> =>
    call(() => DesktopAPI.InstallSkill(workspace, scope, source)),
  setSkillEnabled: (workspace: string, scope: string, name: string, enabled: boolean): Promise<OKResult> =>
    call(() => DesktopAPI.SetSkillEnabled(workspace, scope, name, enabled)),
  removeSkill: (workspace: string, scope: string, name: string): Promise<OKResult> =>
    call(() => DesktopAPI.RemoveSkill(workspace, scope, name)),
  consolidateMemories: (request: WorkspaceRequest): Promise<number> =>
    call(() => DesktopAPI.ConsolidateMemories(request)),
  saveReviewNote: (note: { workspace: string; relative: string; line?: string; body: string }): Promise<OKResult> =>
    call(() => DesktopAPI.SaveReviewNote(note)),
  listReviewNotes: (workspace: string, relative: string): Promise<Array<{ id: string; relative: string; line?: string; body: string }>> =>
    call(() => DesktopAPI.ListReviewNotes(workspace, relative)),
  probeProvider: (request: WorkspaceRequest): Promise<ProviderProbeView> =>
    call(() => DesktopAPI.ProbeProvider(request)),
  confirmProviderAdmission: (request: ProviderAdmissionRequest): Promise<OKResult> =>
    call(() => DesktopAPI.ConfirmProviderAdmission(request)),
  writeProjectInstructions: (workspace: string, content: string): Promise<OKResult> =>
    call(() => DesktopAPI.WriteProjectInstructions(workspace, content)),
  inspectMemory: (workspace: string, candidateID: string): Promise<{ candidate: { id: string; run_id?: string; outcome: string; status: string }; audit?: string[] }> =>
    call(() => DesktopAPI.InspectMemory(workspace, candidateID)),
  onStream: (handler: (event: StreamEvent) => void): (() => void) => subscribeStream(handler),
};

export type DesktopClient = typeof desktop;
