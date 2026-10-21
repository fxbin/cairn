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

/** 右侧检查栏：文件 / 改动 / 队列 / 详情（渐进披露）。 */
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

  const load = useCallback(async () => {
    if (!workspace?.path || !workspace.exists) {
      return;
    }
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
  }, [workspace?.path, workspace?.exists, runId, onError]);

  useEffect(() => {
    void load();
    // onError 由父级稳定引用，避免 effect 风暴
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workspace?.path, workspace?.exists, runId, tab]);

  const renderTree = (node: FileNode, depth = 0): JSX.Element | null => {
    if (!node.children?.length) {
      return null;
    }
    return (
      <div key={node.relative || "root"}>
        {node.children.slice(0, 200).map((child) => (
          <div key={child.relative || child.name} style={{ marginLeft: depth * 10 }}>
            <button
              type="button"
              className="thread-item"
              style={{ padding: "6px 8px" }}
              onClick={() => {
                if (child.kind === "file" && workspace?.path) {
                  void desktop
                    .diffFile(workspace.path, child.relative)
                    .then((text) => {
                      setDiff(text);
                      setDiffPath(child.relative);
                    })
                    .catch(() => setDiff(""));
                }
              }}
            >
              <div className="thread-item-title" style={{ fontWeight: 450 }}>
                {child.kind === "dir" ? "📁 " : child.kind === "blocked" ? "⛔ " : "· "}
                {child.name}
                {child.reason ? ` (${child.reason})` : ""}
              </div>
            </button>
            {child.kind === "dir" && renderTree(child, depth + 1)}
          </div>
        ))}
      </div>
    );
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
          <button key={id} type="button" className={`rail-tab ${tab === id ? "active" : ""}`} onClick={() => onTab(id)}>
            {label}
          </button>
        ))}
      </div>
      <div className="rail-body" data-testid="rail-body">
        {tab === "files" && (
          <div className="rail-section">
            <h3>工作区</h3>
            <div className="muted" style={{ marginBottom: 8 }}>
              {workspace?.path || "未选择"}
            </div>
            <div className="list-card">{tree ? renderTree(tree) : <div className="muted">加载中…</div>}</div>
            {diff && (
              <div className="rail-section">
                <h3>Diff · {diffPath}</h3>
                <div className="mono-block">{diff.slice(0, 12000) || "（无差异）"}</div>
              </div>
            )}
          </div>
        )}
        {tab === "changes" && (
          <div className="rail-section">
            <h3>改动</h3>
            {!changes || changes.entries.length === 0 ? (
              <div className="muted">工作区无改动。</div>
            ) : (
              changes.entries.map((entry) => (
                <div className="list-card" key={entry.relative}>
                  <div className="title">{entry.relative}</div>
                  <div className="meta">
                    {entry.status} · 归属 {entry.agent_owned ? "Agent" : "未确认"} · {entry.evidence || "无证据"}
                  </div>
                  <div className="btn-row" style={{ marginTop: 8 }}>
                    <button
                      type="button"
                      className="btn-sm"
                      onClick={() => {
                        if (!workspace?.path) {
                          return;
                        }
                        void desktop
                          .diffFile(workspace.path, entry.relative)
                          .then((text) => {
                            setDiff(text);
                            setDiffPath(entry.relative);
                          })
                          .catch(() => undefined);
                      }}
                    >
                      Diff
                    </button>
                  </div>
                </div>
              ))
            )}
            {diff && (
              <div className="rail-section">
                <h3>Diff · {diffPath}</h3>
                <div className="mono-block">{diff.slice(0, 12000)}</div>
              </div>
            )}
          </div>
        )}
        {tab === "queue" && (
          <div className="rail-section">
            <h3>后续队列</h3>
            {queue.length === 0 ? (
              <div className="muted">队列为空。可在 composer 提交任务后入队。</div>
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
              <div className="meta">{statusLabel}</div>
            </div>
            <h3>完成证据</h3>
            {evidence ? (
              <div className="list-card">
                <div className="title">
                  {evidence.state} · {evidence.outcome}
                  {evidence.trusted ? " · 可信" : " · 非可信"}
                </div>
                <div className="meta">
                  验证 OK：{evidence.verification.verification_ok ? "是" : "否"}
                  {evidence.failure_class ? ` · ${evidence.failure_class}` : ""}
                </div>
                <div className="meta">
                  turns {evidence.metrics.turns} · tools {evidence.metrics.tool_calls} · in{" "}
                  {evidence.metrics.input_tokens || "—"} · out {evidence.metrics.output_tokens || "—"}
                  {evidence.metrics.usage_estimated ? "（估算）" : ""}
                </div>
              </div>
            ) : (
              <div className="muted">选择运行后显示。</div>
            )}
            <h3>恢复</h3>
            <div className="btn-row">
              <button
                type="button"
                className="btn-sm danger"
                disabled={!runId}
                data-testid="abort-run"
                onClick={() => {
                  if (!runId || !workspace?.path) {
                    return;
                  }
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
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
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
          if (!runId || !workspacePath) {
            return;
          }
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
