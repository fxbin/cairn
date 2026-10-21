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

describe("App shell", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    desktopMock.health.mockResolvedValue({ ok: true, app_name: "Cairn", version: "0.1.0", go_version: "go1.26.4" });
    desktopMock.observeWorkspace.mockResolvedValue({
      path: "/tmp/demo", exists: true, is_workspace: true, has_agent_dir: true, has_config: true,
      dirty: false, state_dir: "/tmp/state", config_path: "/tmp/demo/.agent/config.yaml", config_loaded: true,
    });
    desktopMock.listRecentProjects.mockResolvedValue([
      { path: "/tmp/demo", name: "demo", last_used: new Date().toISOString(), pinned: false, archived: false },
    ]);
    desktopMock.rememberProject.mockResolvedValue({ ok: true });
    desktopMock.listRuns.mockResolvedValue([{
      run_id: "run-1", session_id: "s1", task: "修复登录问题", state: "completed", phase: "completed",
      created_at: new Date().toISOString(), updated_at: new Date().toISOString(),
      title: "修复登录问题", waiting_approval: false, waiting_reconcile: false,
    }]);
    desktopMock.inspectRun.mockResolvedValue({ run_id: "run-1", state: "completed", summary: {}, events: [] });
    desktopMock.evidence.mockResolvedValue({ state: "completed", outcome: "ok", trusted: false, verification: { verification_ok: false } });
    desktopMock.listQueue.mockResolvedValue([]);
    desktopMock.listFileTree.mockResolvedValue({ name: "root", relative: ".", path: "/tmp/demo", kind: "dir", children: [] });
    desktopMock.listChanges.mockResolvedValue({ root: "/tmp/demo", entries: [], added: 0, deleted: 0, modified: 0, untracked: 0 });
    desktopMock.doctor.mockResolvedValue({ ready: true });
    desktopMock.mcpStatus.mockResolvedValue([]);
    desktopMock.listSkills.mockResolvedValue([]);
    desktopMock.onStream.mockReturnValue(() => undefined);
  });

  it("renders shell regions and run list", async () => {
    render(<App />);
    expect(screen.getByTestId("cairn-shell")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByTestId("run-list")).toHaveTextContent("修复登录问题");
    });
    expect(screen.getByTestId("task-input")).toBeInTheDocument();
    expect(screen.getByTestId("approval-empty")).toBeInTheDocument();
  });

  it("submits a task through the desktop bridge", async () => {
    desktopMock.startRun.mockResolvedValue({ run_id: "run-2", state: "starting" });
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await userEvent.type(screen.getByTestId("task-input"), "写一个测试");
    await userEvent.click(screen.getByTestId("task-submit"));
    await waitFor(() => {
      expect(desktopMock.startRun).toHaveBeenCalledWith(
        expect.objectContaining({ task: "写一个测试", workspace: "/tmp/demo" }),
      );
    });
  });
});
