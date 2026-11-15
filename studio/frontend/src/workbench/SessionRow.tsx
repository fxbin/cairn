/**
 * 侧边栏会话树节点组件：以紧凑树形行呈现运行状态微标、层级缩进与显式会话元数据操作。
 * 分叉操作始终显式创建新会话，绝不静默续接原会话。
 * Author: fxbin
 */
import { useEffect, useRef, useState } from "react";
import type { RunHistoryView } from "../desktop/types";

const TERMINAL_RUN_STATES = new Set(["completed", "failed", "cancelled"]);

interface SessionBadgeMeta {
  tone: "is-ok" | "is-warn" | "is-accent" | "is-muted";
  label: string;
  pulse: boolean;
  prominent: boolean;
}

/**
 * 根据服务端下发的运行状态投影生成会话状态徽标元数据，不在前端猜测终态。
 */
function resolveSessionBadge(run: RunHistoryView): SessionBadgeMeta {
  if (run.waiting_reconcile) {
    return { tone: "is-warn", label: "待对账", pulse: true, prominent: true };
  }
  if (run.waiting_approval) {
    return { tone: "is-warn", label: "待授权", pulse: true, prominent: true };
  }
  if (!TERMINAL_RUN_STATES.has(run.state)) {
    return { tone: "is-accent", label: run.phase_label || "运行中", pulse: true, prominent: true };
  }
  if (run.state === "failed") {
    return { tone: "is-warn", label: "失败", pulse: false, prominent: true };
  }
  if (run.archived) {
    return { tone: "is-muted", label: "归档", pulse: false, prominent: true };
  }
  if (run.state === "completed") {
    return { tone: "is-ok", label: "完成", pulse: false, prominent: false };
  }
  return { tone: "is-muted", label: "已取消", pulse: false, prominent: false };
}

/**
 * 将本地运行时间戳格式化为紧凑月/日简写，无效时间戳返回空字符串以保持树节点干净。
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
 * 渲染单条紧凑会话树节点与显式管理菜单。
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
        : badge.tone === "is-ok"
          ? "var(--wb-moss)"
          : "var(--wb-subtle)";

  return (
    <div
      ref={rowRef}
      className={"wb-session-row" + (selected ? " current" : "")}
      data-testid="session-row"
      style={indent > 0 ? { paddingLeft: `${Math.min(indent, 3) * 12}px` } : undefined}
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
            aria-label={"打开会话 " + (run.title || run.task || run.run_id)}
            title={run.title || run.task || run.run_id}
          >
            {indent > 0 && (
              <span className="wb-session-sub" aria-hidden="true">
                ↳
              </span>
            )}
            {badge.pulse ? (
              <i className="wb-pulse-dot" aria-hidden="true" style={{ color: dotColor }} />
            ) : (
              <span
                aria-hidden="true"
                style={{
                  width: 6,
                  height: 6,
                  flex: "0 0 6px",
                  borderRadius: "50%",
                  background: dotColor,
                  opacity: badge.tone === "is-muted" ? 0.55 : 0.85,
                }}
              />
            )}
            {run.pinned && (
              <span title="已固定" aria-hidden="true" style={{ fontSize: 10, color: "var(--wb-action)" }}>
                ◆
              </span>
            )}
            <span className="wb-session-name" style={{ flex: 1 }}>
              {run.title || run.task || run.run_id}
            </span>
            {childCount > 0 && <span className="wb-session-sub">{childCount} 子运行</span>}
            {badge.prominent ? (
              <span className={"wb-badge " + badge.tone}>{badge.label}</span>
            ) : compactDate ? (
              <span className="wb-session-sub" aria-hidden="true">
                {compactDate}
              </span>
            ) : null}
          </button>
          <button
            type="button"
            className="wb-session-more"
            aria-label={"管理会话 " + run.run_id}
            aria-expanded={menu}
            onClick={() => setMenu((x) => !x)}
          >
            ···
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
