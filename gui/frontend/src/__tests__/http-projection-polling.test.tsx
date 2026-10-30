/**
 * HTTP 传输通道下运行投影轮询的守卫用例。
 *
 * 背景：events.ts 在 HTTP 模式退化为空订阅句柄（既定失败关闭语义），
 * App 层用有界 Journal 投影轮询补齐审批停点感知。这些用例锁住三条边界：
 * 轮询能发现审批停点并渲染审批卡、到达停点后轮询即停、wails 与演示桥
 * 模式不进入轮询路径。
 */
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
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
import type { DomainEvent, RunHistoryView } from "../desktop/types";

/**
 * 使 listRuns 依调用序返回脚本化的运行列表：脚本耗尽后重复最后一幕，
 * 便于断言「停点之后不再有新调用」。
 */
function scriptRuns(script: RunHistoryView[][]): () => number {
  let calls = 0;
  desktopMock.listRuns.mockImplementation(async () => {
    const index = Math.min(calls, script.length - 1);
    calls += 1;
    return script[index];
  });
  return () => calls;
}

function domainEvent(seq: number, type: string, payload: Record<string, unknown>, runId = "run-2"): DomainEvent {
  return {
    schema_version: 1,
    event_id: `e-${seq}`,
    run_id: runId,
    workspace_hash: "w",
    seq,
    occurred_at: new Date().toISOString(),
    type,
    payload,
    prev_hash: "",
    event_hash: "",
  };
}

const baseRun: RunHistoryView = {
  run_id: "run-2",
  session_id: "s2",
  task: "演示审批闭环",
  state: "running",
  phase: "running",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  title: "演示审批闭环",
  pinned: false,
  archived: false,
  waiting_approval: false,
  waiting_reconcile: false,
};

const waitingApprovalRun: RunHistoryView = {
  ...baseRun,
  state: "running",
  phase: "waiting_approval",
  waiting_approval: true,
};

async function submitDraft() {
  await userEvent.type(screen.getByTestId("composer-input"), "演示审批闭环");
  await userEvent.click(screen.getByTestId("composer-send"));
}

describe("HTTP 模式运行投影轮询", () => {
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
    desktopMock.startRun.mockResolvedValue({ run_id: "run-2", session_id: "s2", state: "starting" });
    desktopMock.inspectRun.mockResolvedValue({
      run_id: "run-2",
      state: "running",
      summary: {},
      events: [
        domainEvent(1, "run_created", { task: "演示审批闭环" }),
        domainEvent(2, "approval_requested", { fingerprint: "fp-demo", summary: "写入 demo/out.txt" }),
      ],
    });
    desktopMock.evidence.mockResolvedValue({
      state: "running",
      outcome: "running",
      trusted: false,
      verification: { verification_ok: false },
      metrics: { turns: 1, tool_calls: 0, input_tokens: 0, output_tokens: 0, usage_estimated: false, context_compactions: 0, wall_time_ms: 0 },
    });
    desktopMock.listQueue.mockResolvedValue([]);
    desktopMock.listFileTree.mockResolvedValue({ name: "r", relative: ".", path: "/tmp/demo", kind: "dir", children: [] });
    desktopMock.listChanges.mockResolvedValue({ root: "/tmp/demo", entries: [], added: 0, deleted: 0, modified: 0, untracked: 0 });
    desktopMock.onStream.mockReturnValue(() => undefined);
  });

  afterEach(() => {
    delete (window as unknown as { __CAIRN_HOST__?: string }).__CAIRN_HOST__;
    delete (window as unknown as { __CAIRN_BRIDGE__?: unknown }).__CAIRN_BRIDGE__;
  });

  it("轮询发现审批停点后渲染审批卡并收起运行指示", async () => {
    // jsdom 无宿主标记与演示入口，真实绑定存在，bridgeMode 为 http。
    const calls = scriptRuns([[], [waitingApprovalRun], [waitingApprovalRun]]);
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await submitDraft();
    expect(desktopMock.startRun).toHaveBeenCalledWith(
      expect.objectContaining({ task: "演示审批闭环", workspace: "/tmp/demo" }),
    );
    // 事件通道在 HTTP 模式是空句柄，审批卡只能来自投影轮询触发的 Journal 重取。
    await waitFor(
      () => {
        expect(screen.getByTestId("approval-card")).toBeInTheDocument();
      },
      { timeout: 6000 },
    );
    await waitFor(
      () => {
        expect(screen.queryByTestId("composer-stop")).not.toBeInTheDocument();
      },
      { timeout: 3000 },
    );
    expect(calls()).toBeGreaterThan(1);
  }, 15000);

  it("到达审批停点后轮询即停，不再发起新的投影请求", async () => {
    scriptRuns([[], [waitingApprovalRun], [waitingApprovalRun]]);
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await submitDraft();
    await waitFor(
      () => {
        expect(screen.getByTestId("approval-card")).toBeInTheDocument();
      },
      { timeout: 6000 },
    );
    const settled = desktopMock.listRuns.mock.calls.length;
    await new Promise((resolve) => setTimeout(resolve, 2600));
    expect(desktopMock.listRuns.mock.calls.length).toBe(settled);
  }, 15000);

  it("演示桥模式不进入轮询路径", async () => {
    (window as unknown as { __CAIRN_BRIDGE__?: unknown }).__CAIRN_BRIDGE__ = { Health: async () => ({}) };
    scriptRuns([[], [waitingApprovalRun]]);
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await submitDraft();
    await new Promise((resolve) => setTimeout(resolve, 2600));
    const afterWait = desktopMock.listRuns.mock.calls.length;
    await new Promise((resolve) => setTimeout(resolve, 1200));
    expect(desktopMock.listRuns.mock.calls.length).toBe(afterWait);
  }, 15000);

  it("wails 宿主模式不进入轮询路径", async () => {
    (window as unknown as { __CAIRN_HOST__?: string }).__CAIRN_HOST__ = "wails";
    scriptRuns([[], [waitingApprovalRun]]);
    render(<App />);
    await waitFor(() => expect(desktopMock.listRuns).toHaveBeenCalled());
    await submitDraft();
    await new Promise((resolve) => setTimeout(resolve, 2600));
    const afterWait = desktopMock.listRuns.mock.calls.length;
    await new Promise((resolve) => setTimeout(resolve, 1200));
    expect(desktopMock.listRuns.mock.calls.length).toBe(afterWait);
  }, 15000);
});
