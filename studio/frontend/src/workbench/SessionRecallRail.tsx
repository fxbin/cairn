/**
 * INPUT: 对话消息块列表 ChatBlock[]、当前滚动激活锚点与跳转回调。
 * OUTPUT: 左侧“对话召回刻度尺”停靠站索引纯函数与鱼眼刻度尺 + 悬浮召回卡片组件。
 * POS: studio/frontend/src/workbench 长会话回溯定位器，只读投影对话流中的回合与关键节点。
 *
 * Author: fxbin
 */
import { useEffect, useRef, useState } from "react";
import { isWritePatchTool, type ChatBlock } from "../desktop/chatBlocks";
import { parseUserMessageAttachments } from "./composerContext";
import { stripWorkflowProtocolSuffix } from "./workflowConversation";

/**
 * 剥离工作流编排约定与 @ 文件引用上下文，提取适合召回卡片展示的用户原始提问文本。
 */
function cleanUserRecallBody(rawText: string): string {
  const withoutWorkflow = stripWorkflowProtocolSuffix(rawText).body;
  const parsed = parseUserMessageAttachments(withoutWorkflow);
  return parsed.body || withoutWorkflow;
}

/**
 * SessionRecallStop 描述左侧对话召回刻度尺中的单个定位锚点与预览摘要。
 */
export interface SessionRecallStop {
  id: string;
  kind: "turn" | "step" | "approval" | "patch";
  title: string;
  excerpt: string;
  badge?: string;
  badgeTone?: "is-warn" | "is-ok" | "is-accent" | "is-muted";
}

const MAX_TITLE_CHARS = 96;
const MAX_EXCERPT_CHARS = 180;
const CLOSE_GRACE_MS = 110;
const ESTIMATED_POPOVER_HEIGHT = 116;

/**
 * 为 Markdown 标题节点生成确定性召回锚点 ID，确保索引建表与正文 DOM 渲染严格一致。
 */
export function makeHeadingRecallId(blockId: string, headingText: string): string {
  const normalized = headingText
    .trim()
    .toLowerCase()
    .replace(/\s+/g, "-")
    .slice(0, 48);
  return `${blockId}--h-${normalized || "section"}`;
}

/**
 * 清洗 Markdown 格式记号与代码块，提取适合卡片预览的紧凑纯文本摘要。
 */
