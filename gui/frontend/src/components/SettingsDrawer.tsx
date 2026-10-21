import { useEffect, useState } from "react";
import { desktop } from "../desktop/client";
import type { ErrorBody, WorkspaceStatus } from "../desktop/types";

/**
 * 设置抽屉：主题、只读模式说明、Provider/诊断、MCP、Skills。
 * 与 DeepSeek Harness 设置入口类似——模态抽屉，不打断会话上下文。
 */
export function SettingsDrawer({
  open,
  onClose,
  workspace,
  workspacePath,
  theme,
  onTheme,
  readOnly,
  onReadOnly,
  onError,
}: {
  open: boolean;
  onClose: () => void;
  workspace: WorkspaceStatus | null;
  workspacePath: string;
  theme: string;
  onTheme: (value: string) => void;
  readOnly: boolean;
  onReadOnly: (value: boolean) => void;
  onError: (error: ErrorBody) => void;
}) {
  const [doctor, setDoctor] = useState("打开后加载…");
  const [mcp, setMcp] = useState("—");
  const [skills, setSkills] = useState("—");
  const [tab, setTab] = useState<"general" | "provider" | "resources">("general");

  useEffect(() => {
    if (!open || !workspacePath) {
      return;
    }
    let cancelled = false;
    void (async () => {
      try {
        const [doc, mcpResult, skillResult] = await Promise.all([
          desktop.doctor({ workspace: workspacePath }),
          desktop.mcpStatus({ workspace: workspacePath }).catch(() => "MCP 不可用"),
          desktop.listSkills({ workspace: workspacePath }).catch(() => "Skills 不可用"),
        ]);
        if (!cancelled) {
          setDoctor(JSON.stringify(doc, null, 2).slice(0, 5000));
          setMcp(JSON.stringify(mcpResult, null, 2).slice(0, 3000));
          setSkills(JSON.stringify(skillResult, null, 2).slice(0, 3000));
        }
      } catch (error) {
        if (!cancelled) {
          setDoctor("诊断不可用");
          onError(
            error && typeof error === "object" && "body" in error
              ? (error as { body: ErrorBody }).body
              : { code: "internal", message: String(error) },
          );
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [open, workspacePath, onError]);

  if (!open) {
    return null;
  }

  return (
    <div className="drawer-overlay" data-testid="settings-drawer" role="dialog" aria-label="设置">
      <div className="drawer">
        <div className="drawer-header">
          <h2>设置</h2>
          <button type="button" className="icon-btn" onClick={onClose} aria-label="关闭设置">
            ✕
          </button>
        </div>
        <div className="rail-tabs" style={{ borderBottom: "1px solid var(--c-border)" }}>
          {(
            [
              ["general", "通用"],
              ["provider", "Provider"],
              ["resources", "Skills / MCP"],
            ] as const
          ).map(([id, label]) => (
            <button key={id} type="button" className={`rail-tab ${tab === id ? "active" : ""}`} onClick={() => setTab(id)}>
              {label}
            </button>
          ))}
        </div>
        <div className="drawer-body">
          {tab === "general" && (
            <>
              <label className="field">
                <span>主题</span>
                <select className="chip-select" value={theme} onChange={(event) => onTheme(event.target.value)}>
                  <option value="dark">深色</option>
                  <option value="light">浅色</option>
                  <option value="system">跟随系统</option>
                </select>
              </label>
              <label className="field">
                <span>只读模式（下次任务生效）</span>
                <input
                  type="checkbox"
                  checked={readOnly}
                  onChange={(event) => onReadOnly(event.target.checked)}
                  data-testid="readonly-toggle"
                />
                <span className="muted">禁用写入与命令执行，仍可浏览与分析</span>
              </label>
              <div className="field">
                <span>当前工作区</span>
                <div className="mono-block">{workspace?.path || workspacePath || "未选择"}</div>
              </div>
              <div className="field">
                <span>配置</span>
                <div className="muted">
                  {workspace?.config_loaded ? "已加载" : "未加载"}
                  {workspace?.branch ? ` · ${workspace.branch}` : ""}
                  {workspace?.config_error ? ` · ${workspace.config_error}` : ""}
                </div>
              </div>
            </>
          )}
          {tab === "provider" && (
            <div className="field">
              <span>诊断 / 预算 / 准入（只读）</span>
              <div className="mono-block">{doctor}</div>
            </div>
          )}
          {tab === "resources" && (
            <>
              <div className="field">
                <span>Skills（本地内容包，不授予权限）</span>
                <div className="mono-block">{skills}</div>
              </div>
              <div className="field">
                <span>MCP（声明/状态，不等于已授权）</span>
                <div className="mono-block">{mcp}</div>
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

/** 会话行更多操作：重命名/置顶/归档。 */
export function ThreadContextMenu({
  open,
  onClose,
  title,
  onRename,
  onPin,
  onArchive,
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  onRename: (title: string) => void;
  onPin: () => void;
  onArchive: () => void;
}) {
  const [value, setValue] = useState(title);
  useEffect(() => {
    setValue(title);
  }, [title]);
  if (!open) {
    return null;
  }
  return (
    <div className="drawer-overlay" data-testid="thread-menu" role="dialog">
      <div className="drawer" style={{ width: 360 }}>
        <div className="drawer-header">
          <h2>会话操作</h2>
          <button type="button" className="icon-btn" onClick={onClose}>
            ✕
          </button>
        </div>
        <div className="drawer-body">
          <label className="field">
            <span>重命名</span>
            <input className="input-sm" value={value} onChange={(event) => setValue(event.target.value)} data-testid="rename-input" />
          </label>
          <div className="btn-row">
            <button
              type="button"
              className="btn-sm primary"
              onClick={() => {
                onRename(value);
                onClose();
              }}
            >
              保存名称
            </button>
            <button type="button" className="btn-sm" onClick={() => { onPin(); onClose(); }}>
              置顶 / 取消置顶
            </button>
            <button type="button" className="btn-sm danger" onClick={() => { onArchive(); onClose(); }}>
              归档
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
