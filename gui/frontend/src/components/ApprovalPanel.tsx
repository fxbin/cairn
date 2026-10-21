import { useMemo, useState } from "react";
import { desktop } from "../desktop/client";
import type { ErrorBody, RunHistoryView, StreamEvent } from "../desktop/types";

/** 审批面板：展示待审批摘要并提交一次性决定。 */
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

  const waiting = useMemo(() => {
    for (let i = streamEvents.length - 1; i >= 0; i -= 1) {
      const event = streamEvents[i];
      if (event.kind === "run.waiting_approval" && event.error?.fingerprint) {
        return event.error;
      }
    }
    return null;
  }, [streamEvents]);

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
          {selectedRun?.waiting_approval ? "（历史运行标记为待审批，请重新进入运行）" : ""}
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
