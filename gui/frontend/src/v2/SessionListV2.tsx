/**
 * V2 会话列表：搜索、置顶标记、归档显隐与行内重命名。
 * 归档只影响列表视图（由服务端按 include_archived 过滤），不删除 Journal 数据；
 * 置顶只影响排序（服务端已按置顶优先返回），本组件不做本地重排或数据补造。
 * Author: fxbin
 */
import { useState } from "react";
import { desktop } from "../desktop/client";
import type { RunHistoryView } from "../desktop/types";

/** 重命名草稿：sessionId 绑定正在编辑的会话，value 是未提交的新标题。 */
interface RenameDraft {
  sessionId: string;
  value: string;
}

/** 单行会话的展示与操作；缺少 session_id 的历史项不可改展示偏好。 */
function SessionRow({ run, current, renameDraft, onRenameStart, onRenameChange, onRenameCommit, onRenameCancel, onTogglePinned, onToggleArchived, onSelect }: {
  run: RunHistoryView;
  current: boolean;
  renameDraft: RenameDraft | null;
  onRenameStart: (run: RunHistoryView) => void;
  onRenameChange: (sessionId: string, value: string) => void;
  onRenameCommit: (sessionId: string) => void;
  onRenameCancel: () => void;
  onTogglePinned: (run: RunHistoryView) => void;
  onToggleArchived: (run: RunHistoryView) => void;
  onSelect: (runId: string) => void;
}) {
  const metaKey = run.session_id || run.run_id;
  const isRenaming = renameDraft !== null && renameDraft.sessionId === run.session_id && Boolean(run.session_id);
  if (isRenaming && renameDraft) {
    return <div className="v2-session-row v2-session-rename">
      <input aria-label="会话新标题" value={renameDraft.value} autoFocus
        onChange={(event) => onRenameChange(run.session_id || "", event.target.value)} />
      <div className="v2-session-actions">
        <button className="v2-mini-primary" aria-label={"保存会话标题 " + metaKey} onClick={() => onRenameCommit(run.session_id || "")}>保存</button>
        <button aria-label={"取消重命名 " + metaKey} onClick={onRenameCancel}>取消</button>
      </div>
    </div>;
  }
  return <div className={"v2-session-row" + (current ? " current" : "")}>
    <button className="v2-session-open" onClick={() => onSelect(run.run_id)}>
      {run.pinned && <span className="v2-session-pin">置顶</span>}
      <span className="v2-session-name">{run.title || run.task || run.run_id}</span>
      {(run.waiting_approval || run.waiting_reconcile) && <span className="v2-dot-warning" title="需要处理"/>}
      {run.archived && <span className="v2-session-archived">已归档</span>}
    </button>
    <span className="v2-session-actions">
      <button title="重命名会话" aria-label={"重命名会话 " + metaKey} disabled={!run.session_id} onClick={() => onRenameStart(run)}>改</button>
      <button title={run.pinned ? "取消置顶" : "置顶会话"} aria-label={(run.pinned ? "取消置顶 " : "置顶 ") + metaKey} disabled={!run.session_id} onClick={() => onTogglePinned(run)}>{run.pinned ? "取消置顶" : "置顶"}</button>
      <button title={run.archived ? "恢复会话展示" : "归档会话（仅隐藏列表，不删除数据）"} aria-label={(run.archived ? "恢复会话 " : "归档会话 ") + metaKey} disabled={!run.session_id} onClick={() => onToggleArchived(run)}>{run.archived ? "恢复" : "归档"}</button>
    </span>
  </div>;
}

export function SessionListV2({ runs, currentRunId, showArchived, onToggleArchivedView, onSelect, onChanged, onError }: {
  runs: RunHistoryView[];
  currentRunId: string | null;
  showArchived: boolean;
  onToggleArchivedView: () => void;
  onSelect: (runId: string) => void;
  onChanged: () => void;
  onError: (message: string) => void;
}) {
  const [filter, setFilter] = useState("");
  const [renameDraft, setRenameDraft] = useState<RenameDraft | null>(null);
  const [busy, setBusy] = useState(false);

  const visible = runs
    .filter((run) => (run.title + "\n" + run.task).toLowerCase().includes(filter.toLowerCase()))
    .slice(0, 50);

  const commitRename = async (sessionId: string) => {
    if (!renameDraft || busy) return;
    const title = renameDraft.value.trim();
    setBusy(true);
    try {
      await desktop.renameSession(sessionId, title);
      setRenameDraft(null);
      onChanged();
    } catch (err) { onError(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(false); }
  };

  const applyFlags = async (run: RunHistoryView, pinned: boolean, archived: boolean) => {
    if (!run.session_id || busy) return;
    setBusy(true);
    try {
      await desktop.setSessionFlags(run.session_id, pinned, archived);
      onChanged();
    } catch (err) { onError(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(false); }
  };

  return <div className="v2-session-list">
    <input aria-label="搜索会话" placeholder="搜索会话…" value={filter} onChange={(event) => setFilter(event.target.value)}/>
    <button className="v2-archived-toggle" aria-pressed={showArchived} onClick={onToggleArchivedView}>
      {showArchived ? "隐藏已归档会话" : "显示已归档会话"}
    </button>
    {visible.map((run) => <SessionRow key={run.run_id} run={run} current={currentRunId === run.run_id} renameDraft={renameDraft}
      onRenameStart={(target) => setRenameDraft({ sessionId: target.session_id || "", value: target.title || target.task })}
      onRenameChange={(sessionId, value) => setRenameDraft((current) => current && current.sessionId === sessionId ? { sessionId, value } : current)}
      onRenameCommit={(sessionId) => void commitRename(sessionId)}
      onRenameCancel={() => setRenameDraft(null)}
      onTogglePinned={(target) => void applyFlags(target, !target.pinned, target.archived)}
      onToggleArchived={(target) => void applyFlags(target, target.pinned, !target.archived)}
      onSelect={onSelect}/>)}
    {!visible.length && <div className="v2-hint">{showArchived ? "没有匹配的会话。" : "没有匹配的会话；已归档会话默认隐藏，可用上方开关显示。"}</div>}
  </div>;
}
