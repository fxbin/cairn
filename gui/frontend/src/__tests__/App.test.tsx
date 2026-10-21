import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import App from "../App";
import { desktop } from "../desktop/client";
import type { HealthResult, RecentProject, RunHistoryView, WorkspaceStatus } from "../desktop/types";

vi.mock("../desktop/client", async () => {
  const actual = await vi.importActual<typeof import("../desktop/client")>("../desktop/client");
  return {
    ...actual,
    desktop: {
      health: vi.fn(),
      observeWorkspace: vi.fn(),
      listRecentProjects: vi.fn(),
      rememberProject: vi.fn(),
      listRuns: vi.fn(),
      startRun: vi.fn(),
      resumeRun: vi.fn(),
      cancelRun: vi.fn(),
      doctor: vi.fn(),
      onStream: vi.fn(() => () => undefined),
    },
  };
});

const health: HealthResult = {
  ok: true,
  app_name: "Cairn",
  version: "0.1.0",
  go_version: "go1.26.4",
};

const workspace: WorkspaceStatus = {
  path: "/tmp/demo",
  exists: true,
  is_workspace: true,
  has_agent_dir: true,
  has_config: true,
  dirty: false,
  state_dir: "/tmp/state",
  config_path: "/tmp/demo/.agent/config.yaml",
  config_loaded: true,
};

const recent: RecentProject[] = [
  { path: "/tmp/demo", name: "demo", last_used: new Date().toISOString(), pinned: false, archived: false },
];

const runs: RunHistoryView[] = [
  {
    run_id: "run-1",
    session_id: "s1",
    task: "修复登录问题\n细节",
    state: "completed",
    phase: "completed",
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    title: "修复登录问题",
    waiting_approval: false,
    waiting_reconcile: false,
  },
];

describe("App shell", () => {
  beforeEach(() => {
    vi.mocked(desktop.health).mockResolvedValue(health);
    vi.mocked(desktop.observeWorkspace).mockResolvedValue(workspace);
    vi.mocked(desktop.listRecentProjects).mockResolvedValue(recent);
    vi.mocked(desktop.rememberProject).mockResolvedValue({ ok: true });
    vi.mocked(desktop.listRuns).mockResolvedValue(runs);
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
    vi.mocked(desktop.startRun).mockResolvedValue({ run_id: "run-2", state: "starting" });
    render(<App />);
    await waitFor(() => expect(desktop.listRuns).toHaveBeenCalled());
    const input = screen.getByTestId("task-input");
    await userEvent.type(input, "写一个测试");
    await userEvent.click(screen.getByTestId("task-submit"));
    await waitFor(() => {
      expect(desktop.startRun).toHaveBeenCalledWith(
        expect.objectContaining({ task: "写一个测试", workspace: "/tmp/demo" }),
      );
    });
  });
});
