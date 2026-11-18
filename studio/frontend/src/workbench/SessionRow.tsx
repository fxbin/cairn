/**
 * INPUT: 单条运行视图 RunHistoryView、选中状态、树形缩进/子运行数量与管理回调。
 * OUTPUT: 紧凑克制的侧栏会话树节点组件（弱化终态徽标，悬浮切换操作菜单，最大化标题可读宽度）。
 * POS: studio/frontend/src/workbench 左侧项目与会话树叶子行组件。
 *
 * Author: fxbin
 */
import { useEffect, useRef, useState } from "react";
import type { RunHistoryView } from "../desktop/types";

const TERMINAL_RUN_STATES = new Set(["completed", "failed", "cancelled"]);

interface SessionBadgeMeta {
  tone: "is-ok" | "is-warn" | "is-accent" | "is-muted";
  label: string;
  pulse: boolean;
  isActionable: boolean;
}

/**
 * 根据服务端下发的运行状态投影分级生成会话状态元数据：运行中/待决策使用动作徽标，
 * 已结束运行退居静默色点与微弱尾标，避免大面积边框胶囊视觉噪点。
 */
function resolveSessionBadge(run: RunHistoryView): SessionBadgeMeta {
  if (run.waiting_reconcile) {
    return { tone: "is-warn", label: "待对账", pulse: true, isActionable: true };
  }
  if (run.waiting_approval) {
    return { tone: "is-warn", label: "待授权", pulse: true, isActionable: true };
  }
  if (!TERMINAL_RUN_STATES.has(run.state)) {
    return {
      tone: "is-accent",
      label: run.phase_label || "运行中",
      pulse: true,
      isActionable: true,
    };
  }
  if (run.state === "failed") {
    return { tone: "is-warn", label: "失败", pulse: false, isActionable: false };
  }
  if (run.archived) {
    return { tone: "is-muted", label: "归档", pulse: false, isActionable: false };
  }
  if (run.state === "completed") {
    return { tone: "is-ok", label: "完成", pulse: false, isActionable: false };
  }
  return { tone: "is-muted", label: "已取消", pulse: false, isActionable: false };
}

/**
 * 将本地运行时间戳格式化为紧凑月/日简写，无效时间戳返回空字符串以保持行尾整洁。
 */
