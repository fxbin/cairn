/**
 * Desktop 2.0 第三批迁移测试：会话管理（重命名/置顶/归档/分支）、
 * 文件面板（树/预览/打开位置）与子代理父级导航。
 * 只通过真实 desktop bridge mock 验证合同，不构造本地成功状态。
 */
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

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
  renameSession: vi.fn(),
  setSessionFlags: vi.fn(),
  listFileTree: vi.fn(),
  previewFile: vi.fn(),
  openPathInEditor: vi.fn(),
  listOpenLocations: vi.fn(),
  openInLocation: vi.fn(),
  onStream: vi.fn(() => () => {}),
}));
vi.mock("../desktop/client", () => ({ desktop: mock }));
import AppV2 from "../v2/AppV2";
import { FilePanelV2 } from "../v2/FilePanelV2";

const runOf = (overrides: Partial<Record<string, unknown>> = {}) => ({
  run_id: "r1",
  session_id: "session1",
  task: "修复登陆",
  title: "修复登陆",
  state: "completed",
  phase: "completed",
  updated_at: "2026-11-02T00:00:00Z",
  created_at: "2026-11-02T00:00:00Z",
  waiting_approval: false,
  waiting_reconcile: false,
  pinned: false,
  archived: false,
  ...overrides,
});

const fileTree = () => ({
  name: "cairn-a",
  relative: ".",
  path: "/tmp/cairn-a",
  kind: "dir",
  children: [
    { name: "README.md", relative: "README.md", path: "/tmp/cairn-a/README.md", kind: "file", size: 900000 },
    { name: "assets", relative: "assets", path: "/tmp/cairn-a/assets", kind: "dir", children: [
      { name: "logo.png", relative: "assets/logo.png", path: "/tmp/cairn-a/assets/logo.png", kind: "file", size: 4096 },
    ] },
  ],
});

const delegatedEvent = {
  schema_version: 1,
  event_id: "e-sub-1",
  run_id: "r1",
  workspace_hash: "hash",
  seq: 3,
  occurred_at: "2026-11-02T00:00:01Z",
  type: "subagent_delegated",
  payload: { child_run_id: "c1", task_digest: "整理文档" },
  prev_hash: "p",
  event_hash: "h",
};

function setup() {
  mock.listRecentProjects.mockResolvedValue([{ path: "/tmp/cairn-a", name: "cairn-a", pinned: true, archived: false, last_used: "" }]);
  mock.rememberProject.mockResolvedValue({ ok: true });
  mock.observeWorkspace.mockResolvedValue({ path: "/tmp/cairn-a", exists: true, has_config: true, config_loaded: true, dirty: false });
  mock.listRuns.mockResolvedValue([runOf()]);
  mock.inspectRun.mockResolvedValue({ run_id: "r1", state: "completed", summary: {}, events: [] });
  mock.evidence.mockResolvedValue({ state: "completed", outcome: "ok", trusted: false, verification: { verification_ok: false }, metrics: { input_tokens: 1, output_tokens: 1, usage_estimated: false } });
  mock.listFileTree.mockResolvedValue(fileTree());
  mock.previewFile.mockResolvedValue({ relative: "README.md", path: "/tmp/cairn-a/README.md", size: 900000, truncated: true, binary: false, text: "# Cairn", language: "markdown" });
  mock.openPathInEditor.mockResolvedValue({ ok: true });
  mock.listOpenLocations.mockResolvedValue([
    { id: "finder", label: "Finder", available: true },
    { id: "cursor", label: "Cursor", available: false },
  ]);
  mock.openInLocation.mockResolvedValue({ ok: true });
  mock.renameSession.mockResolvedValue({ ok: true });
  mock.setSessionFlags.mockResolvedValue({ ok: true });
  mock.startRun.mockResolvedValue({ run_id: "r9", state: "starting" });
  mock.onStream.mockReturnValue(() => {});
}

/** 选中侧栏会话并等待会话页同步切换；运行 detail 由既有 2s 轮询补齐。 */
const selectFirstSession = async () => {
  const entry = await screen.findByRole("button", { name: /修复登陆/ });
  fireEvent.click(entry);
  await screen.findByRole("heading", { name: "修复登陆" });
};

