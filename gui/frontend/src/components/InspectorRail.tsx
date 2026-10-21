import { useCallback, useEffect, useState } from "react";
import { desktop } from "../desktop/client";
import type {
  ChangeSet,
  ErrorBody,
  EvidenceView,
  FileNode,
  QueueItem,
  WorkspaceStatus,
} from "../desktop/types";

export type RailTab = "files" | "changes" | "queue" | "details";

function DiffView({ text }: { text: string }) {
  if (!text.trim()) {
    return <div className="empty-sm">无差异</div>;
  }
  return (
    <div className="mono-block" style={{ padding: 8 }}>
      {text.split("\n").map((line, index) => {
        const kind = line.startsWith("+") && !line.startsWith("+++")
          ? "add"
          : line.startsWith("-") && !line.startsWith("---")
            ? "del"
            : line.startsWith("@@")
              ? "hunk"
              : "meta";
        return (
          <span className="diff-line" data-kind={kind} key={index}>
            {line || " "}
          </span>
        );
      })}
    </div>
  );
}

function FileTree({
  node,
  depth = 0,
  onFile,
}: {
  node: FileNode;
  depth?: number;
  onFile: (relative: string) => void;
}) {
  if (!node.children?.length) return null;
  return (
    <div>
      {node.children.slice(0, 300).map((child) => (
        <div key={child.relative || child.name} style={{ marginLeft: depth * 6 }}>
          <button
            type="button"
            className="tree-row"
            disabled={child.kind === "blocked"}
            onClick={() => {
              if (child.kind === "file") onFile(child.relative);
            }}
          >
            <div className="tree-name">
              <span style={{ opacity: 0.7, width: 14, textAlign: "center" }}>
                {child.kind === "dir" ? "▸" : child.kind === "blocked" ? "⛔" : "·"}
              </span>
              <span>{child.name}</span>
              {child.reason ? <span className="muted">({child.reason})</span> : null}
            </div>
          </button>
          {child.kind === "dir" && (
            <FileTree node={child} depth={depth + 1} onFile={onFile} />
          )}
        </div>
      ))}
    </div>
  );
}

