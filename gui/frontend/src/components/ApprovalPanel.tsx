import { useCallback, useEffect, useMemo, useState } from "react";
import { desktop } from "../desktop/client";
import type { ErrorBody, RunHistoryView, StreamEvent } from "../desktop/types";

/**
 * 审批面板：允许/拒绝一次性决定。
 * 待审批状态来自流事件或 Journal 投影（重开窗口后仍可恢复）。
 */
export function ApprovalPanel({
  workspacePath,
  selectedRun,
  streamEvents,
  onError,
}: {
  workspacePath: string;
  selectedRun: RunHistoryView | null;
  streamEvents: StreamEvent[];
  onError: (error: ErrorBody) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [lastDecision, setLastDecision] = useState<string | null>(null);
  const [snapshot, setSnapshot] = useState<{
    run_id: string;
    fingerprint: string;
    summary: string;
    operation_id?: string;
  } | null>(null);

  const fromStream = useMemo(() => {
    for (let i = streamEvents.length - 1; i >= 0; i -= 1) {
      const event = streamEvents[i];
      if (event.kind === "run.waiting_approval" && event.error?.fingerprint) {
        return {
          run_id: event.error.run_id || event.run_id || "",
          fingerprint: event.error.fingerprint,
          summary: event.error.summary || "",
          operation_id: event.error.operation_id,
        };
      }
    }
    return null;
  }, [streamEvents]);

  const loadFromInspect = useCallback(async () => {
    if (!workspacePath || !selectedRun?.run_id) {
      setSnapshot(null);
      return;
    }
    try {
      const detail = await desktop.inspectRun({
        workspace: workspacePath,
        run_id: selectedRun.run_id,
        include_events: true,
      });
      const events = detail.events ?? [];
      for (let i = events.length - 1; i >= 0; i -= 1) {
        const event = events[i];
        if (event.type === "approval_requested") {
          const payload = (event.payload ?? {}) as Record<string, unknown>;
          const fingerprint = typeof payload.fingerprint === "string" ? payload.fingerprint : "";
          const summary = typeof payload.summary === "string" ? payload.summary : "";
          const operationId =
            typeof payload.operation_id === "string" ? payload.operation_id : event.operation_id;
          if (fingerprint) {
            setSnapshot({
              run_id: selectedRun.run_id,
              fingerprint,
              summary,
              operation_id: operationId,
            });
            return;
          }
        }
        if (
          event.type === "approval_granted" ||
          event.type === "approval_denied" ||
          event.type === "operation_started"
        ) {
          break;
        }
      }
      setSnapshot(null);
    } catch {
      setSnapshot(null);
    }
  }, [workspacePath, selectedRun?.run_id]);

  useEffect(() => {
    void loadFromInspect();
  }, [loadFromInspect]);

  const waiting = fromStream ?? snapshot;

  const decide = async (approve: boolean) => {
    if (!workspacePath || !waiting?.fingerprint || !waiting.run_id) {
      return;
    }
    setBusy(true);
    try {
      await desktop.resumeRun({
        workspace: workspacePath,
        run_id: waiting.run_id,
        approve,
        fingerprint: waiting.fingerprint,
        reason: approve ? "approved via desktop UI" : "denied via desktop UI",
      });
      setLastDecision(approve ? "已允许" : "已拒绝");
      setSnapshot(null);
    } catch (error) {
      onError(
        error && typeof error === "object" && "body" in error
          ? (error as { body: ErrorBody }).body
          : { code: "internal", message: String(error) },
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="panel flex min-h-0 flex-col p-3">
      <h2 className="mb-2 text-sm font-semibold">审批</h2>
      {!waiting && (
        <div className="muted text-xs" data-testid="approval-empty">
          当前没有待审批操作。
          {selectedRun?.waiting_approval ? "（历史运行标记为待审批，正在从 Journal 重建…）" : ""}
        </div>
      )}
      {waiting && (
        <div className="flex flex-col gap-2 text-sm" data-testid="approval-pending">
          <div>
            <div className="font-medium">等待人工决定</div>
            <div className="muted text-xs">
              运行 {waiting.run_id} · 操作 {waiting.operation_id || "—"}
            </div>
          </div>
          <pre className="scroll-y max-h-40 rounded-md bg-[var(--c-bg)] p-2 text-xs">
            {waiting.summary || "（无摘要）"}
          </pre>
          <div className="flex gap-2">
            <button
              type="button"
              className="btn btn-primary"
              disabled={busy}
              onClick={() => void decide(true)}
              data-testid="approval-allow"
            >
              允许
            </button>
            <button
              type="button"
              className="btn btn-danger"
              disabled={busy}
              onClick={() => void decide(false)}
              data-testid="approval-deny"
            >
              拒绝
            </button>
          </div>
        </div>
      )}
      {lastDecision && <div className="muted mt-2 text-xs">{lastDecision}（重复点击会被忽略）</div>}
    </section>
  );
}