/** 等待工作区就绪（侧栏项目装载完成）再打开文件面板。 */
const awaitWorkspaceReady = async () => {
  await waitFor(() => expect(mock.observeWorkspace).toHaveBeenCalledWith("/tmp/cairn-a"));
};

/** 打开文件面板并等待树渲染完成。 */
const openFilePanel = async () => {
  fireEvent.click(screen.getByRole("button", { name: "文件" }));
  await waitFor(() => expect(mock.listFileTree).toHaveBeenCalledWith("/tmp/cairn-a"));
  await screen.findByRole("button", { name: "README.md" });
};

/** 等待子代理面板出现父运行分组；detail 事件经 2s 轮询到达，需要更长超时。 */
const awaitSubagentGroup = async () => {
  await waitFor(() => {
    const panel = document.querySelector('[data-testid="v2-subagents"]');
    expect(panel?.textContent).toContain("父运行 r1");
  }, { timeout: 3500 });
};

describe("V2 会话管理：重命名 / 置顶 / 归档", () => {
  beforeEach(() => { vi.clearAllMocks(); setup(); });

  it("行内重命名走 renameSession 并刷新列表", async () => {
    render(<AppV2 />);
    await selectFirstSession();
    const refreshCount = mock.listRuns.mock.calls.length;
    fireEvent.click(screen.getByRole("button", { name: "重命名会话 session1" }));
    fireEvent.change(screen.getByLabelText("会话新标题"), { target: { value: "登陆修复复盘" } });
    fireEvent.click(screen.getByRole("button", { name: "保存会话标题 session1" }));
    await waitFor(() => expect(mock.renameSession).toHaveBeenCalledWith("session1", "登陆修复复盘"));
    await waitFor(() => expect(mock.listRuns.mock.calls.length).toBeGreaterThan(refreshCount));
  });

  it("置顶与归档走 setSessionFlags，归档仅切换视图不删数据", async () => {
    render(<AppV2 />);
    await selectFirstSession();
    fireEvent.click(screen.getByRole("button", { name: "置顶 session1" }));
    await waitFor(() => expect(mock.setSessionFlags).toHaveBeenCalledWith("session1", true, false));
    fireEvent.click(screen.getByRole("button", { name: "归档会话 session1" }));
    await waitFor(() => expect(mock.setSessionFlags).toHaveBeenCalledWith("session1", false, true));
    expect(mock.setSessionFlags).toHaveBeenCalledTimes(2);
  });

  it("已归档会话默认隐藏，开关经 include_archived 请求", async () => {
    const archived = runOf({ run_id: "r2", session_id: "session2", task: "旧任务", title: "旧任务", archived: true });
    mock.listRuns.mockImplementation((request: { include_archived?: boolean }) =>
      request && request.include_archived ? [runOf(), archived] : [runOf()]);
    render(<AppV2 />);
    await selectFirstSession();
    expect(screen.queryByText("旧任务")).not.toBeInTheDocument();
    expect(mock.listRuns).toHaveBeenLastCalledWith(expect.not.objectContaining({ include_archived: true }));
    fireEvent.click(screen.getByRole("button", { name: "显示已归档会话" }));
    await waitFor(() => expect(mock.listRuns).toHaveBeenLastCalledWith(expect.objectContaining({ include_archived: true })));
    expect(await screen.findByText("旧任务")).toBeInTheDocument();
    expect(screen.getByText("已归档")).toBeInTheDocument();
  });

  it("缺少 session_id 的历史项不提供可操作的展示偏好按钮", async () => {
    mock.listRuns.mockResolvedValue([runOf({ session_id: undefined })]);
    render(<AppV2 />);
    await screen.findByRole("button", { name: /修复登陆/ });
    expect(screen.getByRole("button", { name: "重命名会话 r1" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "置顶 r1" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "归档会话 r1" })).toBeDisabled();
  });
});

