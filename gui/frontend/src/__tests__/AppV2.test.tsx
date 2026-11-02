import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const mock = vi.hoisted(() => ({
  listRecentProjects: vi.fn(),
  rememberProject: vi.fn(),
  observeWorkspace: vi.fn(),
  listRuns: vi.fn(),
  inspectRun: vi.fn(),
  pickDirectory: vi.fn(),
  startRun: vi.fn(),
  resumeRun: vi.fn(),
  resolveReconcile: vi.fn(),
  abortRun: vi.fn(),
  cancelRun: vi.fn(),
  submitQueue: vi.fn(),
  listQueue: vi.fn(),
  confirmQueue: vi.fn(),
  cancelQueue: vi.fn(),
  submitSteering: vi.fn(),
  listChanges: vi.fn(),
  diffFile: vi.fn(),
  evidence: vi.fn(),
  saveReviewNote: vi.fn(),
  listProviderPresets: vi.fn(),
  listWorkflows: vi.fn(),
  onStream: vi.fn(() => () => {}),
}));
vi.mock("../desktop/client", () => ({ desktop: mock }));
import AppV2 from "../v2/AppV2";
import { ReconciliationV2 } from "../v2/RecoveryV2";
import { QueuePanelV2 } from "../v2/QueuePanelV2";

const sampleRun = (state = "completed", reconcile = false) => ({
  run_id: "r1",
  session_id: "session1",
  task: "修复登陆",
  title: "修复登陆",
  state, phase: state,
  updated_at: "2026-11-02T00:00:00Z",
  created_at: "2026-11-02T00:00:00Z",
  waiting_approval: false,
  waiting_reconcile: reconcile,
  pinned: false, archived: false,
});

function setup() {
  mock.listRecentProjects.mockResolvedValue([{ path: "/tmp/cairn-a", name: "cairn-a", pinned: true, archived: false, last_used: "" }]);
  mock.rememberProject.mockResolvedValue({ ok: true });
  mock.observeWorkspace.mockResolvedValue({ path: "/tmp/cairn-a", exists: true, has_config: true, config_loaded: true, dirty: false });
  mock.listRuns.mockResolvedValue([sampleRun()]);
  mock.inspectRun.mockResolvedValue({ run_id: "r1", state: "completed", summary: {}, events: [] });
  mock.listChanges.mockResolvedValue({ root: "/tmp/cairn-a", entries: [{ relative: "README.md", status: "modified", agent_owned: true }], added: 0, deleted: 0, modified: 1, untracked: 0 });
  mock.diffFile.mockResolvedValue("diff --git a/README.md b/README.md\n@@ -1,1 +1,1 @@\n-old\n+new");
  mock.evidence.mockResolvedValue({ state: "completed", outcome: "ok", trusted: false, verification: { verification_ok: false }, metrics: { input_tokens: 123, output_tokens: 42, usage_estimated: true } });
  mock.listProviderPresets.mockResolvedValue([]);
  mock.listWorkflows.mockResolvedValue([]);
  mock.listQueue.mockResolvedValue([]);
  mock.saveReviewNote.mockResolvedValue({ ok: true });
  mock.onStream.mockReturnValue(() => {});
}

