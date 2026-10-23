import { useEffect, useRef } from "react";

/** 底部 Composer：多行输入、IME 安全、模型/模式、发送/停止/入队。 */
export function Composer({
  value,
  onChange,
  onSubmit,
  onStop,
  onQueue,
  onInsertFile,
  running,
  model,
  onModelChange,
  readOnly,
  disabled,
  placeholder,
}: {
  value: string;
  onChange: (value: string) => void;
  onSubmit: () => void;
  onStop: () => void;
  onQueue: () => void;
  onInsertFile?: () => void;
  running: boolean;
  model: string;
  onModelChange: (value: string) => void;
  readOnly: boolean;
  disabled?: boolean;
  placeholder?: string;
}) {
  const ref = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) {
      return;
    }
    el.style.height = "auto";
    el.style.height = `${Math.min(el.scrollHeight, 220)}px`;
  }, [value]);

  return (
    <div className="composer-wrap">
      <div className="composer">
        <textarea
          ref={ref}
          value={value}
          onChange={(event) => onChange(event.target.value)}
          onKeyDown={(event) => {
            if (event.key !== "Enter" || event.nativeEvent.isComposing) return;
            if (event.metaKey || event.ctrlKey) {
              event.preventDefault();
              if (value.trim() && !disabled) onQueue();
              return;
            }
            if (!event.shiftKey) {
              event.preventDefault();
              if (!running && value.trim() && !disabled) onSubmit();
            }
          }}
          placeholder={placeholder || "向 Cairn 描述任务…（Enter 发送，⌘/Ctrl+Enter 入队，Shift+Enter 换行）"}
          title="可用内置命令见资源页；斜杠文本不会直接执行为 shell"
          aria-label="任务输入"
          data-testid="composer-input"
          rows={2}
          onPaste={(event) => {
            // 保留粘贴文本原样（含代码块），交给 textarea 默认处理。
            event.stopPropagation();
          }}
        />
        <div className="composer-toolbar">
          <select
            className="chip-select"
            value={model}
            onChange={(event) => onModelChange(event.target.value)}
            aria-label="模型"
            data-testid="model-select"
          >
            <option value="">配置默认模型</option>
            <option value="glm-5.2">glm-5.2</option>
            <option value="mimo-v2.6-flash">mimo-v2.6-flash</option>
          </select>
          <button
            type="button"
            className={`btn-sm ${readOnly ? "primary" : ""}`}
            onClick={() => onModelChange(model)}
            style={{ display: "none" }}
            data-read-only={readOnly ? "1" : "0"}
          />
          <span className="muted" style={{ fontSize: 11 }} data-testid="mode-label">
            {readOnly ? "只读" : "可写"}
          </span>
          <span className="spacer" />
          {onInsertFile && (
            <button
              type="button"
              className="btn-sm"
              disabled={disabled || running}
              data-testid="composer-file"
              title="插入工作区文件引用"
              onClick={onInsertFile}
            >
              引用文件
            </button>
          )}
          <button
            type="button"
            className="btn-sm"
            disabled={disabled || !value.trim() || running}
            onClick={onQueue}
            data-testid="composer-queue"
            title="加入后续队列，不打断当前任务"
          >
            入队
          </button>
          {running ? (
            <button type="button" className="btn-send" style={{ background: "var(--c-danger)" }} onClick={onStop} data-testid="composer-stop">
              停止
            </button>
          ) : (
            <button
              type="button"
              className="btn-send"
              disabled={disabled || !value.trim()}
              onClick={onSubmit}
              data-testid="composer-send"
            >
              发送 ↵
            </button>
          )}
        </div>
      </div>
      <div className="composer-hint">副作用会先请求批准；运行事实写入 Event Journal。</div>
    </div>
  );
}

