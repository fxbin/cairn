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
});
