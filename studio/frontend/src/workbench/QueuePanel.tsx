/**
 * 后续任务队列：提交仅记录候选，人工确认才创建真实 Run。
 * 所有动作经现有 desktop bridge，不构造本地成功状态。
 * Author: fxbin
 */
import { useCallback, useEffect, useState } from "react";
import { desktop } from "../desktop/client";
import type { QueueItem } from "../desktop/types";

/**
 * 判断队列条目是否处于等待人工确认启动的候选状态。
 */
function isPendingQueueItem(state: string): boolean {
  return state === "pending" || state === "submitted";
}

/**
 * 根据队列状态生成语义化徽标类名与文案，清晰区分待确认草稿与已消费记录。
 */
function describeQueueBadge(item: QueueItem): { tone: string; label: string } {
  if (isPendingQueueItem(item.state)) {
    return { tone: "is-warn", label: "待人工确认 · " + item.state };
  }
  if (item.target_run_id) {
    return { tone: "is-ok", label: "已消费启动 · " + item.state };
  }
  return { tone: "is-muted", label: item.state };
}

/**
 * 右侧后续任务候选队列面板：强化「入队 ≠ 自动运行」的显式确认门禁。
 */
export function QueuePanel({
  workspace,
  revision,
  onRun,
  onError,
}: {
  workspace: string;
  revision: number;
  onRun: (id: string) => void;
  onError: (message: string) => void;
}) {
  const [items, setItems] = useState<QueueItem[]>([]);
  const [busy, setBusy] = useState("");
  const [loading, setLoading] = useState(false);

  const load = useCallback(async () => {
    if (!workspace) {
      setItems([]);
      return;
    }
    setLoading(true);
    try {
      const result = await desktop.listQueue({ workspace });
      setItems(result);
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [workspace, onError]);

  useEffect(() => {
    void load();
  }, [load, revision]);

  const act = async (item: QueueItem, action: "confirm" | "cancel") => {
    if (busy) return;
    setBusy(item.input_id);
    try {
      if (action === "confirm") {
        const ack = await desktop.confirmQueue({ workspace, input_id: item.input_id });
        onRun(ack.run_id);
      } else {
        await desktop.cancelQueue({
          workspace,
          input_id: item.input_id,
          reason: "desktop user cancelled queued input",
        });
      }
      await load();
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy("");
    }
  };

  const pendingCount = items.filter((item) => isPendingQueueItem(item.state)).length;

  return (
    <div className="wb-panel-body" data-testid="queue-panel">
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, flexWrap: "wrap" }}>
        <div className="wb-label">后续任务 · Submit ≠ Confirm</div>
        <span className={"wb-badge " + (pendingCount > 0 ? "is-warn" : "is-muted")}>
          {pendingCount > 0 ? pendingCount + " 项待确认" : "无待确认项"}
        </span>
      </div>
      <h2>任务队列</h2>
      <div className="wb-trust-banner is-unverified" style={{ marginBottom: 12 }}>
        <div>
          <strong>入队 ≠ 自动运行</strong>
          <p style={{ margin: "4px 0 0" }}>
            提交到队列仅保存后续候选草稿，不会自动触发副作用；只有人工点击「确认启动」后才会开始新的 Run。
          </p>
        </div>
      </div>
      <button type="button" className="wb-secondary" style={{ marginTop: 0, marginBottom: 12 }} onClick={() => void load()}>
        刷新队列
      </button>
      {loading && <p>读取中…</p>}
      {items.length === 0 && !loading && <div className="wb-empty">当前工作区没有待处理队列。</div>}
      {items.map((item) => {
        const badge = describeQueueBadge(item);
        const pending = isPendingQueueItem(item.state);
        return (
          <div className="wb-queue-entry" key={item.input_id}>
            <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, flexWrap: "wrap" }}>
              <span className={"wb-badge " + badge.tone}>{badge.label}</span>
              <span className="wb-hash-pill">{item.input_id}</span>
            </div>
            <p style={{ margin: "8px 0" }}>{item.content_preview || "（内容预览不可用）"}</p>
            {pending ? (
              <div className="wb-actions">
                <button
                  type="button"
                  className="wb-primary"
                  disabled={!!busy}
                  onClick={() => void act(item, "confirm")}
                >
                  确认启动
                </button>
                <button type="button" disabled={!!busy} onClick={() => void act(item, "cancel")}>
                  取消
                </button>
              </div>
            ) : (
              <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                {item.target_run_id ? (
                  <>
                    <span className="wb-badge is-ok">✓ 已关联 Run: {item.target_run_id}</span>
                    <button
                      type="button"
                      className="wb-secondary"
                      style={{ marginTop: 0, padding: "5px 10px" }}
                      onClick={() => onRun(item.target_run_id!)}
                    >
                      查看关联运行
                    </button>
                  </>
                ) : (
                  <span className="wb-badge is-muted">该记录不可再次确认</span>
                )}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
