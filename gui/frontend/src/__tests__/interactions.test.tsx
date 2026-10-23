import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
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
  probeProvider: vi.fn(async () => ({ result: "pass" })),
  writeProjectInstructions: vi.fn(async () => ({ ok: true })),
  inspectMemory: vi.fn(async () => ({ candidate: { id: "c1", outcome: "ok", status: "active" }, audit: [] })),
  onStream: vi.fn((_: (event: StreamEvent) => void) => () => undefined),
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
import type { DomainEvent, RunHistoryView, StreamEvent } from "../desktop/types";

function event(partial: Partial<DomainEvent> & { type: string; seq: number }): DomainEvent {
  return {
    schema_version: 1,
    event_id: `e-${partial.seq}`,
    run_id: "run-1",
    workspace_hash: "w",
    occurred_at: new Date().toISOString(),
    payload: {},
    prev_hash: "",
    event_hash: "",
    ...partial,
  };
}

const run1: RunHistoryView = {
  run_id: "run-1",
  session_id: "s1",
  task: "修复登录",
  state: "running",
  phase: "running",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  title: "修复登录",
  pinned: false,
  archived: false,
  waiting_approval: false,
  waiting_reconcile: false,
};

const run2: RunHistoryView = {
  ...run1,
  run_id: "run-2",
  title: "写文档",
  state: "completed",
  phase: "completed",
};

function mockBase() {
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
  desktopMock.listRuns.mockResolvedValue([run1, run2]);
  desktopMock.evidence.mockResolvedValue({
    state: "completed",
    outcome: "ok",
    trusted: true,
    verification: { verification_ok: true, verification_seq: 3 },
    metrics: { turns: 2, tool_calls: 1, input_tokens: 10, output_tokens: 20, usage_estimated: false, context_compactions: 0, wall_time_ms: 5 },
  });
  desktopMock.listQueue.mockResolvedValue([]);
  desktopMock.listFileTree.mockResolvedValue({ name: "r", relative: ".", path: "/tmp/demo", kind: "dir", children: [] });
  desktopMock.listChanges.mockResolvedValue({ root: "/tmp/demo", entries: [], added: 0, deleted: 0, modified: 0, untracked: 0 });
  desktopMock.doctor.mockResolvedValue({ ready: true });
  desktopMock.mcpStatus.mockResolvedValue([]);
  desktopMock.listSkills.mockResolvedValue([]);
  desktopMock.onStream.mockReturnValue(() => undefined);
}

describe("interactions", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockBase();
  });

  it("opens settings drawer and toggles read-only", async () => {
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await userEvent.click(screen.getByTestId("open-settings"));
    expect(screen.getByTestId("settings-drawer")).toBeInTheDocument();
    await waitFor(() => expect(desktopMock.doctor).toHaveBeenCalled());
    const toggle = screen.getByTestId("readonly-toggle");
    await userEvent.click(toggle);
    expect(toggle).toBeChecked();
  });

  it("queues a follow-up via composer", async () => {
    desktopMock.submitQueue.mockResolvedValue({ input_id: "q-1", state: "pending" });
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await userEvent.type(screen.getByTestId("composer-input"), "稍后改测试");
    await userEvent.click(screen.getByTestId("composer-queue"));
    await waitFor(() => {
      expect(desktopMock.submitQueue).toHaveBeenCalledWith(
        expect.objectContaining({ content: "稍后改测试", workspace: "/tmp/demo" }),
      );
    });
  });

  it("shows approval card and resumes with fingerprint", async () => {
    desktopMock.inspectRun.mockResolvedValue({
      run_id: "run-1",
      state: "running",
      summary: {},
      events: [
        event({ type: "run_created", seq: 1, payload: { task: "修复登录" } }),
        event({
          type: "approval_requested",
          seq: 2,
          payload: { fingerprint: "fp-1", summary: "写入 login.go" },
          operation_id: "op-1",
        }),
      ],
    });
    desktopMock.resumeRun.mockResolvedValue({ run_id: "run-1", state: "resuming" });
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await userEvent.click(screen.getByTestId("thread-list").querySelector("button")!);
    await waitFor(() => {
      expect(desktopMock.inspectRun).toHaveBeenCalled();
      expect(screen.getByText("需要你的确认")).toBeInTheDocument();
    });
    await userEvent.click(screen.getByRole("button", { name: "允许并继续" }));
    await waitFor(() => {
      expect(desktopMock.resumeRun).toHaveBeenCalledWith(
        expect.objectContaining({ run_id: "run-1", approve: true, fingerprint: "fp-1" }),
      );
    });
  });

  it("switching threads does not mix run events", async () => {
    desktopMock.inspectRun.mockImplementation(async (req: { run_id: string }) => ({
      run_id: req.run_id,
      state: "completed",
      summary: {},
      events: [event({ type: "run_created", seq: 1, payload: { task: req.run_id }, run_id: req.run_id })],
    }));
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await userEvent.click(screen.getByRole("button", { name: /写文档/ }));
    await waitFor(() => expect(desktopMock.inspectRun).toHaveBeenCalled());
    const transcript = document.querySelector(".transcript");
    expect(transcript?.textContent).toContain("run-2");
    expect(transcript?.textContent).not.toContain("run-1");
  });
});