describe("Cairn Desktop 2.0 — real bridge contracts", () => {
  beforeEach(() => { vi.clearAllMocks(); setup(); });

  it("displays composer-first home with real workspace and no fake runs", async () => {
    render(<AppV2 />);
    expect(screen.getByTestId("cairn-v2-shell")).toBeInTheDocument();
    await waitFor(() => expect(mock.listRuns).toHaveBeenCalledWith({ workspace: "/tmp/cairn-a" }));
    expect(screen.getByRole("heading", { name: "今天想让 Cairn 做什么？" })).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "描述任务" })).toBeInTheDocument();
  });

  it("preserves linked session ID when continuing a completed Run", async () => {
    mock.startRun.mockResolvedValue({ run_id: "r2", state: "starting" });
    render(<AppV2 />);
    await screen.findByRole("button", { name: /修复登陆/ });
    fireEvent.click(screen.getByRole("button", { name: /修复登陆/ }));
    fireEvent.change(screen.getByRole("textbox", { name: "描述任务" }), { target: { value: "写测试" } });
    fireEvent.click(screen.getByRole("button", { name: "提交任务" }));
    await waitFor(() => expect(mock.startRun).toHaveBeenCalledWith(expect.objectContaining({
      workspace: "/tmp/cairn-a", task: "写测试", session_id: "session1", prev_run_id: "r1", require_session_tail: true,
    })));
  });

  it("opens right-hand diff and saves a review note without executing it", async () => {
    render(<AppV2 />);
    await screen.findByRole("button", { name: /修复登陆/ });
    fireEvent.click(screen.getByRole("button", { name: /修复登陆/ }));
    fireEvent.click(screen.getByRole("button", { name: "检查变更" }));
    await waitFor(() => expect(mock.diffFile).toHaveBeenCalledWith("/tmp/cairn-a", "README.md"));
    const lineButton = (await screen.findAllByRole("button", { name: "为第 1 行添加意见" }))[0];
    fireEvent.click(lineButton);
    await userEvent.type(screen.getByRole("textbox", { name: "行级审查意见" }), "请核对变更");
    fireEvent.click(screen.getByRole("button", { name: "保存并带回会话" }));
    await waitFor(() => expect(mock.saveReviewNote).toHaveBeenCalledWith(expect.objectContaining({
      workspace: "/tmp/cairn-a", relative: "README.md", line: "1", body: "请核对变更",
    })));
    expect(mock.startRun).not.toHaveBeenCalled();
  });

  it("does not offer normal retry for a reconcile-required run", async () => {
    mock.listRuns.mockResolvedValue([sampleRun("waiting_reconcile", true)]);
    mock.inspectRun.mockResolvedValue({ run_id: "r1", state: "waiting_reconcile", events: [], summary: {} });
    render(<AppV2 />);
    await screen.findByRole("button", { name: /修复登陆/ });
    fireEvent.click(screen.getByRole("button", { name: /修复登陆/ }));
    expect(await screen.findByTestId("v2-reconcile")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "提交人工对账" })).toBeDisabled();
    expect(mock.resumeRun).not.toHaveBeenCalled();
  });
});

describe("V2 safety-first action panels", () => {
  beforeEach(() => { vi.clearAllMocks(); setup(); });

  it("requires operation, reason and evidence before recording a reconciliation", async () => {
    mock.resolveReconcile.mockResolvedValue({ state: "completed" });
    render(<ReconciliationV2 workspace="/tmp/cairn-a" runId="r1" events={[]} onDone={vi.fn()} onError={vi.fn()} />);
    const submit = screen.getByRole("button", { name: "提交人工对账" });
    expect(submit).toBeDisabled();
    fireEvent.change(screen.getByLabelText("操作 ID"), { target: { value: "op1" } });
    fireEvent.change(screen.getByRole("textbox", { name: "人工核查说明" }), { target: { value: "已检查本地文件" } });
    expect(submit).toBeDisabled();
    fireEvent.change(screen.getByRole("textbox", { name: "证据引用" }), { target: { value: "git:abcd" } });
    fireEvent.click(submit);
    await waitFor(() => expect(mock.resolveReconcile).toHaveBeenCalledWith({
      workspace: "/tmp/cairn-a", run_id: "r1", operation_id: "op1",
      verdict: "abort_run", note: "已检查本地文件", evidence: "git:abcd",
    }));
    expect(mock.resumeRun).not.toHaveBeenCalled();
  });

  it("only starts a pending queued task after explicit confirmation", async () => {
    mock.listQueue.mockResolvedValue([{ input_id: "q1", state: "pending", content_preview: "补测试" }]);
    mock.confirmQueue.mockResolvedValue({ run_id: "r3", state: "running" });
    const onRun = vi.fn();
    render(<QueuePanelV2 workspace="/tmp/cairn-a" revision={0} onRun={onRun} onError={vi.fn()} />);
    await screen.findByText("补测试");
    expect(mock.confirmQueue).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "确认启动" }));
    await waitFor(() => expect(mock.confirmQueue).toHaveBeenCalledWith({ workspace: "/tmp/cairn-a", input_id: "q1" }));
    await waitFor(() => expect(onRun).toHaveBeenCalledWith("r3"));
  });
});