/** 侧栏会话列表。 */
export function ThreadSidebar({
  threads,
  activeId,
  query,
  onQuery,
  onSelect,
  onNew,
  onContext,
  workspacePath,
  onPickWorkspace,
  workspaceLabel,
  recent = [],
  onSwitchWorkspace,
}: {
  threads: Array<{
    run_id: string;
    session_id?: string;
    title: string;
    phase?: string;
    state: string;
    updated_at: string;
    waiting_approval?: boolean;
    pinned?: boolean;
  }>;
  activeId: string | null;
  query: string;
  onQuery: (value: string) => void;
  onSelect: (runId: string) => void;
  onNew: () => void;
  onContext: (runId: string, title: string, sessionId: string, pinned: boolean) => void;
  workspacePath: string;
  onPickWorkspace: () => void;
  workspaceLabel: string;
  recent?: Array<{ path: string; name: string; archived?: boolean }>;
  onSwitchWorkspace?: (path: string) => void;
}) {
  return (
    <aside className="sidebar">
      <div className="sidebar-brand">
        <div className="brand-mark">C</div>
        <div>
          <div className="brand-title">Cairn</div>
          <div className="brand-sub">事件溯源编码 Agent</div>
        </div>
      </div>
      <div className="sidebar-actions">
        <button type="button" className="btn-new-chat" onClick={onNew} data-testid="new-session">
          <span>＋</span> 新建任务
        </button>
      </div>
      <div className="sidebar-search">
        <input
          className="input-sm"
          value={query}
          onChange={(event) => onQuery(event.target.value)}
          placeholder="搜索会话…"
          aria-label="搜索会话"
          data-testid="thread-search"
        />
      </div>
      <div className="sidebar-section-title">最近</div>
      <div className="thread-list" data-testid="thread-list">
        {threads.length === 0 && (
          <div className="muted" style={{ padding: "8px 12px", fontSize: 12 }}>
            暂无会话
          </div>
        )}
        {threads.map((thread) => (
          <div key={thread.run_id} className="thread-item-wrap">
            <button
              type="button"
              className={`thread-item ${thread.run_id === activeId ? "active" : ""}`}
              onClick={() => onSelect(thread.run_id)}
            >
              <div className="thread-item-title">
                {thread.pinned ? <span title="置顶">📌 </span> : null}
                {thread.title}
              </div>
              <div className="thread-item-meta">
                <span
                  className="thread-dot"
                  data-state={thread.waiting_approval ? "waiting_approval" : thread.state}
                />
                <span>{thread.waiting_approval ? "待审批" : thread.phase || thread.state}</span>
                <span>{new Date(thread.updated_at).toLocaleDateString("zh-CN")}</span>
              </div>
            </button>
            <button
              type="button"
              className="thread-more"
              aria-label="会话操作"
              onClick={(event) => {
                event.stopPropagation();
                onContext(thread.run_id, thread.title, thread.session_id || thread.run_id, Boolean(thread.pinned));
              }}
              data-testid={`thread-more-${thread.run_id}`}
            >
              ⋯
            </button>
          </div>
        ))}
      </div>
      <div className="sidebar-footer">
        {recent.length > 1 && onSwitchWorkspace && (
          <div className="recent-projects" data-testid="recent-projects">
            {recent.slice(0, 5).map((item) => (
              <button
                key={item.path}
                type="button"
                className={`recent-item ${item.path === workspacePath ? "active" : ""}`}
                data-testid={`recent-${item.name}`}
                title={item.path}
                onClick={() => onSwitchWorkspace(item.path)}
              >
                {item.name}
              </button>
            ))}
          </div>
        )}
        <button type="button" className="workspace-chip" onClick={onPickWorkspace} title={workspacePath} data-testid="workspace-chip">
          <span className="ws-icon">⌂</span>
          <span className="path">{workspaceLabel || "选择工作区"}</span>
          <span className="muted" style={{ fontSize: 11 }}>更换</span>
        </button>
      </div>
    </aside>
  );
}