function cleanPreviewText(raw: string, maxLen = MAX_EXCERPT_CHARS): string {
  let text = raw
    .replace(/```[\s\S]*?```/g, " [代码] ")
    .replace(/```[\s\S]*$/g, " [代码] ")
    .replace(/~~~[\s\S]*?~~~/g, " [代码] ")
    .replace(/\[([^\]]+)\]\([^)]+\)/g, "$1")
    .replace(/`([^`]+)`/g, "$1")
    .replace(/^\s{0,3}#{1,6}\s+/gm, "")
    .replace(/^\s{0,3}>\s?/gm, "")
    .replace(/(\*\*|__|~~)/g, "")
    .replace(/\s+/g, " ")
    .trim();

  if (!text) return "";
  if (text.length > maxLen) {
    return `${text.slice(0, maxLen - 1).trimEnd()}…`;
  }
  return text;
}

/**
 * 将单个 Agent 消息切分为一二三级 Markdown 标题章节（忽略围栏代码块内的 # 行）。
 */
function extractHeadingSections(
  block: Extract<ChatBlock, { kind: "agent" }>,
): Array<{ id: string; title: string; excerpt: string }> {
  const lines = block.text.split(/\r?\n/);
  const sections: Array<{ rawTitle: string; bodyLines: string[] }> = [];
  let inFence = false;
  let current: { rawTitle: string; bodyLines: string[] } | null = null;

  for (const line of lines) {
    const trimmed = line.trim();
    if (/^(```|~~~)/.test(trimmed)) {
      inFence = !inFence;
      if (current) current.bodyLines.push(line);
      continue;
    }
    if (!inFence) {
      const headingMatch = /^#{1,3}\s+(.+?)\s*#*$/.exec(trimmed);
      if (headingMatch?.[1]) {
        if (current) sections.push(current);
        current = { rawTitle: headingMatch[1], bodyLines: [] };
        continue;
      }
    }
    if (current) {
      current.bodyLines.push(line);
    }
  }
  if (current) sections.push(current);

  const seen = new Set<string>();
  const out: Array<{ id: string; title: string; excerpt: string }> = [];
  for (const sec of sections) {
    const cleanTitle = cleanPreviewText(sec.rawTitle, MAX_TITLE_CHARS);
    if (!cleanTitle) continue;
    const anchor = makeHeadingRecallId(block.id, sec.rawTitle);
    if (seen.has(anchor)) continue;
    seen.add(anchor);
    const bodyPreview = cleanPreviewText(sec.bodyLines.join("\n"), MAX_EXCERPT_CHARS);
    out.push({
      id: anchor,
      title: cleanTitle,
      excerpt: bodyPreview || "见本节正文详情",
    });
  }
  return out;
}

/**
 * 为尚未产出 Agent 文本的回合或阶段生成状态/审批/工具回退摘要。
 */
function summarizeBlocksFallback(slice: ChatBlock[]): string {
  const pendingApproval = slice.find(
    (b): b is Extract<ChatBlock, { kind: "approval" }> => b.kind === "approval" && !b.resolved,
  );
  if (pendingApproval) {
    return `等待授权：${pendingApproval.summary}`;
  }

  const tools = slice.filter((b): b is Extract<ChatBlock, { kind: "tool" }> => b.kind === "tool");
  if (tools.length > 0) {
    const lastTool = tools[tools.length - 1];
    return `共 ${tools.length} 次工具调用 · 最近：${lastTool.name}${lastTool.targetPath ? ` (${lastTool.targetPath})` : ""}`;
  }

  for (let i = slice.length - 1; i >= 0; i -= 1) {
    const item = slice[i];
    if (item.kind === "status") {
      return item.detail ? `${item.label} · ${item.detail}` : item.label;
    }
  }
  return "已记录本轮任务上下文";
}

/**
 * 将审批块映射为独立的召回停靠节点预览描述。
 */
function formatApprovalStop(block: Extract<ChatBlock, { kind: "approval" }>): SessionRecallStop {
  const targetDetail =
    block.proposal?.targetPath ||
    block.proposal?.commandArgv ||
    (block.resolved === "approved"
      ? "已人工授权并继续执行"
      : block.resolved === "denied"
        ? "已拒绝执行该副作用操作"
        : "需要人工确认后方可执行");

  return {
    id: block.id,
    kind: "approval",
    title: cleanPreviewText(block.summary || "人工审批门禁", MAX_TITLE_CHARS),
    excerpt: cleanPreviewText(targetDetail, MAX_EXCERPT_CHARS),
    ...(block.resolved ? {} : { badge: "待授权", badgeTone: "is-warn" as const }),
  };
}

/**
 * 将按时间序合并后的对话块列表投影为左侧召回刻度尺停靠节点（少于 2 个节点时返回空列表）。
 * 同时兼容多轮续聊问答与单轮深度执行两种会话形态。
 */
export function buildSessionRecallStops(blocks: ChatBlock[]): SessionRecallStop[] {
  if (blocks.length === 0) return [];

  const userIndices: number[] = [];
  for (let i = 0; i < blocks.length; i += 1) {
    if (blocks[i].kind === "user") {
      userIndices.push(i);
    }
  }

  const rawStops: SessionRecallStop[] = [];

  if (userIndices.length >= 2) {
    for (let uIdx = 0; uIdx < userIndices.length; uIdx += 1) {
      const start = userIndices[uIdx];
      const end = uIdx + 1 < userIndices.length ? userIndices[uIdx + 1] : blocks.length;
      const userBlock = blocks[start] as Extract<ChatBlock, { kind: "user" }>;
      const turnSlice = blocks.slice(start + 1, end);

      const userTitle =
        cleanPreviewText(cleanUserRecallBody(userBlock.text), MAX_TITLE_CHARS) || "用户输入";
      const agentBlocks = turnSlice.filter(
        (b): b is Extract<ChatBlock, { kind: "agent" }> => b.kind === "agent",
      );
      const hasPendingApproval = turnSlice.some(
        (b) => b.kind === "approval" && !b.resolved,
      );
      const joinedAgent = cleanPreviewText(
        agentBlocks.map((b) => b.text).join("\n\n"),
        MAX_EXCERPT_CHARS,
      );

      rawStops.push({
        id: userBlock.id,
        kind: "turn",
        title: userTitle,
        excerpt: joinedAgent || summarizeBlocksFallback(turnSlice),
        ...(hasPendingApproval ? { badge: "待授权", badgeTone: "is-warn" as const } : {}),
      });

      for (const item of turnSlice) {
        if (item.kind === "approval" && !item.resolved) {
          rawStops.push(formatApprovalStop(item));
        }
      }

      if (agentBlocks.length >= 3) {
        for (let aIdx = 1; aIdx < agentBlocks.length; aIdx += 1) {
          const stepBlock = agentBlocks[aIdx];
          const firstLine = stepBlock.text.split(/\r?\n/).find((line) => line.trim().length > 0) ?? "";
          rawStops.push({
            id: stepBlock.id,
            kind: "step",
            title: cleanPreviewText(firstLine || `第 ${aIdx + 1} 段回复`, MAX_TITLE_CHARS),
            excerpt: cleanPreviewText(stepBlock.text, MAX_EXCERPT_CHARS),
          });
        }
      }
    }
  } else {
    const agentBlocks = blocks.filter(
      (b): b is Extract<ChatBlock, { kind: "agent" }> => b.kind === "agent",
    );
    const hasInitialUser = userIndices.length === 1;
    let seenAgentCount = 0;

    for (let i = 0; i < blocks.length; i += 1) {
      const block = blocks[i];
      if (block.kind === "user") {
        const userTitle =
          cleanPreviewText(cleanUserRecallBody(block.text), MAX_TITLE_CHARS) || "任务指令";
        const firstAgent = agentBlocks[0];
        const excerpt = firstAgent
          ? cleanPreviewText(firstAgent.text, MAX_EXCERPT_CHARS)
          : summarizeBlocksFallback(blocks.slice(i + 1));
        rawStops.push({
          id: block.id,
          kind: "turn",
          title: userTitle,
          excerpt,
        });
        continue;
      }

      if (block.kind === "agent") {
        const currentAgentIndex = seenAgentCount;
        seenAgentCount += 1;

        if (!hasInitialUser || currentAgentIndex >= 1) {
          const firstLine = block.text.split(/\r?\n/).find((line) => line.trim().length > 0) ?? "";
          rawStops.push({
            id: block.id,
            kind: "step",
            title: cleanPreviewText(firstLine || `助手回应 #${currentAgentIndex + 1}`, MAX_TITLE_CHARS),
            excerpt: cleanPreviewText(block.text, MAX_EXCERPT_CHARS),
          });
        }

        if (agentBlocks.length <= 2) {
          const headings = extractHeadingSections(block);
          if (headings.length >= 2) {
            for (const sec of headings) {
              rawStops.push({
                id: sec.id,
                kind: "step",
                title: sec.title,
                excerpt: sec.excerpt,
              });
            }
          }
        }
        continue;
      }

      if (block.kind === "approval") {
        rawStops.push(formatApprovalStop(block));
        continue;
      }

      if (block.kind === "tool" && isWritePatchTool(block.name)) {
        const target = block.targetPath || block.summary || block.name;
        const statusLabel =
          block.status === "ok" ? "已完成写入" : block.status === "failed" ? "写入失败" : "正在执行";
        rawStops.push({
          id: block.id,
          kind: "patch",
          title: cleanPreviewText(`${block.name} · ${target}`, MAX_TITLE_CHARS),
          excerpt: `状态：${statusLabel}${block.summary && block.summary !== target ? ` · ${block.summary}` : ""}`,
        });
      }
    }
  }

  const deduped: SessionRecallStop[] = [];
  const seenIds = new Set<string>();
  for (const stop of rawStops) {
    if (!stop.id || seenIds.has(stop.id)) continue;
    seenIds.add(stop.id);
    deduped.push(stop);
  }

  return deduped.length >= 2 ? deduped : [];
}