describe("review regressions", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
    mockBase();
    desktopMock.inspectRun.mockResolvedValue({ run_id: "run-1", state: "completed", summary: {}, events: [] });
  });

  it("Ctrl+Enter queues exactly once without also starting a run", async () => {
    desktopMock.submitQueue.mockResolvedValue({ input_id: "q", state: "pending" });
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    const input = screen.getByTestId("composer-input");
    await userEvent.type(input, "queued task");
    fireEvent.keyDown(input, { key: "Enter", ctrlKey: true });
    await waitFor(() => expect(desktopMock.submitQueue).toHaveBeenCalledTimes(1));
    expect(desktopMock.startRun).not.toHaveBeenCalled();
  });

  it("preserves both failed queued text and new input typed while awaiting the response", async () => {
    let rejectQueue!: (reason: Error) => void;
    desktopMock.submitQueue.mockImplementation(() => new Promise((_, reject) => { rejectQueue = reject; }));
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    const input = screen.getByTestId("composer-input");
    await userEvent.type(input, "original");
    await userEvent.click(screen.getByTestId("composer-queue"));
    await userEvent.type(input, "new draft");
    await act(async () => rejectQueue(new Error("queue failed")));
    expect(input).toHaveValue("original\nnew draft");
  });

  it("clears previous run content before a different run inspection resolves", async () => {
    desktopMock.inspectRun.mockImplementation((request: { run_id: string }) => request.run_id === "run-1"
      ? Promise.resolve({ run_id: "run-1", state: "completed", summary: {}, events: [event({ type: "run_created", seq: 1, payload: { task: "OLD_RUN_BODY" } })] })
      : new Promise(() => undefined));
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await userEvent.click(screen.getByRole("button", { name: /修复登录/ }));
    await waitFor(() => expect(document.querySelector(".transcript")?.textContent).toContain("OLD_RUN_BODY"));
    await userEvent.click(screen.getByRole("button", { name: /写文档/ }));
    expect(document.querySelector(".transcript")?.textContent || "").not.toContain("OLD_RUN_BODY");
  });

  it("allows retry after approval transport failure and preserves recorded overrides", async () => {
    desktopMock.inspectRun.mockResolvedValue({ run_id: "run-1", state: "running", summary: {}, events: [
      event({ type: "run_created", seq: 1, payload: { task: "approval" } }),
      event({ type: "approval_requested", seq: 2, operation_id: "op", payload: { fingerprint: "fp", summary: "write" } }),
    ] });
    desktopMock.resumeRun.mockRejectedValueOnce(new Error("transport failed")).mockResolvedValue({ run_id: "run-1", state: "resuming" });
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await userEvent.click(screen.getByRole("button", { name: /修复登录/ }));
    const allow = await screen.findByTestId("approval-allow");
    await userEvent.click(allow);
    await waitFor(() => expect(allow).not.toBeDisabled());
    await userEvent.click(allow);
    await waitFor(() => expect(desktopMock.resumeRun).toHaveBeenCalledTimes(2));
    expect(desktopMock.resumeRun.mock.calls[0][0]).not.toHaveProperty("mode");
    expect(desktopMock.resumeRun.mock.calls[0][0]).not.toHaveProperty("model");
  });

  it("ignores foreign workspace completion and stale A-to-B-to-A run responses", async () => {
    const a = "/tmp/demo", b = "/tmp/other";
    const recent = [{ path: a, name: "demo" }, { path: b, name: "other" }];
    desktopMock.listRecentProjects.mockResolvedValue(recent);
    desktopMock.observeWorkspace.mockImplementation(async (path: string) => ({ path, exists: true, has_config: true, config_loaded: true }));
    let firstA = true;
    let releaseOld!: (runs: RunHistoryView[]) => void;
    desktopMock.listRuns.mockImplementation((request: { workspace: string }) => {
      if (request.workspace === a && firstA) {
        firstA = false;
        return new Promise((resolve) => { releaseOld = resolve; });
      }
      return Promise.resolve([{ ...run2, run_id: request.workspace === a ? "current-a" : "current-b", title: request.workspace === a ? "CURRENT_A" : "CURRENT_B" }]);
    });
    let handler!: (event: StreamEvent) => void;
    desktopMock.onStream.mockImplementation((next) => { handler = next; return () => undefined; });
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalledWith(expect.objectContaining({ workspace: a })));
    await userEvent.click(screen.getByTestId("recent-other"));
    await waitFor(() => expect(screen.getByTestId("thread-list")).toHaveTextContent("CURRENT_B"));
    await act(async () => handler({ workspace: a, run_id: "foreign", kind: "run.event", event: event({ run_id: "foreign", type: "assistant_message_appended", seq: 1, payload: { text: "FOREIGN_BODY" } }) }));
    expect(document.querySelector(".transcript")?.textContent || "").not.toContain("FOREIGN_BODY");
    expect(desktopMock.inspectRun).not.toHaveBeenCalledWith(expect.objectContaining({ run_id: "foreign" }));
    await userEvent.click(screen.getByTestId("recent-demo"));
    await waitFor(() => expect(screen.getByTestId("thread-list")).toHaveTextContent("CURRENT_A"));
    await act(async () => releaseOld([{ ...run1, title: "STALE_A" }]));
    expect(screen.getByTestId("thread-list")).toHaveTextContent("CURRENT_A");
    expect(screen.getByTestId("thread-list")).not.toHaveTextContent("STALE_A");
  });
});
