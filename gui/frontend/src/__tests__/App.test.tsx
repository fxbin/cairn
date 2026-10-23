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
  listQueue: vi.fn(),
  submitQueue: vi.fn(),
  confirmQueue: vi.fn(),
  cancelQueue: vi.fn(),
  sessionTree: vi.fn(),
  doctor: vi.fn(),
  mcpStatus: vi.fn(),
  listSkills: vi.fn(),
  listFileTree: vi.fn(),
  previewFile: vi.fn(),
  listChanges: vi.fn(),
  diffFile: vi.fn(),
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

describe("Chat-first shell", () => {
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
    desktopMock.rememberProject.mockResolvedValue({ ok: true });
    desktopMock.listRuns.mockResolvedValue([
      {
        run_id: "run-1",
        session_id: "s1",
        task: "修复登录问题",
        state: "completed",
        phase: "completed",
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
        title: "修复登录问题",
        waiting_approval: false,
        waiting_reconcile: false,
      },
    ]);
    desktopMock.inspectRun.mockResolvedValue({
      run_id: "run-1",
      state: "completed",
      summary: {},
      events: [
        {
          schema_version: 1,
          event_id: "e1",
          run_id: "run-1",
          workspace_hash: "w",
          seq: 1,
          occurred_at: new Date().toISOString(),
          type: "run_created",
          payload: { task: "修复登录问题" },
          prev_hash: "",
          event_hash: "",
        },
      ],
    });
    desktopMock.evidence.mockResolvedValue({
      state: "completed",
      outcome: "ok",
      trusted: false,
      verification: { verification_ok: false },
      metrics: { turns: 1, tool_calls: 0, input_tokens: 0, output_tokens: 0, usage_estimated: false, context_compactions: 0, wall_time_ms: 0 },
    });
    desktopMock.listQueue.mockResolvedValue([]);
    desktopMock.listFileTree.mockResolvedValue({ name: "r", relative: ".", path: "/tmp/demo", kind: "dir", children: [] });
    desktopMock.listChanges.mockResolvedValue({ root: "/tmp/demo", entries: [], added: 0, deleted: 0, modified: 0, untracked: 0 });
    desktopMock.onStream.mockReturnValue(() => undefined);
  });

  it("renders chat shell, threads and composer", async () => {
    render(<App />);
    expect(screen.getByTestId("cairn-shell")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByTestId("thread-list")).toHaveTextContent("修复登录问题");
    });
    expect(screen.getByTestId("composer-input")).toBeInTheDocument();
    expect(screen.getByTestId("new-session")).toBeInTheDocument();
  });

  it("submits a task via composer", async () => {
    desktopMock.startRun.mockResolvedValue({ run_id: "run-2", state: "starting" });
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await userEvent.type(screen.getByTestId("composer-input"), "写一个测试");
    await userEvent.click(screen.getByTestId("composer-send"));
    await waitFor(() => {
      expect(desktopMock.startRun).toHaveBeenCalledWith(
        expect.objectContaining({ task: "写一个测试", workspace: "/tmp/demo" }),
      );
    });
  });
  it("配置生成成功后清除已解决的旧错误并显示成功通知", async () => {
    desktopMock.listRuns.mockRejectedValueOnce(new Error("尚未配置 Provider"));
    desktopMock.doctor.mockResolvedValue({});
    desktopMock.mcpStatus.mockResolvedValue([]);
    desktopMock.listSkills.mockResolvedValue([]);
    desktopMock.runProviderSetup.mockResolvedValue({ config_path: "/tmp/demo/.agent/config.yaml", probe_ran: true, notes: [] });
    render(<App />);
    await waitFor(() => expect(screen.getByTestId("error-banner")).toHaveTextContent("尚未配置 Provider"));
    await userEvent.click(screen.getByTestId("open-settings"));
    await userEvent.click(screen.getByRole("button", { name: "Provider" }));
    await userEvent.type(screen.getByTestId("setup-base-url"), "http://127.0.0.1:1/v1");
    await userEvent.type(screen.getByTestId("setup-model"), "local-model");
    await userEvent.click(screen.getByTestId("run-setup"));
    await waitFor(() => expect(screen.getByTestId("provider-notice")).toHaveTextContent("配置已生成"));
    await waitFor(() => expect(screen.queryByTestId("error-banner")).not.toBeInTheDocument());
  });

});
