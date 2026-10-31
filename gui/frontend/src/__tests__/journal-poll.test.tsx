import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const desktopMock = vi.hoisted(() => ({
  health: vi.fn(),
  observeWorkspace: vi.fn(),
  pickDirectory: vi.fn(),
  listRecentProjects: vi.fn(),
  rememberProject: vi.fn(),
  archiveProject: vi.fn(),
  listRuns: vi.fn(),
  renameSession: vi.fn(),
  setSessionFlags: vi.fn(),
  inspectRun: vi.fn(),
  evidence: vi.fn(),
  startRun: vi.fn(),
  resumeRun: vi.fn(),
  abortRun: vi.fn(),
  resolveReconcile: vi.fn(),
  cancelRun: vi.fn(),
  listQueue: vi.fn(async () => []),
  submitQueue: vi.fn(),
  confirmQueue: vi.fn(),
  cancelQueue: vi.fn(),
  sessionTree: vi.fn(),
  doctor: vi.fn(),
  mcpStatus: vi.fn(),
  listSkills: vi.fn(),
  listFileTree: vi.fn(async () => ({ name: "r", relative: ".", path: "/tmp/demo", kind: "dir", children: [] })),
  previewFile: vi.fn(),
  listChanges: vi.fn(async () => ({ root: "/tmp/demo", entries: [], added: 0, deleted: 0, modified: 0, untracked: 0 })),
  diffFile: vi.fn(async () => ""),
  openPathInEditor: vi.fn(),
  submitSteering: vi.fn(),
  listHooks: vi.fn(async () => []),
  upsertHook: vi.fn(async () => ({ ok: true })),
  disableHook: vi.fn(async () => ({ ok: true })),
  listWorkflows: vi.fn(async () => []),
  saveWorkflow: vi.fn(async () => ({ ok: true })),
  runWorkflow: vi.fn(async () => ({ workflow_id: "w1", state: "running" })),
  listProjectInstructions: vi.fn(async () => ({ documents: [] })),
  listMemories: vi.fn(async () => []),
  revokeMemory: vi.fn(async () => ({})),
  listCommands: vi.fn(async () => []),
  installSkill: vi.fn(async () => ({ name: "s" })),
  setSkillEnabled: vi.fn(async () => ({ ok: true })),
  removeSkill: vi.fn(async () => ({ ok: true })),
  consolidateMemories: vi.fn(async () => 0),
  saveReviewNote: vi.fn(async () => ({ ok: true })),
  listReviewNotes: vi.fn(async () => []),
  listProviderPresets: vi.fn(async () => []),
  runProviderSetup: vi.fn(),
  probeProvider: vi.fn(async () => ({ result: "pass" })),
  writeProjectInstructions: vi.fn(async () => ({ ok: true })),
  inspectMemory: vi.fn(async () => ({ candidate: { id: "c1", outcome: "ok", status: "active" }, audit: [] })),
  onStream: vi.fn(() => () => undefined),
}));

vi.mock("../desktop/client", () => ({
  desktop: desktopMock,
  DesktopError: class DesktopError extends Error {
    body: { code: string; message: string };
    constructor(body: { code: string; message: string }) {
      super(body.message);
      this.body = body;
    }
  },
}));

import App from "../App";

function runDetail(withApproval: boolean) {
  const events: Array<Record<string, unknown>> = [
    {
      schema_version: 1,
      event_id: "e1",
      run_id: "run-1",
      workspace_hash: "w",
      seq: 1,
      occurred_at: new Date().toISOString(),
      type: "run_created",
      payload: { task: "改一下文件" },
      prev_hash: "",
      event_hash: "",
    },
  ];
  if (withApproval) {
    events.push({
      schema_version: 1,
      event_id: "e2",
      run_id: "run-1",
      workspace_hash: "w",
      seq: 2,
      occurred_at: new Date().toISOString(),
      type: "approval_requested",
      payload: { fingerprint: "fp-1", summary: "写入 notes.txt" },
      prev_hash: "",
      event_hash: "",
    });
  }
  return {
    run_id: "run-1",
    session_id: "s1",
    state: withApproval ? "running" : "running",
    phase: "running",
    summary: {},
    events,
  };
}

describe("HTTP 传输通道按 Journal 轮询运行进度", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    desktopMock.health.mockResolvedValue({ ok: true, app_name: "Cairn", version: "0.1.0", go_version: "go" });
    desktopMock.observeWorkspace.mockResolvedValue({
      path: "/tmp/demo",
      exists: true,
      is_workspace: true,
      has_agent_dir: true,
      has_config: true,
      dirty: false,
      state_dir: "/s",
      config_path: "/c",
      config_loaded: true,
    });
    desktopMock.listRecentProjects.mockResolvedValue([
      { path: "/tmp/demo", name: "demo", last_used: new Date().toISOString(), pinned: false, archived: false },
    ]);
    desktopMock.listRuns.mockResolvedValue([
      {
        run_id: "run-1",
        session_id: "s1",
        task: "改一下文件",
        state: "running",
        phase: "running",
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
        title: "改一下文件",
        waiting_approval: false,
        waiting_reconcile: false,
      },
    ]);
    desktopMock.rememberProject.mockResolvedValue({ ok: true });
    desktopMock.listQueue.mockResolvedValue([]);
    desktopMock.listFileTree.mockResolvedValue({ name: "r", relative: ".", path: "/tmp/demo", kind: "dir", children: [] });
    desktopMock.listChanges.mockResolvedValue({ root: "/tmp/demo", entries: [], added: 0, deleted: 0, modified: 0, untracked: 0 });
    desktopMock.evidence.mockResolvedValue({
      state: "running",
      outcome: "",
      trusted: false,
      verification: { verification_ok: false },
      metrics: { turns: 1, tool_calls: 0, input_tokens: 0, output_tokens: 0, usage_estimated: false, context_compactions: 0, wall_time_ms: 0 },
    });
    desktopMock.onStream.mockReturnValue(() => undefined);
  });

  it("选中运行后按间隔重新读取运行详情，审批停点因此可见", async () => {
    // 选中运行本身会触发几次详情读取；只有轮询（间隔 2 秒）之后的读取才会拿到
    // 运行到达审批停点后的事件，所以审批卡的出现即为轮询生效的证据。
    const startedAt = Date.now();
    desktopMock.inspectRun.mockImplementation(async () => runDetail(Date.now() - startedAt > 1200));

    render(<App />);
    await waitFor(() => expect(screen.getByTestId("thread-list")).toHaveTextContent("改一下文件"));
    await userEvent.click(screen.getByText("改一下文件"));

    await waitFor(() => expect(desktopMock.inspectRun).toHaveBeenCalled(), { timeout: 5000 });
    const callsAfterSelect = desktopMock.inspectRun.mock.calls.length;

    // 轮询持续读取运行详情：调用次数随时间增长。
    await waitFor(() => {
      expect(desktopMock.inspectRun.mock.calls.length).toBeGreaterThan(callsAfterSelect);
    }, { timeout: 8000 });

    // 轮询拿到的事件进入界面：审批停点可见。
    await waitFor(() => expect(screen.getByTestId("approval-card")).toBeInTheDocument(), { timeout: 8000 });
    expect(screen.getByTestId("approval-card")).toHaveTextContent("写入 notes.txt");
  }, 20000);
});