describe("V2 分支（fork）与续接显式区分", () => {
  beforeEach(() => { vi.clearAllMocks(); setup(); });

  it("分支发送携带 fork_source_run_id 且不带 session 链", async () => {
    render(<AppV2 />);
    await selectFirstSession();
    expect(screen.getByText("发送将续接当前会话")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "从此会话分支" }));
    const banner = screen.getByTestId("v2-fork-banner");
    expect(banner.textContent).toContain("不续接");
    fireEvent.change(screen.getByRole("textbox", { name: "描述任务" }), { target: { value: "从当前会话拉一个新分支" } });
    fireEvent.click(screen.getByRole("button", { name: "提交任务" }));
    await waitFor(() => expect(mock.startRun).toHaveBeenCalledTimes(1));
    const request = mock.startRun.mock.calls[0][0] as Record<string, unknown>;
    expect(request).toEqual(expect.objectContaining({ workspace: "/tmp/cairn-a", task: "从当前会话拉一个新分支", fork_source_run_id: "r1" }));
    expect(request.session_id).toBeUndefined();
    expect(request.prev_run_id).toBeUndefined();
    expect(request.require_session_tail).toBeFalsy();
  });

  it("默认发送仍续接 session 链且不携带 fork_source_run_id", async () => {
    render(<AppV2 />);
    await selectFirstSession();
    fireEvent.change(screen.getByRole("textbox", { name: "描述任务" }), { target: { value: "继续修" } });
    fireEvent.click(screen.getByRole("button", { name: "提交任务" }));
    await waitFor(() => expect(mock.startRun).toHaveBeenCalledTimes(1));
    const request = mock.startRun.mock.calls[0][0] as Record<string, unknown>;
    expect(request).toEqual(expect.objectContaining({ session_id: "session1", prev_run_id: "r1", require_session_tail: true }));
    expect(request.fork_source_run_id).toBeUndefined();
  });

  it("切换会话会取消未发送的分支模式", async () => {
    mock.listRuns.mockResolvedValue([runOf(), runOf({ run_id: "r2", session_id: "session2", task: "另一个任务", title: "另一个任务" })]);
    render(<AppV2 />);
    await selectFirstSession();
    fireEvent.click(screen.getByRole("button", { name: "从此会话分支" }));
    expect(screen.getByTestId("v2-fork-banner")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /另一个任务/ }));
    await screen.findByRole("heading", { name: "另一个任务" });
    expect(screen.queryByTestId("v2-fork-banner")).not.toBeInTheDocument();
    expect(screen.getByText("发送将续接当前会话")).toBeInTheDocument();
  });
});