/** 右侧检查栏。 */
export function InspectorRail({
  tab,
  onTab,
  workspace,
  runId,
  statusLabel,
  onError,
  onRefreshThreads,
}: {
  tab: RailTab;
  onTab: (tab: RailTab) => void;
  workspace: WorkspaceStatus | null;
  runId: string | null;
  statusLabel: string;
  onError: (error: ErrorBody) => void;
  onRefreshThreads: () => void;
}) {
  const [tree, setTree] = useState<FileNode | null>(null);
  const [changes, setChanges] = useState<ChangeSet | null>(null);
  const [queue, setQueue] = useState<QueueItem[]>([]);
  const [evidence, setEvidence] = useState<EvidenceView | null>(null);
  const [diffPath, setDiffPath] = useState("");
  const [diff, setDiff] = useState("");
  const [preview, setPreview] = useState("");

  const load = useCallback(async () => {
    if (!workspace?.path || !workspace.exists) return;
    try {
      const [treeResult, changeResult, queueResult] = await Promise.all([
        desktop.listFileTree(workspace.path),
        desktop.listChanges(workspace.path),
        desktop.listQueue({ workspace: workspace.path }),
      ]);
      setTree(treeResult);
      setChanges(changeResult);
      setQueue(queueResult);
      if (runId) {
        setEvidence(await desktop.evidence({ workspace: workspace.path, run_id: runId }));
      }
    } catch (error) {
      onError(
        error && typeof error === "object" && "body" in error
          ? (error as { body: ErrorBody }).body
          : { code: "internal", message: String(error) },
      );
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workspace?.path, workspace?.exists, runId, tab]);

  useEffect(() => {
    void load();
  }, [load]);

  const openDiff = async (relative: string) => {
    if (!workspace?.path) return;
    try {
      const text = await desktop.diffFile(workspace.path, relative);
      setDiff(text);
      setDiffPath(relative);
      setPreview("");
    } catch {
      setDiff("");
    }
  };

  const openPreview = async (relative: string) => {
    if (!workspace?.path) return;
    try {
      const p = await desktop.previewFile(workspace.path, relative);
      setPreview((p.text || p.reason || "（空）") + "");
      setDiffPath(relative);
      setDiff("");
    } catch {
      setPreview("无法预览");
    }
  };

  return (
    <aside className="rail">
      <div className="rail-tabs">
        {(
          [
            ["files", "文件"],
            ["changes", "改动"],
            ["queue", "队列"],
            ["details", "详情"],
          ] as const
        ).map(([id, label]) => (
          <button
            key={id}
            type="button"
            className={`rail-tab ${tab === id ? "active" : ""}`}
            onClick={() => onTab(id)}
          >
            {label}
          </button>
        ))}
      </div>
      <div className="rail-body" data-testid="rail-body">
        {tab === "files" && (
          <div className="rail-section">
            <h3>工作区</h3>
            <div className="muted" style={{ marginBottom: 10, fontSize: 11.5 }}>
              {workspace?.path || "未选择"}
            </div>
            {tree ? <FileTree node={tree} onFile={(rel) => void openPreview(rel)} /> : <div className="empty-sm">加载中…</div>}
            {preview && (
              <div className="rail-section">
                <h3>预览 · {diffPath}</h3>
                <div className="mono-block">{preview.slice(0, 12000)}</div>
              </div>
            )}
          </div>
        )}

        {tab === "changes" && (
          <div className="rail-section">
            <h3>改动</h3>
            {!changes || changes.entries.length === 0 ? (
              <div className="empty-sm">工作区无改动</div>
            ) : (
              changes.entries.map((entry) => (
                <div className="list-card" key={entry.relative}>
                  <div className="title">{entry.relative}</div>
                  <div className="meta">
                    {entry.status} · 归属 {entry.agent_owned ? "Agent" : "未确认"} ·{" "}
                    {entry.evidence || "无证据"}
                  </div>
                  <div className="btn-row" style={{ marginTop: 10 }}>
                    <button type="button" className="btn-sm" onClick={() => void openDiff(entry.relative)}>
                      Diff
                    </button>
                  </div>
                </div>
              ))
            )}
            {diff && (
              <div className="rail-section">
                <h3>Diff · {diffPath}</h3>
                <DiffView text={diff} />
              </div>
            )}
          </div>
        )}

        {tab === "queue" && (
          <div className="rail-section">
            <h3>后续队列</h3>
            {queue.length === 0 ? (
              <div className="empty-sm">队列为空 · 在输入框 ⌘/Ctrl+Enter 入队</div>
            ) : (
              queue.map((item) => (
                <div className="list-card" key={item.input_id}>
                  <div className="title">{item.input_id}</div>
                  <div className="meta">
                    {item.state}
                    {item.target_run_id ? ` → ${item.target_run_id}` : ""}
                  </div>
                </div>
              ))
            )}
          </div>
        )}

        {tab === "details" && (
          <div className="rail-section">
            <h3>运行状态</h3>
            <div className="list-card">
              <div className="title" style={{ fontSize: 13 }}>
                {statusLabel}
              </div>
              <div className="meta">{runId ? `run ${runId}` : "未选择运行"}</div>
            </div>

            <h3>完成证据</h3>
            {evidence ? (
              <div className="list-card">
                <div className="title">
                  {evidence.state} · {evidence.outcome}
                  <span
                    style={{
                      marginLeft: 8,
                      color: evidence.trusted ? "var(--c-success)" : "var(--c-warning)",
                      fontWeight: 700,
                      fontSize: 11,
                    }}
                  >
                    {evidence.trusted ? "可信" : "非可信"}
                  </span>
                </div>
                <div className="meta" style={{ marginBottom: 8 }}>
                  验证 OK：{evidence.verification.verification_ok ? "是" : "否"}
                  {evidence.failure_class ? ` · ${evidence.failure_class}` : ""}
                </div>
                <div className="meta">用量（未知显示为 —）</div>
                <div
                  className="meta"
                  style={{
                    display: "grid",
                    gridTemplateColumns: "1fr 1fr",
                    gap: 6,
                    marginTop: 6,
                  }}
                >
                  <span>turns {evidence.metrics.turns || "—"}</span>
                  <span>tools {evidence.metrics.tool_calls || "—"}</span>
                  <span>in {evidence.metrics.input_tokens || "—"}</span>
                  <span>out {evidence.metrics.output_tokens || "—"}</span>
                </div>
                {evidence.metrics.usage_estimated && (
                  <div className="meta" style={{ marginTop: 4 }}>
                    * token 为估算
                  </div>
                )}
              </div>
            ) : (
              <div className="empty-sm">选择运行后显示</div>
            )}

            <h3>恢复</h3>
            <div className="btn-row">
              <button
                type="button"
                className="btn-sm danger"
                disabled={!runId}
                data-testid="abort-run"
                onClick={() => {
                  if (!runId || !workspace?.path) return;
                  void desktop
                    .abortRun({ workspace: workspace.path, run_id: runId, reason: "user abort" })
                    .then(() => {
                      onRefreshThreads();
                      return load();
                    })
                    .catch((error) =>
                      onError(
                        error && typeof error === "object" && "body" in error
                          ? (error as { body: ErrorBody }).body
                          : { code: "internal", message: String(error) },
                      ),
                    );
                }}
              >
                Abort 收尾
              </button>
            </div>

            <h3>对账</h3>
            <ResolveForm
              workspacePath={workspace?.path || ""}
              runId={runId}
              onError={onError}
              onDone={() => {
                onRefreshThreads();
                void load();
              }}
            />
          </div>
        )}
      </div>
    </aside>
  );
}

function ResolveForm({
  workspacePath,
  runId,
  onError,
  onDone,
}: {
  workspacePath: string;
  runId: string | null;
  onError: (error: ErrorBody) => void;
  onDone: () => void;
}) {
  const [operationId, setOperationId] = useState("");
  const [verdict, setVerdict] = useState("committed");
  const [note, setNote] = useState("");
  const [evidence, setEvidence] = useState("");
  const [busy, setBusy] = useState(false);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      <input
        className="input-sm"
        placeholder="operation_id"
        value={operationId}
        onChange={(event) => setOperationId(event.target.value)}
        data-testid="resolve-op"
      />
      <select className="input-sm" value={verdict} onChange={(event) => setVerdict(event.target.value)}>
        <option value="committed">已提交</option>
        <option value="rolled_back">已回滚</option>
        <option value="unknown">不确定</option>
      </select>
      <input className="input-sm" placeholder="说明" value={note} onChange={(event) => setNote(event.target.value)} />
      <input
        className="input-sm"
        placeholder="证据引用"
        value={evidence}
        onChange={(event) => setEvidence(event.target.value)}
      />
      <button
        type="button"
        className="btn-sm"
        disabled={!runId || !operationId.trim() || busy}
        data-testid="resolve-submit"
        onClick={() => {
          if (!runId || !workspacePath) return;
          setBusy(true);
          void desktop
            .resolveReconcile({
              workspace: workspacePath,
              run_id: runId,
              operation_id: operationId.trim(),
              verdict,
              note,
              evidence,
            })
            .then(() => onDone())
            .catch((error) =>
              onError(
                error && typeof error === "object" && "body" in error
                  ? (error as { body: ErrorBody }).body
                  : { code: "internal", message: String(error) },
              ),
            )
            .finally(() => setBusy(false));
        }}
      >
        提交对账
      </button>
    </div>
  );
}