export interface SessionRecallRailProps {
  stops: SessionRecallStop[];
  activeId: string | null;
  onSelect: (id: string) => void;
}

/**
 * 渲染会话画布左侧鱼眼对话召回刻度尺与悬浮预览卡；支持悬停预览与点击锚点跳转。
 */
export default function SessionRecallRail({ stops, activeId, onSelect }: SessionRecallRailProps) {
  const [hoveredIndex, setHoveredIndex] = useState<number | null>(null);
  const [popoverTop, setPopoverTop] = useState(0);
  const railRef = useRef<HTMLElement>(null);
  const closeTimerRef = useRef<number | null>(null);

  useEffect(() => {
    return () => {
      if (closeTimerRef.current !== null) {
        window.clearTimeout(closeTimerRef.current);
      }
    };
  }, []);

  useEffect(() => {
    setHoveredIndex((prev) => (prev !== null && prev < stops.length ? prev : null));
  }, [stops.length]);

  if (stops.length < 2) {
    return null;
  }

  const activeIdx = stops.findIndex((item) => item.id === activeId);
  const focusIndex = hoveredIndex !== null ? hoveredIndex : activeIdx >= 0 ? activeIdx : 0;

  const cancelClose = () => {
    if (closeTimerRef.current !== null) {
      window.clearTimeout(closeTimerRef.current);
      closeTimerRef.current = null;
    }
  };

  const scheduleClose = () => {
    cancelClose();
    closeTimerRef.current = window.setTimeout(() => {
      setHoveredIndex(null);
      closeTimerRef.current = null;
    }, CLOSE_GRACE_MS);
  };

  const revealStopAt = (idx: number, triggerEl: HTMLElement | null) => {
    cancelClose();
    setHoveredIndex(idx);
    const railEl = railRef.current;
    if (!railEl || !triggerEl) return;
    const railRect = railEl.getBoundingClientRect();
    const btnRect = triggerEl.getBoundingClientRect();
    const rawTop = btnRect.top - railRect.top - 12;
    const maxTop = Math.max(0, railEl.clientHeight - ESTIMATED_POPOVER_HEIGHT);
    setPopoverTop(Math.min(Math.max(0, rawTop), maxTop));
  };

  const hoveredStop = hoveredIndex !== null ? stops[hoveredIndex] : undefined;

  return (
    <aside
      ref={railRef}
      className="wb-turn-rail"
      data-testid="session-recall-rail"
      aria-label="对话召回导航"
      onMouseLeave={scheduleClose}
    >
      <div className="wb-turn-rail-track" role="navigation" aria-label="对话节点刻度尺">
        {stops.map((stop, idx) => {
          const distance = Math.abs(idx - focusIndex);
          const classes = ["wb-turn-rail-item", `kind-${stop.kind}`];
          if (distance === 0) classes.push("is-current");
          else if (distance === 1) classes.push("is-near-1");
          else if (distance === 2) classes.push("is-near-2");
          if (stop.badgeTone === "is-warn") classes.push("is-warn");

          return (
            <button
              key={stop.id}
              type="button"
              className={classes.join(" ")}
              aria-label={`跳转到：${stop.title}`}
              aria-current={idx === (activeIdx >= 0 ? activeIdx : 0) ? "true" : undefined}
              onMouseEnter={(e) => revealStopAt(idx, e.currentTarget)}
              onFocus={(e) => revealStopAt(idx, e.currentTarget)}
              onBlur={scheduleClose}
              onClick={() => onSelect(stop.id)}
            >
              <span className="wb-turn-rail-bar" aria-hidden="true" />
            </button>
          );
        })}
      </div>

      {hoveredStop && (
        <div
          className="wb-turn-rail-popover"
          data-testid="session-recall-popover"
          role="button"
          tabIndex={0}
          style={{ top: popoverTop }}
          onMouseEnter={cancelClose}
          onMouseLeave={scheduleClose}
          onClick={() => onSelect(hoveredStop.id)}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") {
              e.preventDefault();
              onSelect(hoveredStop.id);
            }
          }}
        >
          <div className="wb-turn-rail-card-head">
            <strong className="wb-turn-rail-card-title">{hoveredStop.title}</strong>
            {hoveredStop.badge && (
              <span className={"wb-badge " + (hoveredStop.badgeTone || "is-muted")}>
                {hoveredStop.badge}
              </span>
            )}
          </div>
          {hoveredStop.excerpt && (
            <p className="wb-turn-rail-card-body">{hoveredStop.excerpt}</p>
          )}
        </div>
      )}
    </aside>
  );
}
