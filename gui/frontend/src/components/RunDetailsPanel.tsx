import { useCallback, useEffect, useMemo, useState } from "react";
import { desktop } from "../desktop/client";
import type { DomainEvent, ErrorBody, RunDetail, RunHistoryView, StreamEvent } from "../desktop/types";
import type { PanelId } from "./panelTypes";
import { toTimeline, toSubagentViews, toContextCompactions, toContextUsage } from "../desktop/timeline";
import { SettingsPanel } from "./SettingsPanel";

/** 运行详情/恢复区：事件时间线、取消与设置。 */
export function RunDetailsPanel({
  workspacePath,
  selectedRun,
  streamEvents,
  panel,
  onPanel,
  onError,
}: {
  workspacePath: string;
  selectedRun: RunHistoryView | null;
  streamEvents: StreamEvent[];
  panel: PanelId;
  onPanel: (panel: PanelId) => void;
  onError: (error: ErrorBody) => void;
}) {
  const [detail, setDetail] = useState<RunDetail | null>(null);
  const [loading, setLoading] = useState(false);
  const [cancelling, setCancelling] = useState(false);

  const loadDetail = useCallback(async () => {
    if (!workspacePath || !selectedRun?.run_id) {
      setDetail(null);
      return;
    }
    setLoading(true);
    try {
      const result = await desktop.inspectRun({
        workspace: workspacePath,
        run_id: selectedRun.run_id,
        include_events: true,
      });
      setDetail(result);
    } catch (error) {
      onError(
        error && typeof error === "object" && "body" in error
          ? (error as { body: ErrorBody }).body
          : { code: "internal", message: String(error) },
      );
    } finally {
      setLoading(false);
    }
  }, [workspacePath, selectedRun?.run_id]);

  useEffect(() => {
    void loadDetail();
  }, [loadDetail]);

  const liveEvents: DomainEvent[] = useMemo(() => {
    const base = detail?.events ?? [];
    const seen = new Set(base.map((event) => event.seq));
    const extras = streamEvents
      .map((item) => item.event)
      .filter((event): event is DomainEvent =>
        Boolean(
          event &&
            selectedRun?.run_id &&
            event.run_id === selectedRun.run_id,
        ),
      )
      .filter((event) => !seen.has(event.seq));
    return base.concat(extras);
  }, [detail?.events, streamEvents, selectedRun?.run_id]);

  const timeline = useMemo(() => toTimeline(liveEvents), [liveEvents]);
  const [highlightSeq, setHighlightSeq] = useState<number | null>(null);
  const scrollToTimelineSeq = (seq: number) => {
    const el = document.querySelector(`[data-rdp-timeline-seq="${seq}"]`);
    el?.scrollIntoView({ block: "nearest", behavior: "smooth" });
    setHighlightSeq(seq);
    window.setTimeout(() => setHighlightSeq((v) => (v === seq ? null : v)), 1200);
  };
  const subagentViews = useMemo(() => toSubagentViews(liveEvents), [liveEvents]);
  const compactions = useMemo(() => toContextCompactions(liveEvents), [liveEvents]);
  const contextUsage = useMemo(() => toContextUsage(liveEvents), [liveEvents]);
  const [expandedCompaction, setExpandedCompaction] = useState<number | null>(null);

  const cancelRun = async () => {
    if (!selectedRun?.run_id) {
      return;
    }
    setCancelling(true);
    try {
      await desktop.cancelRun({ run_id: selectedRun.run_id, reason: "user cancel" });
    } catch (error) {
      onError(
        error && typeof error === "object" && "body" in error
          ? (error as { body: ErrorBody }).body
          : { code: "internal", message: String(error) },
      );
    } finally {
      setCancelling(false);
    }
  };

  return (
    <section className="panel flex min-h-0 flex-col p-3">
      <div className="mb-2 flex items-center gap-2">
        <h2 className="text-sm font-semibold">运行详情</h2>
        <div className="ml-auto flex gap-1">
          {(
            [
              ["run", "运行"],
              ["files", "文件"],
              ["settings", "设置"],
            ] as const
          ).map(([id, label]) => (
            <button
              key={id}
              type="button"
              className={`btn ${panel === id ? "btn-primary" : ""}`}
              onClick={() => onPanel(id)}
            >
              {label}
            </button>
          ))}
          <button
            type="button"
            className="btn btn-danger"
            disabled={!selectedRun || cancelling}
            onClick={() => void cancelRun()}
            data-testid="cancel-run"
          >
            取消运行
          </button>
          <button type="button" className="btn" disabled={!selectedRun || loading} onClick={() => void loadDetail()}>
            刷新
          </button>
        </div>
      </div>
      {panel === "settings" && <SettingsPanel workspacePath={workspacePath} onError={onError} />}
      {panel !== "settings" && (
        <>
          {!selectedRun && (
            <div className="muted text-xs" data-testid="run-empty">
              选择左侧运行以查看事件时间线。
            </div>
          )}
          {selectedRun && (
            <div className="muted text-xs" data-testid="run-selected">
              运行 {selectedRun.run_id} · {detail?.phase || selectedRun.phase || selectedRun.state}
              {detail?.task ? ` · ${detail.task.slice(0, 80)}` : ""}
            </div>
          )}
          {selectedRun && subagentViews.length > 0 && (
            <div className="mt-2 space-y-1" data-testid="subagent-views-detail">
              <div className="text-xs font-semibold">子运行</div>
              {subagentViews.map((view) => (
                <div
                  key={view.childRunId}
                  className="rounded-md border border-[var(--c-border)] px-2 py-1 text-xs"
                  style={{ cursor: "pointer" }}
                  title="定位到时间线对应事件"
                  onClick={() => {
                    const match = timeline.find((item) => item.type.startsWith("subagent_") && item.detail.includes(view.childRunId));
                    if (match) scrollToTimelineSeq(match.seq);
                  }}
                >
                  <div className="flex justify-between gap-2">
                    <span>{view.childRunId}</span>
                    <span className="muted">
                      {view.state}
                      {view.state === "completed" ? ` · ${view.outcome || "未标注"}` : ""}
                    </span>
                  </div>
                  <div className="muted">
                    工具 {view.usageReported || view.toolCalls ? view.toolCalls : "—"} · {view.usageReported ? `in ${view.promptTokens} / out ${view.completionTokens} tok` : <span data-testid="usage-unreported" style={{ color: "var(--c-warning)", fontWeight: 600, fontSize: 11 }}>未回报</span>}
                  </div>
                </div>
              ))}
            </div>
          )}
          {selectedRun && compactions.length > 0 && (
            <div className="mt-2 space-y-1" data-testid="compaction-views-detail">
              <div className="text-xs font-semibold">压缩检查点</div>
              {compactions.map((view) => (
                <div key={view.seq} className="rounded-md border border-[var(--c-border)] px-2 py-1 text-xs">
                  <div className="flex justify-between gap-2">
                    <button
                      type="button"
                      className="btn-sm"
                      data-testid={`compaction-expand-${view.seq}`}
                      aria-expanded={expandedCompaction === view.seq}
                      onClick={() => setExpandedCompaction(expandedCompaction === view.seq ? null : view.seq)}
                    >
                      #{view.seq} {view.trigger} {expandedCompaction === view.seq ? "收起" : "展开"}
                    </button>
                    <span className="muted">
                      {view.unitsBefore}→{view.unitsAfter} · 轮次 {view.compactedTurns}
                    </span>
                  </div>
                  {expandedCompaction === view.seq ? (
                    <pre
                      data-testid={`compaction-summary-${view.seq}`}
                      className="mt-1 max-h-32 overflow-auto whitespace-pre-wrap"
                    >
                      {view.summaryFull}
                    </pre>
                  ) : (
                    <div className="muted">{view.summaryPreview}</div>
                  )}
                </div>
              ))}
              <div className="muted text-xs">
                压缩次数 {contextUsage.compactionCount || 0}
                {contextUsage.lastTrigger ? ` · 触发 ${contextUsage.lastTrigger}` : ""}
              </div>
            </div>
          )}
          <div className="scroll-y mt-2 min-h-0 flex-1 space-y-1" data-testid="stream-events">
            {loading && <div className="muted text-xs">加载事件…</div>}
            {!loading && timeline.length === 0 && (
              <div className="muted text-xs">暂无事件。提交任务后将显示流式过程。</div>
            )}
            {!loading &&
              timeline.map((item) => (
                <div
                  key={item.id}
                  className="rounded-md border border-[var(--c-border)] px-2 py-1 text-xs"
                  data-event-type={item.type}
                  data-rdp-timeline-seq={item.seq}
                  style={highlightSeq === item.seq ? { background: "var(--c-bg-panel)", outline: "1px solid var(--c-accent)" } : undefined}
                >
                  <div className="flex justify-between gap-2">
                    <span className={`font-medium tone-${item.tone}`}>{item.label}</span>
                    <span className="muted">seq {item.seq}</span>
                  </div>
                  {item.detail && <div className="muted mt-0.5 line-clamp-2">{item.detail}</div>}
                </div>
              ))}
          </div>
        </>
      )}
    </section>
  );
}
