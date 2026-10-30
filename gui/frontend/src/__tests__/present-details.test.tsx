import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const desktopMock = vi.hoisted(() => ({
  listFileTree: vi.fn(),
  listChanges: vi.fn(),
  listQueue: vi.fn(async () => []),
  previewFile: vi.fn(),
  diffFile: vi.fn(async () => ""),
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

import { InspectorRail } from "../components/InspectorRail";
import { ThreadSidebar } from "../components/ChatChrome";
import type { WorkspaceStatus } from "../desktop/types";

const workspace: WorkspaceStatus = {
  path: "/tmp/demo",
  exists: true,
  is_workspace: true,
  has_agent_dir: true,
  has_config: true,
  dirty: true,
  state_dir: "/tmp/demo/.cairn",
  config_path: "/tmp/demo/.agent/config.yaml",
  config_loaded: true,
};

function renderRail(tab: "files" | "changes") {
  return render(
    <InspectorRail
      tab={tab}
      onTab={() => undefined}
      workspace={workspace}
      runId={null}
      statusLabel=""
      onError={() => undefined}
      onRefreshThreads={() => undefined}
      onQueueAction={() => undefined}
    />,
  );
}

function renderSidebar(props: { workspaceDirty?: boolean; health?: unknown }) {
  return render(
    <ThreadSidebar
      threads={[]}
      activeId={null}
      query=""
      onQuery={() => undefined}
      onSelect={() => undefined}
      onNew={() => undefined}
      onContext={() => undefined}
      workspacePath="/tmp/demo"
      onPickWorkspace={() => undefined}
      workspaceLabel="demo"
      workspaceDirty={props.workspaceDirty}
      health={props.health as never}
    />,
  );
}

describe("界面呈现细节补齐", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    desktopMock.listFileTree.mockResolvedValue({
      name: "r",
      relative: ".",
      path: "/tmp/demo",
      kind: "dir",
      children: [{ name: "main.go", relative: "main.go", path: "/tmp/demo/main.go", kind: "file", size: 2048 }],
    });
    desktopMock.listChanges.mockResolvedValue({
      root: "/tmp/demo",
      entries: [{ relative: "main.go", status: "M", agent_owned: true }],
      added: 1,
      deleted: 2,
      modified: 3,
      untracked: 4,
    });
  });

  it("改动页签展示后端给出的汇总计数", async () => {
    renderRail("changes");
    await waitFor(() => {
      expect(screen.getByTestId("change-summary")).toHaveTextContent("新增 1");
    });
    const summary = screen.getByTestId("change-summary");
    expect(summary).toHaveTextContent("修改 3");
    expect(summary).toHaveTextContent("删除 2");
    expect(summary).toHaveTextContent("未跟踪 4");
  });

  it("文件预览展示语言、体积与截断标记", async () => {
    desktopMock.previewFile.mockResolvedValue({
      relative: "main.go",
      path: "/tmp/demo/main.go",
      language: "go",
      size: 2048,
      truncated: true,
      binary: false,
      text: "package main",
    });
    renderRail("files");
    await waitFor(() => expect(screen.getByText("main.go")).toBeInTheDocument());
    await userEvent.click(screen.getByText("main.go"));
    await waitFor(() => {
      expect(screen.getByTestId("preview-meta")).toHaveTextContent("go");
    });
    expect(screen.getByTestId("preview-meta")).toHaveTextContent("2.0 KB");
    expect(screen.getByTestId("preview-meta")).toHaveTextContent("内容已截断");
  });

  it("二进制文件如实标记为未展示", async () => {
    desktopMock.previewFile.mockResolvedValue({
      relative: "main.go",
      path: "/tmp/demo/main.go",
      size: 900,
      truncated: false,
      binary: true,
      reason: "二进制文件",
    });
    renderRail("files");
    await waitFor(() => expect(screen.getByText("main.go")).toBeInTheDocument());
    await userEvent.click(screen.getByText("main.go"));
    await waitFor(() => {
      expect(screen.getByTestId("preview-meta")).toHaveTextContent("二进制未展示");
    });
  });

  it("侧栏展示未提交标记与桥接健康出口", () => {
    renderSidebar({
      workspaceDirty: true,
      health: { ok: true, app_name: "Cairn", version: "0.1.0", go_version: "go1.26" },
    });
    expect(screen.getByTestId("workspace-dirty")).toHaveTextContent("未提交");
    expect(screen.getByTestId("bridge-status")).toHaveTextContent("桥接 0.1.0");
  });

  it("工作区干净时不显示未提交标记，桥接未连通时如实说明", () => {
    renderSidebar({ workspaceDirty: false, health: null });
    expect(screen.queryByTestId("workspace-dirty")).not.toBeInTheDocument();
    expect(screen.getByTestId("bridge-status")).toHaveTextContent("桥接未连接");
  });
});