describe("V2 文件面板：预览与打开位置", () => {
  beforeEach(() => { vi.clearAllMocks(); setup(); });

  it("文件树浏览后预览如实标注截断与二进制", async () => {
    render(<AppV2 />);
    await awaitWorkspaceReady();
    await openFilePanel();
    fireEvent.click(screen.getByRole("button", { name: "README.md" }));
    await waitFor(() => expect(mock.previewFile).toHaveBeenCalledWith("/tmp/cairn-a", "README.md"));
    expect(await screen.findByTestId("v2-preview-meta")).toHaveTextContent("内容已截断");
    mock.previewFile.mockResolvedValue({ relative: "assets/logo.png", path: "/tmp/cairn-a/assets/logo.png", size: 4096, truncated: false, binary: true });
    fireEvent.click(screen.getByRole("button", { name: "assets" }));
    fireEvent.click(screen.getByRole("button", { name: "logo.png" }));
    await waitFor(() => expect(mock.previewFile).toHaveBeenCalledWith("/tmp/cairn-a", "assets/logo.png"));
    expect(await screen.findByText("该文件为二进制内容，按安全边界不展示原文。")).toBeInTheDocument();
    expect(screen.getByTestId("v2-preview-meta")).toHaveTextContent("二进制未展示");
  });

  it("编辑器打开仅在选中文件后可用，成功状态如实展示", async () => {
    render(<AppV2 />);
    await awaitWorkspaceReady();
    await openFilePanel();
    const openEditor = screen.getByRole("button", { name: "在编辑器打开" });
    expect(openEditor).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "README.md" }));
    await waitFor(() => expect(mock.previewFile).toHaveBeenCalled());
    fireEvent.click(screen.getByRole("button", { name: "在编辑器打开" }));
    await waitFor(() => expect(mock.openPathInEditor).toHaveBeenCalledWith("/tmp/cairn-a", "README.md"));
    expect(await screen.findByRole("status")).toHaveTextContent("已请求系统编辑器打开");
  });

  it("打开位置禁用不可用目标，失败关闭并可见报错", async () => {
    render(<AppV2 />);
    await awaitWorkspaceReady();
    await openFilePanel();
    fireEvent.click(screen.getByRole("button", { name: "README.md" }));
    await waitFor(() => expect(mock.previewFile).toHaveBeenCalled());
    fireEvent.click(screen.getByRole("button", { name: "打开位置 ▾" }));
    await waitFor(() => expect(mock.listOpenLocations).toHaveBeenCalledWith("/tmp/cairn-a"));
    expect(screen.getByRole("button", { name: "Finder" })).toBeEnabled();
    expect(screen.getByRole("button", { name: /Cursor（不可用）/ })).toBeDisabled();
    mock.openInLocation.mockRejectedValueOnce(new Error("unknown open location target"));
    fireEvent.click(screen.getByRole("button", { name: "Finder" }));
    await waitFor(() => expect(mock.openInLocation).toHaveBeenCalledWith("/tmp/cairn-a", "README.md", "finder"));
    expect(await screen.findByRole("alert")).toHaveTextContent("unknown open location target");
  });

  it("探测失败不伪造任何可用目标", async () => {
    mock.listOpenLocations.mockRejectedValue(new Error("probe failed"));
    render(<FilePanelV2 workspace="/tmp/cairn-a" seed="" onError={vi.fn()} />);
    await waitFor(() => expect(mock.listFileTree).toHaveBeenCalled());
    fireEvent.click(screen.getByRole("button", { name: "打开位置 ▾" }));
    expect(await screen.findByTestId("v2-open-location-menu")).toHaveTextContent("没有可用目标");
  });

  it("检查变更的文件可跳转到文件面板预览", async () => {
    mock.listChanges.mockResolvedValue({ root: "/tmp/cairn-a", entries: [{ relative: "README.md", status: "modified", agent_owned: true }], added: 0, deleted: 0, modified: 1, untracked: 0 });
    mock.diffFile.mockResolvedValue("diff --git");
    render(<AppV2 />);
    await selectFirstSession();
    fireEvent.click(screen.getByRole("button", { name: "检查变更" }));
    fireEvent.click(await screen.findByRole("button", { name: "预览文件 README.md" }));
    await waitFor(() => expect(mock.previewFile).toHaveBeenCalledWith("/tmp/cairn-a", "README.md"));
    expect(await screen.findByTestId("v2-file-panel")).toBeInTheDocument();
  });
});

describe("V2 子代理父级导航", () => {
  beforeEach(() => { vi.clearAllMocks(); setup(); });

  it("按父运行分组展示，用量未知如实标注", async () => {
    mock.inspectRun.mockResolvedValue({ run_id: "r1", state: "completed", summary: {}, events: [delegatedEvent] });
    render(<AppV2 />);
    await selectFirstSession();
    fireEvent.click(screen.getByRole("button", { name: "子智能体" }));
    await awaitSubagentGroup();
    const panel = screen.getByTestId("v2-subagents");
    expect(panel.textContent).toContain("整理文档");
    expect(panel.textContent).toContain("用量未知");
  });

  it("进入子运行显示面包屑并可返回父运行", async () => {
    mock.inspectRun.mockResolvedValue({ run_id: "r1", state: "completed", summary: {}, events: [delegatedEvent] });
    render(<AppV2 />);
    await selectFirstSession();
    fireEvent.click(screen.getByRole("button", { name: "子智能体" }));
    await awaitSubagentGroup();
    fireEvent.click(screen.getByRole("button", { name: "查看子运行" }));
    const crumb = await screen.findByTestId("v2-parent-crumb");
    expect(crumb.textContent).toContain("父运行 r1");
    expect(crumb.textContent).toContain("子运行 c1");
    fireEvent.click(screen.getByRole("button", { name: "返回父运行" }));
    await screen.findByRole("heading", { name: "修复登陆" });
    expect(screen.queryByTestId("v2-parent-crumb")).not.toBeInTheDocument();
    await waitFor(() => {
      const parentInspections = mock.inspectRun.mock.calls.filter((call) => (call[0] as { run_id?: string }).run_id === "r1");
      expect(parentInspections.length).toBeGreaterThan(1);
    }, { timeout: 3500 });
  });
});
