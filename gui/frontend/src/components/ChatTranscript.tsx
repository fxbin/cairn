import { useEffect, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { ChatBlock } from "../desktop/chatBlocks";
import type { ErrorBody } from "../desktop/types";

function formatTime(iso: string): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" });
}

const LONG_BODY_THRESHOLD = 2400;

/** AssistantBody 安全渲染模型正文：Markdown 不执行 raw HTML/脚本。 */
export function AssistantBody({ text }: { text: string }) {
  const [expanded, setExpanded] = useState(false);
  const long = text.length > LONG_BODY_THRESHOLD;
  const shown = long && !expanded ? text.slice(0, LONG_BODY_THRESHOLD) + "\n…" : text;
  return (
    <div className="msg-body" data-testid="assistant-body">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        skipHtml
        components={{
          a: ({ href, children }) => (
            <a href={href} target="_blank" rel="noreferrer noopener">
              {children}
            </a>
          ),
          code: ({ className, children }) => {
            const isBlock = String(className || "").includes("language-") || String(children).includes("\n");
            if (!isBlock) {
              return <code>{children}</code>;
            }
            const raw = String(children);
            return (
              <div className="code-block">
                <button
                  type="button"
                  className="btn-sm code-copy"
                  data-testid="copy-code"
                  onClick={() => {
                    void navigator.clipboard?.writeText(raw);
                  }}
                >
                  复制
                </button>
                <pre>
                  <code className={className}>{children}</code>
                </pre>
              </div>
            );
          },
        }}
      >
        {shown}
      </ReactMarkdown>
      {long && (
        <button type="button" className="btn-sm" data-testid="expand-body" onClick={() => setExpanded((v) => !v)}>
          {expanded ? "收起" : "展开全文"}
        </button>
      )}
    </div>
  );
}

function toolGlyph(name: string, status: string): string {
  if (status === "failed") return "!";
  if (status === "ok") return "✓";
  const n = name.toLowerCase();
  if (n.includes("read") || n.includes("file")) return "¶";
  if (n.includes("exec") || n.includes("command") || n.includes("run")) return "›_";
  if (n.includes("patch") || n.includes("write") || n.includes("edit")) return "✎";
  if (n.includes("search") || n.includes("grep") || n.includes("glob")) return "⌕";
  if (n.includes("list") || n.includes("dir")) return "☰";
  return "⚙";
}

function ToolCard({ block }: { block: Extract<ChatBlock, { kind: "tool" }> }) {
  const [open, setOpen] = useState(block.status === "failed");
  return (
    <div className="tool-card" data-status={block.status}>
      <button type="button" className="tool-card-header" onClick={() => setOpen((v) => !v)}>
        <span className="tool-icon">{toolGlyph(block.name, block.status)}</span>
        <span className="tool-name">{block.name}</span>
        <span className="tool-summary">{block.summary}</span>
        <span className="tool-meta">
          <span>
            {block.status === "running" ? "执行中" : block.status === "ok" ? "成功" : "失败"}
          </span>
          <span>{formatTime(block.at)}</span>
          <span>{open ? "收起" : "展开"}</span>
        </span>
      </button>
      {open && block.detail && (
        <div className="tool-card-body" data-testid="tool-output">
          <button
            type="button"
            className="btn-sm"
            onClick={() => void navigator.clipboard?.writeText(block.detail)}
          >
            复制输出
          </button>
          <pre style={{ marginTop: 6, whiteSpace: "pre-wrap" }}>{block.detail}</pre>
        </div>
      )}
    </div>
  );
}

function ApprovalCard({
  block,
  onDecide,
}: {
  block: Extract<ChatBlock, { kind: "approval" }>;
  onDecide: (approve: boolean, fingerprint: string, runId: string) => Promise<boolean>;
}) {
  const [busy, setBusy] = useState(false);
  const decide = (approve: boolean) => {
    if (block.resolved || busy) {
      return;
    }
    setBusy(true);
    void onDecide(approve, block.fingerprint, block.runId)
      .then((accepted) => { if (!accepted) setBusy(false); })
      .catch(() => setBusy(false));
  };
  return (
    <div className="approval-card" data-testid="approval-card">
      <h3>
        <span>⚠</span> 需要你的确认
      </h3>
      <div className="approval-summary">{block.summary || "（无摘要）"}</div>
      <div className="muted" style={{ fontSize: 12 }}>
        运行 {block.runId}
        {block.operationId ? ` · 操作 ${block.operationId}` : ""}
      </div>
      <details style={{ marginTop: 8 }}>
        <summary style={{ fontSize: 12, cursor: "pointer" }}>影响与指纹</summary>
        <div className="mono-block" style={{ marginTop: 6, fontSize: 11 }}>
          fingerprint: {block.fingerprint || "—"}
        </div>
      </details>
      {block.resolved ? (
        <div
          className="muted"
          style={{
            fontSize: 12.5,
            fontWeight: 700,
            color: block.resolved === "approved" ? "var(--c-success)" : "var(--c-danger)",
          }}
        >
          {block.resolved === "approved" ? "✓ 已允许并继续" : "✗ 已拒绝"}
        </div>
      ) : (
        <div className="approval-actions">
          <button
            type="button"
            className="btn-approve"
            data-testid="approval-allow"
            disabled={busy}
            onClick={() => decide(true)}
          >
            允许并继续
          </button>
          <button
            type="button"
            className="btn-deny"
            data-testid="approval-deny"
            disabled={busy}
            onClick={() => decide(false)}
          >
            拒绝
          </button>
        </div>
      )}
    </div>
  );
}