function formatCompactRunDate(raw: string | undefined): string {
  if (!raw) return "";
  const ts = Date.parse(raw);
  if (Number.isNaN(ts)) return "";
  const d = new Date(ts);
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${m}/${day}`;
}

/**
 * 渲染单条紧凑会话树节点与按需浮现的会话管理菜单。
 */
export default function SessionRow({
  run,
  selected,
  indent = 0,
  childCount = 0,
  onSelect,
  onRename,
  onFlags,
  onFork,
}: {
  run: RunHistoryView;
  selected: boolean;
  indent?: number;
  childCount?: number;
  onSelect: () => void;
  onRename: (title: string) => Promise<void>;
  onFlags: (pinned: boolean, archived: boolean) => Promise<void>;
  onFork: () => void;
}) {
  const [menu, setMenu] = useState(false);
  const [editing, setEditing] = useState(false);
  const [title, setTitle] = useState(run.title || run.task);
  const [busy, setBusy] = useState(false);
  const rowRef = useRef<HTMLDivElement>(null);
  const badge = resolveSessionBadge(run);
  const compactDate = formatCompactRunDate(run.updated_at || run.created_at);
  const displayTitle = run.title || run.task || run.run_id;

  useEffect(() => {
    if (!menu) return;
    const onPointerDown = (event: MouseEvent) => {
      if (rowRef.current && !rowRef.current.contains(event.target as Node)) {
        setMenu(false);
      }
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setMenu(false);
      }
    };
    window.addEventListener("mousedown", onPointerDown);
    window.addEventListener("keydown", onKeyDown);
    return () => {
      window.removeEventListener("mousedown", onPointerDown);
      window.removeEventListener("keydown", onKeyDown);
    };
  }, [menu]);

  const act = async (fn: () => Promise<void>) => {
    if (busy) return;
    setBusy(true);
    try {
      await fn();
      setMenu(false);
      setEditing(false);
    } finally {
      setBusy(false);
    }
  };

  const dotColor =
    badge.tone === "is-warn"
      ? "var(--wb-amber)"
      : badge.tone === "is-accent"
        ? "var(--wb-action)"
        : "var(--wb-moss)";

  const staticDotClass =
    run.state === "failed"
      ? "wb-session-dot is-failed"
      : selected
        ? "wb-session-dot is-active"
        : run.state === "completed"
          ? "wb-session-dot is-ok"
          : "wb-session-dot is-muted";

  return (
    <div
      ref={rowRef}
      className={"wb-session-row" + (selected ? " current" : "") + (menu ? " has-menu" : "")}
      data-testid="session-row"
      style={indent > 0 ? { paddingLeft: `${Math.min(indent, 3) * 10}px` } : undefined}
    >
      {editing ? (
        <div className="wb-session-rename">
          <input
            aria-label="会话名称"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && title.trim()) {
                void act(() => onRename(title.trim()));
              }
              if (e.key === "Escape") {
                setEditing(false);
              }
            }}
          />
          <button
            type="button"
            disabled={!title.trim() || busy}
            onClick={() => void act(() => onRename(title.trim()))}
          >
            保存
          </button>
          <button type="button" onClick={() => setEditing(false)}>
            取消
          </button>
        </div>
      ) : (
        <>
          <button
            type="button"
            className="wb-session-open"
            onClick={onSelect}
            aria-label={"打开会话 " + displayTitle}
            title={displayTitle}
          >
            {indent > 0 && (
              <span className="wb-session-branch-mark" aria-hidden="true">
                ↳
              </span>
            )}
            {badge.pulse ? (
              <i className="wb-pulse-dot" aria-hidden="true" style={{ color: dotColor }} />
            ) : (
              <span className={staticDotClass} aria-hidden="true" />
            )}
            {run.pinned && (
              <span className="wb-session-pin" title="已固定" aria-hidden="true">
                ◆
              </span>
            )}
            <span className="wb-session-name">{displayTitle}</span>
            <span className="wb-session-tail">
              {childCount > 0 && (
                <span className="wb-session-sub" title={`${childCount} 个子运行`}>
                  +{childCount}
                </span>
              )}
              {badge.isActionable ? (
                <span className={"wb-session-pill " + badge.tone}>{badge.label}</span>
              ) : run.state === "failed" ? (
                <span className="wb-session-micro is-failed">失败</span>
              ) : run.archived ? (
                <span className="wb-session-micro is-muted">归档</span>
              ) : compactDate ? (
                <span className="wb-session-date" aria-hidden="true">
                  {compactDate}
                </span>
              ) : null}
            </span>
          </button>
          <button
            type="button"
            className="wb-session-more"
            aria-label={"管理会话 " + run.run_id}
            aria-expanded={menu}
            onClick={() => setMenu((x) => !x)}
          >
            <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
              <circle cx="3.5" cy="8" r="1.25" />
              <circle cx="8" cy="8" r="1.25" />
              <circle cx="12.5" cy="8" r="1.25" />
            </svg>
          </button>
          {menu && (
            <div className="wb-session-menu">
              <button
                type="button"
                onClick={() => {
                  setTitle(run.title || run.task);
                  setEditing(true);
                  setMenu(false);
                }}
              >
                重命名
              </button>
              <button
                type="button"
                disabled={busy || !run.session_id}
                onClick={() => void act(() => onFlags(!run.pinned, run.archived))}
              >
                {run.pinned ? "取消固定" : "固定会话"}
              </button>
              <button
                type="button"
                disabled={busy || !run.session_id}
                onClick={() => void act(() => onFlags(run.pinned, !run.archived))}
              >
                {run.archived ? "取消归档" : "归档会话"}
              </button>
              <button
                type="button"
                disabled={!run.session_id}
                onClick={() => {
                  onFork();
                  setMenu(false);
                }}
              >
                从此 Run 创建分支
              </button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
