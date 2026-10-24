import { useCallback, useEffect, useState } from "react";
import { OpenLocationMenu } from "./OpenLocationMenu";
import { desktop } from "../desktop/client";
import type { ChangeSet, ErrorBody, FileNode, FilePreview, WorkspaceStatus } from "../desktop/types";

/**
 * 文件/改动面板：有界文件树、预览与工作区改动。
 * 归属区分只展示证据，不猜测 Agent 改动。
 */
export function FilesPanel({
  workspace,
  onError,
}: {
  workspace: WorkspaceStatus | null;
  onError: (error: ErrorBody) => void;
}) {
  const [tree, setTree] = useState<FileNode | null>(null);
  const [preview, setPreview] = useState<FilePreview | null>(null);
  const [changes, setChanges] = useState<ChangeSet | null>(null);
  const [diff, setDiff] = useState<string>("");
  const [diffPath, setDiffPath] = useState<string>("");
  const [tab, setTab] = useState<"tree" | "changes" | "preview" | "diff">("tree");
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    if (!workspace?.exists) {
      setTree(null);
      setChanges(null);
      return;
    }
    try {
      const [treeResult, changeResult] = await Promise.all([
        desktop.listFileTree(workspace.path),
        desktop.listChanges(workspace.path),
      ]);
      setTree(treeResult);
      setChanges(changeResult);
      setError(null);
    } catch (err) {
      const body =
        err && typeof err === "object" && "body" in err
          ? (err as { body: ErrorBody }).body
          : { code: "internal", message: String(err) };
      setError(body.message);
      onError(body);
    }
  }, [workspace?.exists, workspace?.path]);

  useEffect(() => {
    void load();
  }, [load]);

  const openPreview = async (relative: string) => {
    if (!workspace?.path) {
      return;
    }
    try {
      const result = await desktop.previewFile(workspace.path, relative);
      setPreview(result);
      setTab("preview");
    } catch (err) {
      const body =
        err && typeof err === "object" && "body" in err
          ? (err as { body: ErrorBody }).body
          : { code: "internal", message: String(err) };
      setError(body.message);
    }
  };

  const openDiff = async (relative: string) => {
    if (!workspace?.path) {
      return;
    }
    try {
      const text = await desktop.diffFile(workspace.path, relative);
      setDiff(text);
      setDiffPath(relative);
      setTab("diff");
    } catch (err) {
      const body =
        err && typeof err === "object" && "body" in err
          ? (err as { body: ErrorBody }).body
          : { code: "internal", message: String(err) };
      setError(body.message);
    }
  };

  const renderNodes = (node: FileNode, depth = 0) => {
    if (!node.children?.length && depth > 0) {
      return null;
    }
    return (
      <ul className={depth === 0 ? "space-y-0.5" : "ml-3 space-y-0.5"}>
        {(node.children || []).map((child) => (
          <li key={child.relative || child.name}>
            <button
              type="button"
              className="w-full truncate rounded px-1 py-0.5 text-left text-xs hover:bg-[var(--c-bg-panel)] disabled:opacity-50"
              disabled={child.kind === "blocked"}
              onClick={() => {
                if (child.kind === "file") {
                  void openPreview(child.relative);
                }
              }}
              title={child.reason || child.relative}
            >
              {child.kind === "dir" ? "📁 " : child.kind === "blocked" ? "⛔ " : "📄 "}
              {child.name}
              {child.reason ? ` (${child.reason})` : ""}
            </button>
            {child.kind === "dir" && renderNodes(child, depth + 1)}
          </li>
        ))}
      </ul>
    );
  };

  return (
    <section className="panel flex min-h-0 flex-col p-3">
      <div className="mb-2 flex items-center gap-2">
        <h2 className="text-sm font-semibold">文件 / 改动</h2>
        {workspace?.path && (
          <OpenLocationMenu workspace={workspace.path} relative={diffPath || "."} />
        )}
        <div className="ml-auto flex gap-1">
          {(
            [
              ["tree", "文件树"],
              ["changes", "改动"],
              ["preview", "预览"],
              ["diff", "Diff"],
            ] as const
          ).map(([id, label]) => (
            <button
              key={id}
              type="button"
              className={`btn ${tab === id ? "btn-primary" : ""}`}
              onClick={() => setTab(id)}
            >
              {label}
            </button>
          ))}
          <button type="button" className="btn" onClick={() => void load()}>
            刷新
          </button>
        </div>
      </div>
      {error && <div className="mb-2 text-xs text-[var(--c-warning)]">{error}</div>}
      {!workspace?.exists && (
        <div className="muted text-xs" data-testid="files-empty">
          选择有效工作区后显示文件树与改动。
        </div>
      )}
      {workspace?.exists && tab === "tree" && (
        <div className="scroll-y min-h-0 flex-1" data-testid="file-tree">
          {tree ? renderNodes(tree) : <div className="muted text-xs">加载中…</div>}
        </div>
      )}
      {workspace?.exists && tab === "changes" && (
        <div className="scroll-y min-h-0 flex-1 text-xs" data-testid="change-list">
          {!changes || changes.entries.length === 0 ? (
            <div className="muted">工作区无改动。</div>
          ) : (
            <ul className="space-y-1">
              {changes.entries.map((entry) => (
                <li key={entry.relative} className="rounded border border-[var(--c-border)] px-2 py-1">
                  <div className="flex justify-between gap-2">
                    <span className="truncate">{entry.relative}</span>
                    <span className="muted">{entry.status}</span>
                  </div>
                  <div className="muted">
                    归属：{entry.agent_owned ? "Agent 改动" : "未确认"} · 证据：{entry.evidence || "无"}
                  </div>
                  <button
                    type="button"
                    className="btn mt-1"
                    onClick={() => void openDiff(entry.relative)}
                  >
                    查看 Diff
                  </button>
                </li>
              ))}
            </ul>
          )}
          {changes && (
            <div className="muted mt-2">
              修改 {changes.modified} · 删除 {changes.deleted} · 未跟踪 {changes.untracked}
            </div>
          )}
        </div>
      )}
      {workspace?.exists && tab === "preview" && (
        <div className="scroll-y min-h-0 flex-1 text-xs" data-testid="file-preview">
          {!preview && <div className="muted">从文件树选择文件以预览。</div>}
          {preview && (
            <>
              <div className="muted mb-1">
                {preview.relative} · {preview.language}
                {preview.truncated ? " · 已截断" : ""}
                {preview.binary ? " · 二进制" : ""}
              </div>
              <pre className="whitespace-pre-wrap rounded bg-[var(--c-bg)] p-2">{preview.text || preview.reason || "（空）"}</pre>
            </>
          )}
        </div>
      )}
      {workspace?.exists && tab === "diff" && (
        <div className="scroll-y min-h-0 flex-1 text-xs" data-testid="file-diff">
          {!diff && <div className="muted">从改动列表选择文件以查看 Diff。</div>}
          {diff && (
            <>
              <div className="muted mb-1">{diffPath}</div>
              <pre className="whitespace-pre-wrap rounded bg-[var(--c-bg)] p-2 font-mono">{diff}</pre>
            </>
          )}
        </div>
      )}
    </section>
  );
}