export function ChatTranscript({
  blocks,
  loading,
  onDecide,
  onSuggestion,
  capability,
}: {
  blocks: ChatBlock[];
  loading: boolean;
  onDecide: (approve: boolean, fingerprint: string, runId: string) => Promise<boolean>;
  onSuggestion: (text: string) => void;
  capability?: {
    hasWorkspace: boolean;
    hasConfig: boolean;
    onPickWorkspace?: () => void;
    onOpenSettings?: () => void;
  };
}) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [stick, setStick] = useState(true);

  useEffect(() => {
    if (!stick || !scrollRef.current) return;
    scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
  }, [blocks, stick]);

  if (loading && blocks.length === 0) {
    return (
      <div className="chat-scroll" ref={scrollRef}>
        <div className="empty-hero" data-testid="loading-session">
          <div className="empty-kicker">Cairn</div>
          <h2>加载会话…</h2>
          <p>正在读取运行事件与工作区状态。</p>
        </div>
      </div>
    );
  }

  if (blocks.length === 0) {
    return (
      <div className="chat-scroll" ref={scrollRef}>
        <div className="empty-hero">
          <div className="empty-kicker">New session</div>
          <h2>今天想改点什么？</h2>
          <p>描述目标即可。Cairn 会规划步骤、执行工具，并在有副作用时请你确认。</p>
          {capability && !capability.hasWorkspace && (
            <div className="list-card" data-testid="empty-need-workspace">
              <div className="title">先选择工作区</div>
              <div className="meta">没有工作区时无法装载配置或启动任务。</div>
              {capability.onPickWorkspace && (
                <button type="button" className="btn-sm primary" onClick={capability.onPickWorkspace}>
                  选择目录
                </button>
              )}
            </div>
          )}
          {capability && capability.hasWorkspace && !capability.hasConfig && (
            <div className="list-card" data-testid="empty-need-config">
              <div className="title">工作区缺少 .agent/config.yaml</div>
              <div className="meta">配置就绪后才能启动真实运行；不会写入演示任务。</div>
              {capability.onOpenSettings && (
                <button type="button" className="btn-sm" onClick={capability.onOpenSettings}>
                  打开设置查看诊断
                </button>
              )}
            </div>
          )}
          <div className="suggestion-row">
            {[
              "梳理仓库模块边界并给出重构建议",
              "跑一遍测试并总结失败原因",
              "给登录流程补一个回归测试",
              "解释 Event Journal 的哈希链设计",
            ].map((text) => (
              <button key={text} type="button" className="suggestion" onClick={() => onSuggestion(text)}>
                {text}
              </button>
            ))}
          </div>
        </div>
      </div>
    );
  }

  return (
    <div
      className="chat-scroll"
      ref={scrollRef}
      onScroll={(event) => {
        const el = event.currentTarget;
        setStick(el.scrollHeight - el.scrollTop - el.clientHeight < 56);
      }}
    >
      <div className="transcript">
        {blocks.map((block) => {
          if (block.kind === "user") {
            return (
              <div className="msg" data-sender="user" key={block.id}>
                <div className="msg-role">
                  <span className="msg-avatar">你</span>
                  <span>你 · {formatTime(block.at)}</span>
                </div>
                <div className="msg-body">
                  <div className="bubble">{block.text}</div>
                </div>
              </div>
            );
          }
          if (block.kind === "agent") {
            return (
              <div className="msg" data-sender="agent" key={block.id}>
                <div className="msg-role">
                  <span className="msg-avatar">C</span>
                  <span>Cairn · {formatTime(block.at)}</span>
                </div>
                <AssistantBody text={block.text} />
              </div>
            );
          }
          if (block.kind === "tool") return <ToolCard key={block.id} block={block} />;
          if (block.kind === "approval") {
            return <ApprovalCard key={block.id} block={block} onDecide={onDecide} />;
          }
          return (
            <div className="timeline-item" key={block.id}>
              <span>·</span>
              <span className="ts-label">{block.label}</span>
              {block.detail ? <span>{block.detail}</span> : null}
              <span>{formatTime(block.at)}</span>
            </div>
          );
        })}
      </div>
    </div>
  );
}

export type ChatError = ErrorBody;
