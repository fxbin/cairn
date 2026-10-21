import { useEffect, useMemo, useRef, useState } from "react";
import type { ChatBlock } from "../desktop/chatBlocks";
import type { ErrorBody } from "../desktop/types";

function formatTime(iso: string): string {
  if (!iso) {
    return "";
  }
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) {
    return "";
  }
  return date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" });
}

/** 简易 Markdown 子集：段落、代码块、行内代码、粗体。不执行 HTML。 */
export function AssistantBody({ text }: { text: string }) {
  const parts = useMemo(() => splitBlocks(text), [text]);
  return (
    <div className="msg-body">
      {parts.map((part, index) =>
        part.type === "code" ? (
          <pre key={index}>
            <code>{part.text}</code>
          </pre>
        ) : (
          <p key={index}>{renderInline(part.text)}</p>
        ),
      )}
    </div>
  );
}

function splitBlocks(text: string): Array<{ type: "p" | "code"; text: string }> {
  const out: Array<{ type: "p" | "code"; text: string }> = [];
  const segments = text.split(/```/);
  segments.forEach((segment, index) => {
    if (index % 2 === 1) {
      const cleaned = segment.replace(/^[a-zA-Z0-9_+-]*\n/, "");
      out.push({ type: "code", text: cleaned.replace(/\n$/, "") });
    } else if (segment.trim()) {
      out.push({ type: "p", text: segment.trim() });
    }
  });
  return out;
}

function renderInline(text: string) {
  const nodes: Array<string | JSX.Element> = [];
  const regex = /`([^`]+)`|\*\*([^*]+)\*\*/g;
  let last = 0;
  let match: RegExpExecArray | null;
  while ((match = regex.exec(text))) {
    if (match.index > last) {
      nodes.push(text.slice(last, match.index));
    }
    if (match[1] != null) {
      nodes.push(<code key={`${match.index}-c`}>{match[1]}</code>);
    } else if (match[2] != null) {
      nodes.push(<strong key={`${match.index}-b`}>{match[2]}</strong>);
    }
    last = match.index + match[0].length;
  }
  if (last < text.length) {
    nodes.push(text.slice(last));
  }
  return nodes;
}

function ToolCard({ block }: { block: Extract<ChatBlock, { kind: "tool" }> }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="tool-card" data-status={block.status}>
      <button type="button" className="tool-card-header" onClick={() => setOpen((v) => !v)}>
        <span className="tool-icon">{block.status === "failed" ? "!" : block.status === "ok" ? "✓" : "…"}</span>
        <span className="tool-name">{block.name}</span>
        <span className="tool-summary">{block.summary}</span>
        <span className="tool-meta">
          <span>{block.status === "running" ? "执行中" : block.status === "ok" ? "成功" : "失败"}</span>
          <span>{formatTime(block.at)}</span>
        </span>
      </button>
      {open && block.detail && <div className="tool-card-body">{block.detail}</div>}
    </div>
  );
}

function ApprovalCard({
  block,
  onDecide,
}: {
  block: Extract<ChatBlock, { kind: "approval" }>;
  onDecide: (approve: boolean, fingerprint: string, runId: string) => void;
}) {
  return (
    <div className="approval-card">
      <h3>
        <span>⚠</span> 需要你的确认
      </h3>
      <div className="approval-summary">{block.summary || "（无摘要）"}</div>
      <div className="muted" style={{ fontSize: 12 }}>
        运行 {block.runId}
        {block.operationId ? ` · 操作 ${block.operationId}` : ""}
      </div>
      {block.resolved ? (
        <div className="muted" style={{ fontSize: 12, fontWeight: 600 }}>
          {block.resolved === "approved" ? "已允许" : "已拒绝"}
        </div>
      ) : (
        <div className="approval-actions">
          <button type="button" className="btn-approve" onClick={() => onDecide(true, block.fingerprint, block.runId)}>
            允许并继续
          </button>
          <button type="button" className="btn-deny" onClick={() => onDecide(false, block.fingerprint, block.runId)}>
            拒绝
          </button>
        </div>
      )}
    </div>
  );
}

/** 对话主时间线。 */
export function ChatTranscript({
  blocks,
  loading,
  onDecide,
  onSuggestion,
}: {
  blocks: ChatBlock[];
  loading: boolean;
  onDecide: (approve: boolean, fingerprint: string, runId: string) => void;
  onSuggestion: (text: string) => void;
}) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [stick, setStick] = useState(true);

  useEffect(() => {
    if (!stick || !scrollRef.current) {
      return;
    }
    scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
  }, [blocks, stick]);

  if (loading && blocks.length === 0) {
    return (
      <div className="chat-scroll" ref={scrollRef}>
        <div className="empty-hero">
          <div className="muted">加载会话…</div>
        </div>
      </div>
    );
  }

  if (blocks.length === 0) {
    return (
      <div className="chat-scroll" ref={scrollRef}>
        <div className="empty-hero">
          <h2>今天想改点什么？</h2>
          <p>描述目标，Cairn 会规划步骤、执行工具，并在有副作用时请你确认。</p>
          <div className="suggestion-row">
            {[
              "梳理这个仓库的模块边界并写进 AGENTS.md 草稿",
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
        setStick(el.scrollHeight - el.scrollTop - el.clientHeight < 48);
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
          if (block.kind === "tool") {
            return <ToolCard key={block.id} block={block} />;
          }
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
