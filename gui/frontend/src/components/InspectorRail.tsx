import { useCallback, useEffect, useRef, useState } from "react";
import { EditorState } from "@codemirror/state";
import { EditorView, keymap, lineNumbers, highlightActiveLine } from "@codemirror/view";
import { defaultKeymap, history, historyKeymap } from "@codemirror/commands";
import { javascript } from "@codemirror/lang-javascript";
import { json } from "@codemirror/lang-json";
import { markdown } from "@codemirror/lang-markdown";
import { desktop } from "../desktop/client";
import { toTimeline, toSubagentViews, toContextCompactions, toContextUsage, filterTimelineByKind, type TimelineKind } from "../desktop/timeline";
import type {
  ChangeSet,
  ErrorBody,
  EvidenceView,
  FileNode,
  QueueItem,
  WorkspaceStatus,
} from "../desktop/types";

export type RailTab = "files" | "changes" | "queue" | "details";

/** CodeView 只读源码视图：CodeMirror 6，支持选择复制。 */
function CodeView({ text, language }: { text: string; language?: string }) {
  const host = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (!host.current) return;
    const lang =
      language === "json"
        ? json()
        : language === "markdown"
          ? markdown()
          : language === "javascript" || language === "typescript"
            ? javascript({ typescript: language === "typescript" })
            : [];
    const extensions = [
      lineNumbers(),
      highlightActiveLine(),
      history(),
      keymap.of(defaultKeymap),
      keymap.of(historyKeymap),
      EditorState.readOnly.of(true),
      EditorView.editable.of(false),
      ...(Array.isArray(lang) ? lang : [lang]),
    ];
    const view = new EditorView({
      state: EditorState.create({ doc: text, extensions }),
      parent: host.current,
    });
    return () => view.destroy();
  }, [text, language]);
  return <div ref={host} className="cm-host" data-testid="code-view" />;
}

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
  onQueueAction,
  onSelectRun,
  runReturnDepth = 0,
  onBackToParent,
  streamEvents = [],
  queueVersion,
  runRevision = 0,
}: {
  tab: RailTab;
  onTab: (tab: RailTab) => void;
  workspace: WorkspaceStatus | null;
  runId: string | null;
  statusLabel: string;
  onError: (error: ErrorBody) => void;
  onRefreshThreads: () => void;
  onQueueAction: (action: "confirm" | "cancel", inputId: string) => void;
  onSelectRun?: (runId: string) => void;
  streamEvents?: Array<{ run_id?: string; event?: { run_id: string; seq: number; type: string; payload: unknown; event_id?: string } }>;
  runReturnDepth?: number;
  onBackToParent?: () => void;
  queueVersion?: number;
  runRevision?: number;
}) {
  const [tree, setTree] = useState<FileNode | null>(null);
  const [changes, setChanges] = useState<ChangeSet | null>(null);
  const [queue, setQueue] = useState<QueueItem[]>([]);
  const [evidence, setEvidence] = useState<EvidenceView | null>(null);
  const [diffPath, setDiffPath] = useState("");
  const [diff, setDiff] = useState("");
  const [preview, setPreview] = useState("");
  const [diffScope, setDiffScope] = useState<string>("all");
  const [timeline, setTimeline] = useState<ReturnType<typeof toTimeline>>([]);
  const timelineKindStorageKey = "cairn.timelineKind";
  const [timelineKind, setTimelineKind] = useState<TimelineKind>(() => {
    try {
      const saved = window.sessionStorage.getItem(timelineKindStorageKey);
      return saved === "subagent" || saved === "compaction" || saved === "tool" ? saved : "all";
    } catch {
      return "all";
    }
  });
  const changeTimelineKind = (kind: TimelineKind) => {
    setTimelineKind(kind);
    try {
      window.sessionStorage.setItem(timelineKindStorageKey, kind);
    } catch {
      // 存储不可用时仅内存保持，不阻断交互
    }
  };
  const [subagentViews, setSubagentViews] = useState<ReturnType<typeof toSubagentViews>>([]);
  const [detailEvents, setDetailEvents] = useState<Array<{ run_id: string; seq: number; type: string; payload: unknown; event_id?: string }>>([]);
  const [contextUsage, setContextUsage] = useState<ReturnType<typeof toContextUsage>>({ compactionCount: 0, estimatedUnitsBefore: 0, estimatedUnitsAfter: 0, lastTrigger: "", lastProviderPromptTokens: 0 });
  const [compactions, setCompactions] = useState<ReturnType<typeof toContextCompactions>>([]);
  const [expandedCompaction, setExpandedCompaction] = useState<number | null>(null);
  const [highlightSeq, setHighlightSeq] = useState<number | null>(null);
  const scrollToTimelineSeq = (seq: number) => {
    const el = document.querySelector(`[data-timeline-seq="${seq}"]`);
    el?.scrollIntoView({ block: "nearest", behavior: "smooth" });
    setHighlightSeq(seq);
    window.setTimeout(() => setHighlightSeq((v) => (v === seq ? null : v)), 1200);
  };
  const [copiedSeq, setCopiedSeq] = useState<number | null>(null);
  const [expandedTaskId, setExpandedTaskId] = useState<string | null>(null);
  // 展开压缩摘要时支持 Ctrl/Cmd+C 复制全文
  useEffect(() => {
    if (expandedCompaction === null) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "c" && !window.getSelection()?.toString()) {
        const item = compactions.find((c) => c.seq === expandedCompaction);
        if (item?.summaryFull) {
          event.preventDefault();
          void copySummary(item.seq, item.summaryFull);
        }
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [expandedCompaction, compactions]);
  const copySummary = async (seq: number, text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopiedSeq(seq);
      window.setTimeout(() => setCopiedSeq((v) => (v === seq ? null : v)), 1500);
    } catch {
      setCopiedSeq(null);
    }
  };
  const loadGenerationRef = useRef(0);
  const fileGenerationRef = useRef(0);
  const [runPhase, setRunPhase] = useState("");
  const [reviewNotes, setReviewNotes] = useState<Array<{ id: string; relative: string; line?: string; body: string }>>([]);

  const reportError = useCallback((error: unknown) => {
    onError(error && typeof error === "object" && "body" in error
      ? (error as { body: ErrorBody }).body
      : { code: "internal", message: String(error) });
  }, [onError]);

  const load = useCallback(async () => {
    const generation = ++loadGenerationRef.current;
    setEvidence(null);
    setTimeline([]);
    setSubagentViews([]);
    setContextUsage({ compactionCount: 0, estimatedUnitsBefore: 0, estimatedUnitsAfter: 0, lastTrigger: "", lastProviderPromptTokens: 0 });
    setCompactions([]);
    setRunPhase("");
    if (!workspace?.path || !workspace.exists) return;
    const [treeResult, changeResult, queueResult, evidenceResult, detailResult] = await Promise.allSettled([
      desktop.listFileTree(workspace.path),
      desktop.listChanges(workspace.path),
      desktop.listQueue({ workspace: workspace.path }),
      runId ? desktop.evidence({ workspace: workspace.path, run_id: runId }) : Promise.resolve(null),
      runId ? desktop.inspectRun({ workspace: workspace.path, run_id: runId, include_events: true }) : Promise.resolve(null),
    ]);
    if (generation !== loadGenerationRef.current) return;
    setTree(treeResult.status === "fulfilled" ? treeResult.value : null);
    setChanges(changeResult.status === "fulfilled" ? changeResult.value : null);
    setQueue(queueResult.status === "fulfilled" ? queueResult.value : []);
    if (evidenceResult.status === "fulfilled") setEvidence(evidenceResult.value);
    if (detailResult.status === "fulfilled" && detailResult.value) {
      setTimeline(toTimeline(detailResult.value.events || []));
      const evs = detailResult.value.events || [];
      setDetailEvents(evs);
      setSubagentViews(toSubagentViews(evs));
      setContextUsage(toContextUsage(detailResult.value.events || []));
      setCompactions(toContextCompactions(detailResult.value.events || []));
      setRunPhase(detailResult.value.phase || detailResult.value.state);
    }
    const relevant = tab === "files" ? treeResult : tab === "changes" ? changeResult : tab === "queue" ? queueResult : detailResult;
    if (relevant.status === "rejected") reportError(relevant.reason);
  }, [workspace?.path, workspace?.exists, runId, tab, queueVersion, runRevision, reportError]);

  // 子运行完成事件到达后刷新父运行 Evidence 用量
  const refreshEvidence = useCallback(async () => {
    if (!runId || !workspace?.path || !workspace.exists) return;
    try {
      const result = await desktop.evidence({ workspace: workspace.path, run_id: runId });
      setEvidence(result);
    } catch {
      // 保留旧证据，不因刷新失败清空
    }
  }, [runId, workspace?.path, workspace?.exists]);

  useEffect(() => {
    const hit = streamEvents.some(
      (item) => item.event && item.event.type === "subagent_completed" && (!runId || item.event.run_id === runId),
    );
    if (hit) void refreshEvidence();
  }, [streamEvents, runId, refreshEvidence]);

  // 子运行完成事件到达后刷新父运行 Evidence 用量
  useEffect(() => {
    const hit = streamEvents.some((item) => item.event?.type === "subagent_completed" && (!runId || item.event.run_id === runId));
    if (!hit || !runId || !workspace?.path || !workspace.exists) return;
    let cancelled = false;
    desktop
      .evidence({ workspace: workspace.path, run_id: runId })
      .then((result) => {
        if (!cancelled) setEvidence(result);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [streamEvents, runId, workspace?.path, workspace?.exists]);

  // stream 事件就地并入子运行/压缩投影，不整页重拉
  useEffect(() => {
    if (streamEvents.length === 0 && detailEvents.length === 0) return;
    const seen = new Map<number, (typeof detailEvents)[number]>();
    for (const ev of detailEvents) seen.set(ev.seq, ev);
    for (const item of streamEvents) {
      const ev = item.event;
      if (!ev || (runId && ev.run_id !== runId)) continue;
      seen.set(ev.seq, ev);
    }
    const merged = Array.from(seen.values()).sort((a, b) => a.seq - b.seq);
    setSubagentViews(toSubagentViews(merged));
    setTimeline(toTimeline(merged));
    // 子运行完成事件后重拉父 Evidence，用量汇总随终态更新
    const sawCompletion = merged.some((ev) => ev.type === "subagent_completed");
    if (sawCompletion) {
      setExpandedTaskId(null);
      setExpandedCompaction(null);
    }
    if (sawCompletion && runId && workspace?.path) {
      void desktop
        .evidence({ workspace: workspace.path, run_id: runId })
        .then((value) => setEvidence(value))
        .catch(() => {});
    }
  }, [streamEvents, detailEvents, runId, workspace?.path]);

  useEffect(() => {
    void load();
    return () => { ++loadGenerationRef.current; ++fileGenerationRef.current; };
  }, [load]);

  const openDiff = async (relative: string, scope = diffScope) => {
    if (!workspace?.path) return;
    const generation = ++fileGenerationRef.current;
    setDiffPath(relative);
    setPreview("");
    setDiff("");
    setReviewNotes([]);
    try {
      const text = await desktop.diffFile(workspace.path, relative, scope);
      if (generation === fileGenerationRef.current) setDiff(text);
    } catch (error) {
      if (generation !== fileGenerationRef.current) return;
      setDiffPath("");
      reportError(error);
    }
  };

  const openPreview = async (relative: string) => {
    if (!workspace?.path) return;
    const generation = ++fileGenerationRef.current;
    setDiffPath(relative);
    setDiff("");
    setPreview("");
    try {
      const p = await desktop.previewFile(workspace.path, relative);
      if (generation === fileGenerationRef.current) setPreview(p.text || p.reason || "（空）");
    } catch (error) {
      if (generation !== fileGenerationRef.current) return;
      setPreview("无法预览");
      reportError(error);
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
            <h3>工作区 / 工件</h3>
            <div className="muted" style={{ marginBottom: 8, fontSize: 12 }}>
              预览经路径与敏感边界；二进制/超大文件截断或拒绝。
            </div>
            <div className="muted" style={{ marginBottom: 10, fontSize: 12 }}>
              {workspace?.path || "未选择"}
            </div>
            {tree ? <FileTree node={tree} onFile={(rel) => void openPreview(rel)} /> : <div className="empty-sm">加载中…</div>}
            {preview && (
              <div className="rail-section">
                <h3>预览 · {diffPath}</h3>
                <CodeView text={preview.slice(0, 120000)} />
              </div>
            )}
          </div>
        )}

        {tab === "changes" && (
          <div className="rail-section">
            <h3>改动</h3>
            <label className="field" style={{ marginBottom: 10 }}>
              <span>比较范围</span>
              <select
                className="chip-select"
                value={diffScope}
                data-testid="diff-scope"
                onChange={(event) => {
                  setDiffScope(event.target.value);
                  if (diffPath) {
                    void openDiff(diffPath, event.target.value);
                  }
                }}
              >
                <option value="all">全部（暂存+未暂存）</option>
                <option value="unstaged">仅未暂存</option>
                <option value="staged">仅暂存</option>
                <option value="untracked">未跟踪</option>
              </select>
            </label>
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
                    <span className="muted" style={{ fontSize: 11, alignSelf: "center" }}>
                      文件回退需 before 快照；隔离工作树集成走授权链，不并行争写
                    </span>
                  </div>
                </div>
              ))
            )}
            {diffPath && !preview && (
              <div className="rail-section">
                <h3>Diff · {diffPath}</h3>
                <DiffView text={diff} />
                <div style={{ marginTop: 8 }}>
                  <button
                    type="button"
                    className="btn-sm"
                    data-testid="add-review-note"
                    onClick={() => {
                      const body = window.prompt("行级审查备注");
                      if (!body || !workspace?.path || !diffPath) return;
                      void desktop
                        .saveReviewNote({ workspace: workspace.path, relative: diffPath, body })
                        .then(() => desktop.listReviewNotes(workspace.path, diffPath))
                        .then(setReviewNotes)
                        .catch((error) =>
                          onError(
                            error && typeof error === "object" && "body" in error
                              ? (error as { body: ErrorBody }).body
                              : { code: "internal", message: String(error) },
                          ),
                        );
                    }}
                  >
                    添加审查备注
                  </button>
                  {reviewNotes
                    .filter((n) => n.relative === diffPath)
                    .map((n) => (
                      <div className="list-card" key={n.id}>
                        <div className="meta">{n.line || "文件级"}</div>
                        <div className="title">{n.body}</div>
                      </div>
                    ))}
                </div>
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
                <div className="list-card" key={item.input_id} data-testid={`queue-item-${item.input_id}`}>
                  <div className="title" title={item.content_preview}>
                    {item.content_preview || item.input_id}
                  </div>
                  <div className="meta">
                    {item.state}
                    {item.target_run_id ? ` → ${item.target_run_id}` : ""}
                    {item.reason ? ` · ${item.reason}` : ""}
                  </div>
                  {item.state === "pending" && (
                    <div style={{ display: "flex", gap: 8, marginTop: 8 }}>
                      <button
                        type="button"
                        className="btn-sm"
                        data-testid={`queue-confirm-${item.input_id}`}
                        onClick={() => onQueueAction("confirm", item.input_id)}
                      >
                        确认执行
                      </button>
                      <button
                        type="button"
                        className="btn-sm danger"
                        data-testid={`queue-cancel-${item.input_id}`}
                        onClick={() => onQueueAction("cancel", item.input_id)}
                      >
                        取消
                      </button>
                    </div>
                  )}
                </div>
              ))
            )}
          </div>
        )}

        {tab === "details" && (
          <div className="rail-section">
            {runReturnDepth > 0 && onBackToParent && (
              <button
                type="button"
                className="btn-sm"
                data-testid="back-to-parent-run"
                style={{ marginBottom: 8 }}
                onClick={onBackToParent}
              >
                ← 返回父运行
              </button>
            )}
            <h3>运行状态</h3>
            <div className="list-card">
              <div className="title" style={{ fontSize: 13 }}>
                {statusLabel}
              </div>
              <div className="meta">{runId ? `run ${runId}` : "未选择运行"}</div>
            </div>

            {!runId ? (
              <div className="empty-sm" data-testid="rail-idle-hint" style={{ marginTop: 12 }}>
                选择运行后显示证据、上下文与时间线
              </div>
            ) : (
              <>
            <h3>完成证据</h3>
            <div className="meta" data-testid="result-layers" style={{ marginBottom: 8 }}>
              完成 · 可信 · 对账分栏展示
            </div>
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
                  验证 OK：{!evidence.verification.verification_seq ? "未运行" : evidence.verification.verification_ok ? "通过" : "失败"}
                  {evidence.failure_class ? ` · ${evidence.failure_class}` : ""}
                {runPhase === "waiting_reconcile" ? (
                  <span style={{ marginLeft: 8, color: "var(--c-warning)", fontWeight: 700, fontSize: 11 }}>待对账</span>
                ) : null}
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
                  <span>turns {evidence.metrics.turns ?? "—"}</span>
                  <span>tools {evidence.metrics.tool_calls ?? "—"}</span>
                  <span>in {evidence.metrics.input_tokens ?? "—"}</span>
                  <span>out {evidence.metrics.output_tokens ?? "—"}</span>
                </div>
                <div className="meta" style={{ marginTop: 4 }} data-testid="usage-source">
                  token 来源：{evidence.metrics.usage_estimated ? "字节估算（非 Provider 回报）" : "Provider 实际回报"}
                </div>
                <div className="meta" style={{ marginTop: 6 }} data-testid="verification-evidence">
                  未运行 / 通过 / 失败 / 未知分别标注；重新验证是新的受授权执行，不重放旧命令。
                </div>
                <div className="meta" style={{ marginTop: 6 }} data-testid="reliability">
                  验证 OK {!evidence.verification.verification_seq ? "未运行" : evidence.verification.verification_ok ? "通过" : "失败"} · seq{" "}
                  {evidence.verification.verification_seq ?? "—"} ·{" "}
                  {evidence.trusted ? "可信" : "非可信"}
                </div>
              </div>
            ) : (
              <div className="empty-sm">选择运行后显示</div>
            )}

            <h3>上下文</h3>
            <div className="list-card" data-testid="context-inspector">
              <div className="meta">
                工作区 {workspace?.path || "—"}
              </div>
              <div className="meta">
                配置 {workspace?.config_loaded ? "已加载" : "未加载"}
                {workspace?.config_fingerprint ? ` · 指纹 ${workspace.config_fingerprint.slice(0, 12)}` : ""}
              </div>
              {evidence ? (
                <div className="meta" data-testid="context-usage-detail" style={{ marginTop: 6 }}>
                  用量明细 · turns {evidence.metrics.turns ?? "—"} · tools {evidence.metrics.tool_calls ?? "—"} · in{" "}
                  {evidence.metrics.input_tokens ?? "—"} / out {evidence.metrics.output_tokens ?? "—"}
                  {evidence.metrics.usage_estimated ? "（字节估算）" : "（Provider 回报）"} · 压缩 {evidence.metrics.context_compactions ?? "—"} · 墙钟{" "}
                  {evidence.metrics.wall_time_ms ?? "—"}ms
                </div>
              ) : null}
              <div className="muted" style={{ fontSize: 12, marginTop: 6 }}>
                注入以 Journal 事件为准；未知容量不填假百分比。
              </div>
            </div>

            <h3>压缩原因</h3>
            <div data-testid="compaction-views" style={{ marginBottom: 12 }}>
              {compactions.length === 0 ? (
                <div className="empty-sm">
                  本运行无上下文压缩。上下文接近上限时会自动写入检查点，摘要与触发原因将在此展示。
                </div>
              ) : (
                compactions.map((view) => (
                  <div
                    className="list-card"
                    key={view.seq}
                    data-testid={`compaction-${view.seq}`} aria-live="polite"
                    role="button"
                    tabIndex={0}
                    style={{ cursor: "pointer" }}
                    title={`压缩检查点 #${view.seq} · units ${view.unitsBefore ?? "—"}→${view.unitsAfter ?? "—"} · 点击定位到时间线`}
                    onClick={() => scrollToTimelineSeq(view.seq)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" || event.key === " ") {
                        event.preventDefault();
                        scrollToTimelineSeq(view.seq);
                      }
                    }}
                  >
                    {!timeline.some((item) => item.seq === view.seq) && (
                      <div className="meta" data-testid={`compaction-seq-misalign-${view.seq}`} style={{ color: "var(--c-warning)", fontSize: 11 }}>
                        seq 与时间线未对齐
                      </div>
                    )}
                    <div
                      className="title"
                      style={{ fontSize: 12, cursor: "pointer" }}
                      data-testid={`compaction-scroll-${view.seq}`}
                      title="定位到时间线"
                      onClick={() => scrollToTimelineSeq(view.seq)}
                    >
                      <span data-testid={`compaction-seq-bold-${view.seq}`} aria-hidden="true" style={{ fontWeight: 700 }}>#{view.seq}</span>
                      {" "}·{" "}
                      <span
                        title={view.trigger}
                        data-testid={`compaction-trigger-${view.seq}`}
                        style={{ display: "inline-block", maxWidth: 120, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", verticalAlign: "bottom" }}
                      >
                        {view.trigger}
                      </span>
                      {" · "}
                      <span
                        title={view.strategy}
                        data-testid={`compaction-trigger-strategy-${view.seq}`}
                        style={{ display: "inline-block", maxWidth: 180, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", verticalAlign: "bottom" }}
                      >
                        {view.strategy}
                      </span>
                      {" · "}
                      <span data-testid={`compaction-units-inline-${view.seq}`} aria-live="polite">
                        units {view.unitsBefore ?? "—"} → {view.unitsAfter ?? "—"}
                        {view.lastProviderPromptTokens ? ` · provider in ${view.lastProviderPromptTokens}` : " · provider in —"} · 轮次{" "}
                        {view.compactedTurns}
                      </span>
                    </div>
                    {view.summaryFull ? (
                      <details
                        className="meta"
                        data-testid={`compaction-details-${view.seq}`}
                        style={{ marginTop: 6 }}
                        open={expandedCompaction === view.seq}
                        onToggle={(event) => {
                          const el = event.currentTarget;
                          if (el.open && expandedCompaction !== view.seq) {
                            setExpandedCompaction(view.seq);
                            scrollToTimelineSeq(view.seq);
                          } else if (!el.open && expandedCompaction === view.seq) {
                            setExpandedCompaction(null);
                          }
                        }}
                      >
                        <summary style={{ cursor: "pointer", listStyle: "none" }} aria-label="展开或收起压缩摘要全文" aria-expanded={expandedCompaction === view.seq}>
                          <span data-testid={`compaction-expand-${view.seq}`}>
                            {expandedCompaction === view.seq ? "收起摘要" : "查看摘要全文"}
                          </span>
                        </summary>
                        <div className="meta">
                          {view.summaryFull ? (
                            <>
                              <button
                                type="button"
                                className="btn-sm"
                                data-testid={`compaction-copy-${view.seq}`}
                                aria-label="复制压缩摘要全文"
                                onClick={() => void copySummary(view.seq, view.summaryFull)}
                              >
                                <span aria-live="polite">{copiedSeq === view.seq ? "已复制" : "复制全文"}</span>
                              </button>
                              <pre
                                data-testid={`compaction-summary-${view.seq}`}
                                role="region"
                                aria-label={`压缩摘要全文 seq ${view.seq}`}
                                tabIndex={0}
                                onKeyDown={(event) => {
                                  if (event.key === "PageDown" || event.key === "PageUp") {
                                    event.preventDefault();
                                    event.currentTarget.scrollTop += event.key === "PageDown" ? 120 : -120;
                                  }
                                  if (event.key === "Home" || event.key === "End") {
                                    event.preventDefault();
                                    event.currentTarget.scrollTop = event.key === "Home" ? 0 : event.currentTarget.scrollHeight;
                                  }
                                  if (event.key === "Escape") {
                                    event.preventDefault();
                                    setExpandedCompaction(null);
                                  }
                                }}
                                style={{ whiteSpace: "pre-wrap", fontSize: 11, marginTop: 6, maxHeight: 160, overflow: "auto", outlineColor: "var(--c-accent)" }}
                              >
                                {view.summaryFull}
                              </pre>
                            </>
                          ) : (
                            <div
                              data-testid={`summary-preview-2line-${view.seq}`}
                              aria-live="polite"
                              style={{ display: "-webkit-box", WebkitLineClamp: 2, WebkitBoxOrient: "vertical", overflow: "hidden", marginTop: 6 }}
                            >
                              {view.summaryPreview}
                            </div>
                          )}
                        </div>
                      </details>
                    ) : null}
                  </div>
                ))
              )}
            </div>

            <h3>上下文用量</h3>
            <div className="list-card" data-testid="context-usage">
              <div className="meta">
                压缩次数 {contextUsage.compactionCount || 0}
                {contextUsage.lastTrigger ? ` · 触发 ${contextUsage.lastTrigger}` : ""}
              </div>
              <div className="meta">
                估算单位 {contextUsage.estimatedUnitsBefore || "—"} → {contextUsage.estimatedUnitsAfter || "—"}
                {contextUsage.lastProviderPromptTokens
                  ? ` · 最近 Provider 输入 ${contextUsage.lastProviderPromptTokens}`
                  : " · 最近 Provider 输入 —"}
              </div>
              {compactions.length > 0 && (
                <div className="meta" style={{ marginTop: 6 }} data-testid="compaction-reasons">
                  {compactions.map((item) => (
                    <div key={item.seq} data-testid={`compaction-${item.seq}`}>
                      #{item.seq} {item.trigger} · {item.unitsBefore}→{item.unitsAfter} · 轮次 {item.compactedTurns}
                      {item.summaryPreview ? ` · ${item.summaryPreview}` : ""}
                    </div>
                  ))}
                </div>
              )}
            </div>

            <h3>子运行</h3>
            <div data-testid="subagent-views" role="list" aria-busy={subagentViews.some((v) => v.state === "delegated")} style={{ marginBottom: 12 }}>
              {subagentViews.length === 0 ? (
                <div className="empty-sm">
                  本运行无子运行委派。子运行由 Agent 工具显式委派产生，完成事件会回写父运行 Journal。
                </div>
              ) : (
                subagentViews.map((view) => (
                  <div
                    className="list-card"
                    key={view.childRunId}
                    data-testid={`subagent-${view.childRunId}`}
                    role="listitem"
                    tabIndex={expandedTaskId === view.childRunId ? 0 : -1}
                    aria-label={`子运行 ${view.childRunId}，${view.state}${view.parentRunId && view.parentRunId !== view.childRunId ? `，隶属 ${view.parentRunId}` : ""}${view.task ? `，任务 ${view.task.slice(0, 30)}` : ""}`}
                    aria-expanded={expandedTaskId === view.childRunId}
                    aria-keyshortcuts="Enter Space Escape ArrowUp ArrowDown Home End"
                    style={{
                      outlineColor: "var(--c-accent)",
                      cursor: onSelectRun ? "pointer" : "default",
                      outline: expandedTaskId === view.childRunId ? "1px solid var(--c-accent)" : undefined,
                    }}
                    title={
                      onSelectRun
                        ? `查看子运行 ${view.childRunId} 详情${view.task ? ` · 完整任务：${view.task}` : ""}`
                        : view.task
                          ? `完整任务：${view.task}`
                          : undefined
                    }
                    onClick={() => {
                      onSelectRun?.(view.childRunId);
                      const match = timeline.find(
                        (item) =>
                          item.type.startsWith("subagent_") &&
                          item.detail.includes(view.childRunId),
                      );
                      if (match) scrollToTimelineSeq(match.seq);
                    }}
                    onKeyDown={(event) => {
                      if (event.key === "Escape" && onBackToParent && runReturnDepth > 0) {
                        event.preventDefault();
                        onBackToParent();
                        return;
                      }
                      if (event.key === "Tab" && !event.shiftKey) {
                        const focusables = event.currentTarget.querySelectorAll('[role="button"], button');
                        if (focusables.length > 1) {
                          event.preventDefault();
                          const list = Array.from(focusables) as HTMLElement[];
                          const idx = list.indexOf(document.activeElement as HTMLElement);
                          const next = idx + 1 >= list.length ? 0 : idx + 1;
                          list[next]?.focus();
                        }
                        return;
                      }
                      if (event.key === "ArrowDown" || event.key === "ArrowUp" || event.key === "Home" || event.key === "End") {
                        event.preventDefault();
                        const cards = Array.from(document.querySelectorAll('[data-testid^="subagent-"]'));
                        const idx = cards.indexOf(event.currentTarget);
                        let next = idx;
                        if (event.key === "ArrowDown") next = idx + 1;
                        else if (event.key === "ArrowUp") next = idx - 1;
                        else if (event.key === "Home") next = 0;
                        else next = cards.length - 1;
                        if (next >= 0 && next < cards.length) {
                          (cards[next] as HTMLElement).focus();
                        }
                        return;
                      }
                      if (onSelectRun && (event.key === "Enter" || event.key === " ")) {
                        event.preventDefault();
                        onSelectRun(view.childRunId);
                        const match = timeline.find(
                          (item) =>
                            item.type.startsWith("subagent_") &&
                            item.detail.includes(view.childRunId),
                        );
                        if (match) scrollToTimelineSeq(match.seq);
                      }
                    }}
                  >
                    <div className="title" style={{ fontSize: 12 }}>
                      {view.childRunId}
                    </div>
                    {view.parentRunId && view.parentRunId !== view.childRunId ? (
                      <div className="meta" data-testid={`subagent-parent-${view.childRunId}`} aria-live="polite">
                        父运行 {view.parentRunId}
                      </div>
                    ) : null}
                    <div className="meta">
                      {view.state === "delegated" ? (
                        <span data-testid={`state-running-${view.childRunId}`} aria-live="polite" style={{ color: "var(--c-accent)", fontWeight: 600, fontSize: 11 }}>
                          进行中
                        </span>
                      ) : view.state === "completed" ? (
                        <span data-testid={`state-completed-${view.childRunId}`} aria-live="polite" style={{ color: "var(--c-success)", fontWeight: 600, fontSize: 11 }}>
                          已完成
                        </span>
                      ) : (
                        view.state
                      )}
                      {view.state === "completed" ? (
                        <>
                          <span aria-hidden="true">{" · "}</span>
                          <span
                            role="button"
                            tabIndex={0}
                            data-testid={`subagent-outcome-${view.childRunId}`}
                            aria-label="子运行结果，点击定位到时间线"
                            title="定位到时间线对应事件"
                            style={{ cursor: "pointer", textDecoration: "underline" }}
                            onClick={(event) => {
                              event.stopPropagation();
                              const match = timeline.find((item) => item.type.startsWith("subagent_") && item.detail.includes(view.childRunId));
                              if (match) scrollToTimelineSeq(match.seq);
                            }}
                            onKeyDown={(event) => {
                              if (event.key === "Escape" && expandedTaskId === view.childRunId) {
                                event.preventDefault();
                                setExpandedTaskId(null);
                                return;
                              }
                              if (event.key === "Enter" || event.key === " ") {
                                event.preventDefault();
                                const match = timeline.find((item) => item.type.startsWith("subagent_") && item.detail.includes(view.childRunId));
                                if (match) scrollToTimelineSeq(match.seq);
                              }
                            }}
                          >
                            <span style={{ color: view.outcome === "ok" ? "var(--c-success)" : view.outcome === "failed" ? "var(--c-danger)" : "var(--c-accent)" }}>
                              {view.outcome || "未标注"}
                            </span>
                          </span>
                        </>
                      ) : ""}
                      {view.task ? (
                        <>
                          {" · "}
                          <span
                            data-testid={`subagent-task-${view.childRunId}`}
                            aria-label={expandedTaskId === view.childRunId ? "收起任务摘要" : "展开任务摘要全文"}
                            aria-pressed={expandedTaskId === view.childRunId}
                            title={`${view.task}（${expandedTaskId === view.childRunId ? "点击收起" : "点击展开全文"}）`}
                            role="button"
                            tabIndex={0}
                            style={{ display: "inline-block", maxWidth: expandedTaskId === view.childRunId ? "100%" : 160, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: expandedTaskId === view.childRunId ? "pre-wrap" : "nowrap", verticalAlign: "bottom", cursor: "pointer", textDecoration: "underline" }}
                            onClick={(event) => {
                              event.stopPropagation();
                              if (expandedTaskId !== view.childRunId) {
                                setExpandedCompaction(null);
                              }
                              setExpandedTaskId(expandedTaskId === view.childRunId ? null : view.childRunId);
                              if (expandedTaskId !== view.childRunId) {
                                document.querySelector(`[data-testid="subagent-${view.childRunId}"]`)?.scrollIntoView({ block: "nearest", behavior: "smooth" });
                              }
                              const match = timeline.find((item) => item.type.startsWith("subagent_") && item.detail.includes(view.childRunId));
                              if (match) scrollToTimelineSeq(match.seq);
                            }}
                            onKeyDown={(event) => {
                              if (event.key === "Enter" || event.key === " ") {
                                event.preventDefault();
                                if (expandedTaskId !== view.childRunId) {
                                setExpandedCompaction(null);
                              }
                              setExpandedTaskId(expandedTaskId === view.childRunId ? null : view.childRunId);
                              if (expandedTaskId !== view.childRunId) {
                                document.querySelector(`[data-testid="subagent-${view.childRunId}"]`)?.scrollIntoView({ block: "nearest", behavior: "smooth" });
                              }
                                const match = timeline.find((item) => item.type.startsWith("subagent_") && item.detail.includes(view.childRunId));
                                if (match) scrollToTimelineSeq(match.seq);
                              }
                            }}
                          >
                            {view.task}
                          </span>
                        </>
                      ) : ""}
                    </div>
                    <div className="meta" data-testid={`subagent-usage-${view.childRunId}`} aria-live="polite">
                      工具 {view.usageReported || view.toolCalls ? view.toolCalls : "—"}
                      {view.usageReported
                        ? ` · in ${view.promptTokens} / out ${view.completionTokens} tok`
                        : <>{" "}<span data-testid="usage-unreported" aria-live="polite" style={{ color: "var(--c-warning)", fontWeight: 600, fontSize: 11 }}>未回报</span></>}
                    </div>
                  </div>
                ))
              )}
              {subagentViews.length > 0 && (
                <div className="meta" data-testid="subagent-summary" aria-live="polite" style={{ marginTop: 6 }}>
                  汇总：子运行 {subagentViews.length} · 工具{" "}
                  <span data-testid="summary-tools-dash">
                    {subagentViews.some((v) => v.usageReported || v.toolCalls)
                      ? subagentViews.reduce((n, v) => n + v.toolCalls, 0)
                      : "—"}
                  </span>
                  {" "}· in{" "}
                  {subagentViews.reduce((n, v) => n + v.promptTokens, 0)} / out{" "}
                  {subagentViews.reduce((n, v) => n + v.completionTokens, 0)} tok
                  {(() => {
                    const unreported = subagentViews.filter((v) => !v.usageReported).length;
                    return unreported > 0 ? (
                      <span data-testid="summary-unreported" aria-label="未回报用量子运行计数" style={{ color: "var(--c-warning)", fontWeight: 600, fontSize: 11 }}>
                        {" "}· 未回报 {unreported}
                      </span>
                    ) : null;
                  })()}
                </div>
              )}
            </div>

            <h3>事件时间线</h3>
            <div className="btn-row" style={{ marginBottom: 6 }} data-testid="timeline-kind-filter">
              {(
                [
                  ["all", "全部"],
                  ["subagent", "子运行"],
                  ["compaction", "压缩"],
                  ["tool", "工具"],
                ] as const
              ).map(([id, label]) => (
                <button
                  key={id}
                  type="button"
                  className={`btn-sm ${timelineKind === id ? "" : "muted"}`}
                  data-testid={`timeline-kind-${id}`}
                  aria-pressed={timelineKind === id}
                  onClick={() => changeTimelineKind(id)}
                >
                  {label}
                </button>
              ))}
            </div>
            <div data-testid="run-timeline" style={{ maxHeight: 220, overflow: "auto" }}>
              {timeline.length === 0 ? (
                <div className="empty-sm">选择运行后显示</div>
              ) : (
                filterTimelineByKind(timeline, timelineKind).map((item) => (
                  <div
                    className="timeline-item"
                    key={`${item.seq}-${item.id}`}
                    data-timeline-seq={item.seq}
                    style={highlightSeq === item.seq ? { background: "var(--c-bg-panel)", outline: "1px solid var(--c-accent)" } : undefined}
                  >
                    <div className="ts-label">
                      <span data-testid={`timeline-seq-bold-${item.seq}`} style={{ fontWeight: 700 }}>#{item.seq}</span>
                      {" "}{item.label}
                    </div>
                    {item.detail ? <div className="meta">{item.detail}</div> : null}
                  </div>
                ))
              )}
            </div>

            <h3>恢复</h3>
            <div className="btn-row">
              <button
                type="button"
                className="btn-sm danger"
                disabled={!runId || !runPhase || ["completed", "failed", "cancelled", "waiting_reconcile"].includes(runPhase) || statusLabel === "执行中"}
                title="仅对已停止执行且无待对账操作的非终态运行收尾"
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
              key={runId || "none"}
              enabled={runPhase === "waiting_reconcile"}
              workspacePath={workspace?.path || ""}
              runId={runId}
              onError={onError}
              onDone={() => {
                onRefreshThreads();
                void load();
              }}
            />
              </>
            )}
          </div>
        )}
      </div>
    </aside>
  );
}

function ResolveForm({
  enabled,
  workspacePath,
  runId,
  onError,
  onDone,
}: {
  enabled: boolean;
  workspacePath: string;
  runId: string | null;
  onError: (error: ErrorBody) => void;
  onDone: () => void;
}) {
  const [operationId, setOperationId] = useState("");
  const [verdict, setVerdict] = useState("applied_success");
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
        <option value="applied_success">已执行成功</option>
        <option value="applied_failure">已执行但失败</option>
        <option value="not_applied">确认未执行</option>
        <option value="abort_run">终止运行</option>
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
        disabled={!enabled || !runId || !operationId.trim() || !note.trim() || busy}
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
