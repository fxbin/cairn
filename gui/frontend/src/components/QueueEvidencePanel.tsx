import { useCallback, useEffect, useState } from "react";
import { desktop } from "../desktop/client";
import type { ErrorBody, EvidenceView, QueueItem } from "../desktop/types";

/**
 * 队列与验证：展示后续队列、确认/取消，并展示完成证据。
 * 排队等待与立即补充（steering）语义分离，此处只操作持久队列。
 */
export function QueueEvidencePanel({
  workspacePath,
  runId,
  onError,
}: {
  workspacePath: string;
  runId: string | null;
  onError: (error: ErrorBody) => void;
}) {
  const [queue, setQueue] = useState<QueueItem[]>([]);
  const [content, setContent] = useState("");
  const [evidence, setEvidence] = useState<EvidenceView | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    if (!workspacePath) {
      return;
    }
    try {
      const items = await desktop.listQueue({ workspace: workspacePath });
      setQueue(items);
      if (runId) {
        const ev = await desktop.evidence({
          workspace: workspacePath,
          run_id: runId,
        });
        setEvidence(ev);
      } else {
        setEvidence(null);
      }
    } catch (error) {
      onError(
        error && typeof error === "object" && "body" in error
          ? (error as { body: ErrorBody }).body
          : { code: "internal", message: String(error) },
      );
    }
  }, [workspacePath, runId]);

  useEffect(() => {
    void load();
  }, [load]);

  const act = async (kind: "submit" | "confirm" | "cancel", inputId?: string) => {
    if (!workspacePath) {
      return;
    }
    setBusy(true);
    try {
      if (kind === "submit") {
        await desktop.submitQueue({ workspace: workspacePath, content });
        setContent("");
      } else if (kind === "confirm" && inputId) {
        await desktop.confirmQueue({ workspace: workspacePath, input_id: inputId });
      } else if (kind === "cancel" && inputId) {
        await desktop.cancelQueue({ workspace: workspacePath, input_id: inputId, reason: "user" });
      }
      await load();
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
    <section className="panel flex min-h-0 flex-col gap-2 p-3">
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-semibold">队列 / 验证</h2>
        <button type="button" className="btn" onClick={() => void load()}>
          刷新
        </button>
      </div>
      <div className="flex gap-2">
        <input
          className="input"
          value={content}
          onChange={(event) => setContent(event.target.value)}
          placeholder="后续任务（排队，不打断当前）"
          data-testid="queue-input"
        />
        <button
          type="button"
          className="btn"
          disabled={busy || !content.trim()}
          onClick={() => void act("submit")}
          data-testid="queue-submit"
        >
          入队
        </button>
      </div>
      <div className="scroll-y min-h-0 flex-1 space-y-1" data-testid="queue-list">
        {queue.length === 0 && <div className="muted text-xs">队列为空。</div>}
        {queue.map((item) => (
          <div key={item.input_id} className="rounded border border-[var(--c-border)] px-2 py-1 text-xs">
            <div className="flex items-center justify-between gap-2">
              <span className="font-medium">{item.input_id}</span>
              <span className="muted">{item.state}</span>
            </div>
            <div className="mt-1 flex gap-1">
              {item.state === "pending" && (
                <>
                  <button
                    type="button"
                    className="btn"
                    disabled={busy}
                    onClick={() => void act("confirm", item.input_id)}
                  >
                    确认启动
                  </button>
                  <button
                    type="button"
                    className="btn btn-danger"
                    disabled={busy}
                    onClick={() => void act("cancel", item.input_id)}
                  >
                    取消
                  </button>
                </>
              )}
            </div>
          </div>
        ))}
      </div>
      <div className="border-t border-[var(--c-border)] pt-2 text-xs" data-testid="evidence-panel">
        <div className="mb-1 font-medium">完成证据</div>
        {!evidence && <div className="muted">选择运行后显示证据与用量。</div>}
        {evidence && (
          <div className="space-y-1">
            <div>
              终态 {evidence.state} · 结果 {evidence.outcome}
              {evidence.trusted ? " · 可信完成" : " · 不可信/未验证"}
            </div>
            <div className="muted">
              验证 seq {evidence.verification.verification_seq ?? "—"} · OK{" "}
              {evidence.verification.verification_ok ? "是" : "否"}
              {evidence.failure_class ? ` · 失败分类 ${evidence.failure_class}` : ""}
            </div>
            <div className="muted">
              用量（未知不显示为 0）：
              {evidence.metrics
                ? ` turns=${String(evidence.metrics.turns ?? "—")} tools=${String(evidence.metrics.tool_calls ?? "—")} in=${String(evidence.metrics.input_tokens ?? "—")} out=${String(evidence.metrics.output_tokens ?? "—")}${evidence.metrics.usage_estimated ? "（估算）" : ""}`
                : " 无"}
            </div>
          </div>
        )}
      </div>
    </section>
  );
}
