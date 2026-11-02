/**
 * 后续任务队列：提交仅记录候选，确认才创建真实 Run。
 * 所有动作经现有 desktop bridge，不构造本地成功状态。
 */
import { useCallback, useEffect, useState } from "react";
import { desktop } from "../desktop/client";
import type { QueueItem } from "../desktop/types";

export function QueuePanelV2({ workspace, revision, onRun, onError }: {
  workspace: string;
  revision: number;
  onRun: (id: string) => void;
  onError: (message: string) => void;
}) {
  const [items, setItems] = useState<QueueItem[]>([]);
  const [busy, setBusy] = useState("");
  const [loading, setLoading] = useState(false);
  const load = useCallback(async () => {
    if (!workspace) { setItems([]); return; }
    setLoading(true);
    try { const result = await desktop.listQueue({ workspace }); setItems(result); }
    catch (e) { onError(e instanceof Error ? e.message : String(e)); }
    finally { setLoading(false); }
  }, [workspace, onError]);

  useEffect(() => { void load(); }, [load, revision]);
  const act = async (item: QueueItem, action: "confirm" | "cancel") => {
    if (busy) return;
    setBusy(item.input_id);
    try {
      if (action === "confirm") {
        const ack = await desktop.confirmQueue({ workspace, input_id: item.input_id });
        onRun(ack.run_id);
      } else {
        await desktop.cancelQueue({ workspace, input_id: item.input_id, reason: "desktop user cancelled queued input" });
      }
      await load();
    } catch (e) { onError(e instanceof Error ? e.message : String(e)); }
    finally { setBusy(""); }
  };
  return <div className="v2-panel-body" data-testid="v2-queue">
    <div className="v2-label">后续任务</div>
    <h2>任务队列</h2>
    <p>提交到队列不等于立即运行；只有人工确认后才会开始新的 Run。</p>
    <button className="v2-secondary" onClick={() => void load()}>刷新队列</button>
    {loading && <p>读取中…</p>}
    {items.length === 0 && !loading && <div className="v2-empty">当前工作区没有待处理队列。</div>}
    {items.map((item) => <div className="v2-queue-entry" key={item.input_id}>
      <div className="v2-label">{item.state} · {item.input_id}</div>
      <p>{item.content_preview || "（内容预览不可用）"}</p>
      {item.state === "pending" || item.state === "submitted" ? <div className="v2-actions">
        <button className="v2-primary" disabled={!!busy} onClick={() => void act(item, "confirm")}>确认启动</button>
        <button disabled={!!busy} onClick={() => void act(item, "cancel")}>取消</button>
      </div> : <p>{item.target_run_id ? "已关联 Run: " + item.target_run_id : "该记录不可再次确认"}</p>}
    </div>)}
  </div>;
}
