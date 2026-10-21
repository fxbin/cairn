import { useEffect, useRef } from "react";

/** 底部 Composer：多行输入、IME 安全、模型选择、发送/停止。 */
export function Composer({
  value,
  onChange,
  onSubmit,
  onStop,
  running,
  model,
  onModelChange,
  disabled,
}: {
  value: string;
  onChange: (value: string) => void;
  onSubmit: () => void;
  onStop: () => void;
  running: boolean;
  model: string;
  onModelChange: (value: string) => void;
  disabled?: boolean;
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
            if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
              event.preventDefault();
              if (!running && value.trim() && !disabled) {
                onSubmit();
              }
            }
          }}
          placeholder="向 Cairn 描述任务…（Enter 发送，Shift+Enter 换行）"
          aria-label="任务输入"
          data-testid="composer-input"
          rows={2}
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
          <span className="spacer" />
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
      <div className="composer-hint">Cairn 会写入 Event Journal；有副作用时会在对话中请求你的批准。</div>
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
  workspacePath,
  onPickWorkspace,
  workspaceLabel,
}: {
  threads: Array<{
    run_id: string;
    title: string;
    phase?: string;
    state: string;
    updated_at: string;
  }>;
  activeId: string | null;
  query: string;
  onQuery: (value: string) => void;
  onSelect: (runId: string) => void;
  onNew: () => void;
  workspacePath: string;
  onPickWorkspace: () => void;
  workspaceLabel: string;
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
        {threads.length === 0 && <div className="muted" style={{ padding: "8px 12px", fontSize: 12 }}>暂无会话</div>}
        {threads.map((thread) => (
          <button
            key={thread.run_id}
            type="button"
            className={`thread-item ${thread.run_id === activeId ? "active" : ""}`}
            onClick={() => onSelect(thread.run_id)}
          >
            <div className="thread-item-title">{thread.title}</div>
            <div className="thread-item-meta">
              <span>{thread.phase || thread.state}</span>
              <span>{new Date(thread.updated_at).toLocaleDateString("zh-CN")}</span>
            </div>
          </button>
        ))}
      </div>
      <div className="sidebar-footer">
        <button type="button" className="workspace-chip" onClick={onPickWorkspace} title={workspacePath}>
          <span>⌂</span>
          <span className="path">{workspaceLabel || "选择工作区"}</span>
        </button>
      </div>
    </aside>
  );
}
