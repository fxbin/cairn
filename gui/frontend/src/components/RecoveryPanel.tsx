import { useState } from "react";
import { desktop } from "../desktop/client";
import type { ErrorBody, RunHistoryView } from "../desktop/types";

/**
 * 恢复控制：Abort 收尾、对账悬而未决副作用。
 * 会话回退/文件回退语义分离；缺少快照或漂移时由后端失败关闭。
 */
export function RecoveryPanel({
  workspacePath,
  selectedRun,
  onError,
  onRefresh,
}: {
  workspacePath: string;
  selectedRun: RunHistoryView | null;
  onError: (error: ErrorBody) => void;
  onRefresh: () => void;
}) {
  const [operationId, setOperationId] = useState("");
  const [verdict, setVerdict] = useState("committed");
  const [note, setNote] = useState("");
  const [evidence, setEvidence] = useState("");
  const [reason, setReason] = useState("user abort");
  const [busy, setBusy] = useState(false);

  const run = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    try {
      await fn();
      onRefresh();
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
    <section className="panel flex flex-col gap-2 p-3">
      <h2 className="text-sm font-semibold">恢复 / 对账</h2>
      <div className="muted text-xs">
        区分：Resume（含审批）、Abort（收尾）、对账（悬而未决副作用）。缺失证据时失败关闭。
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <input
          className="input"
          value={reason}
          onChange={(event) => setReason(event.target.value)}
          placeholder="Abort 原因"
          aria-label="Abort 原因"
        />
        <button
          type="button"
          className="btn btn-danger"
          disabled={!selectedRun || busy}
          onClick={() =>
            void run(() =>
              desktop.abortRun({
                workspace: workspacePath,
                run_id: selectedRun!.run_id,
                reason,
              }),
            )
          }
          data-testid="abort-run"
        >
          Abort 收尾
        </button>
      </div>
      <div className="space-y-2 border-t border-[var(--c-border)] pt-2">
        <div className="text-xs font-medium">对账操作</div>
        <input
          className="input"
          value={operationId}
          onChange={(event) => setOperationId(event.target.value)}
          placeholder="operation_id"
          data-testid="resolve-op"
        />
        <select className="input" value={verdict} onChange={(event) => setVerdict(event.target.value)}>
          <option value="committed">已提交</option>
          <option value="rolled_back">已回滚</option>
          <option value="unknown">不确定</option>
        </select>
        <input
          className="input"
          value={note}
          onChange={(event) => setNote(event.target.value)}
          placeholder="说明"
        />
        <input
          className="input"
          value={evidence}
          onChange={(event) => setEvidence(event.target.value)}
          placeholder="证据引用"
        />
        <button
          type="button"
          className="btn"
          disabled={!selectedRun || busy || !operationId.trim()}
          onClick={() =>
            void run(() =>
              desktop.resolveReconcile({
                workspace: workspacePath,
                run_id: selectedRun!.run_id,
                operation_id: operationId.trim(),
                verdict,
                note,
                evidence,
              }),
            )
          }
          data-testid="resolve-submit"
        >
          提交对账
        </button>
      </div>
    </section>
  );
}
