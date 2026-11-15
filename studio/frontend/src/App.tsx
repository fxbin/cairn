/**
 * Cairn Studio GUI：主工作台（Workbench）入口。
 * 只消费现有 desktop 桥接与 Journal 投影，不复制任何 Agent 运行逻辑。
 * Author: fxbin
 */
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { desktop } from "./desktop/client";
import DiffViewer from "./workbench/DiffViewer";
import { ReconciliationPanel, AbortRunPanel } from "./workbench/RecoveryPanel";
import { QueuePanel } from "./workbench/QueuePanel";
import CapabilitySettings, {
  type CapabilityCounts,
  type CapabilitySection,
} from "./workbench/CapabilitySettings";
import ProviderSettings from "./workbench/ProviderSettings";
import UsageView from "./workbench/UsageView";
import FilesPanel from "./workbench/FilesPanel";
import OpenLocationIcon from "./workbench/OpenLocationIcon";
import SessionRow from "./workbench/SessionRow";
import WorkflowsView from "./workbench/WorkflowsView";
import {
  filterTimelineByKind,
  toContextCompactions,
  toContextUsage,
  toSubagentViews,
  toTimeline,
} from "./desktop/timeline";
import type { TimelineKind } from "./desktop/timeline";
import {
  buildChatBlocks,
  isReadOnlyTool,
  isWritePatchTool,
} from "./desktop/chatBlocks";
import type { ChatBlock } from "./desktop/chatBlocks";
import type {
  ChangeEntry,
  ChangeSet,
  EvidenceView,
  HealthResult,
  ProviderPresetView,
  RecentProject,
  RunDetail,
  RunHistoryView,
  SessionTreeView,
  StreamEvent,
  WorkspaceStatus,
} from "./desktop/types";

type Panel = "closed" | "workbench" | "changes" | "evidence" | "queue" | "subagents" | "files";
type Page = "home" | "workflow" | "settings";
type HeroPopover = "none" | "workspace" | "model";
type ActivityTab = "heatmap" | "overview";
type OpenLocationItem = {
  id: string;
  label: string;
  available: boolean;
};
type HeatmapCell = {
  dateKey: string;
  count: number;
  level: 0 | 1 | 2 | 3 | 4;
};
type LocalActivitySummary = {
  cells: HeatmapCell[];
  months: string[];
  activeDays: number;
  waitingCount: number;
  completedCount: number;
  activeRunCount: number;
};
type SettingsCategory =
  | "general"
  | "appearance"
  | "models"
  | "shortcuts"
  | CapabilitySection
  | "usage"
  | "about";
type SettingsGroupKey = "foundation" | "capabilities" | "diagnostics";
type ColorSchemeMode = "system" | "light" | "dark";
type RunInputMode = "steering" | "queue";

type SettingsNavItem = {
  id: SettingsCategory;
  group: SettingsGroupKey;
  label: string;
  subtitle: string;
  icon: string;
  keywords: string;
  capabilityKey?: CapabilitySection;
};

type DoctorSummaryView = {
  ready?: boolean;
  config_valid?: boolean;
  journal_ok?: boolean;
  artifacts_ok?: boolean;
  memory_ok?: boolean;
  journal_state?: string;
  profile?: string;
  preset?: string;
  config_fingerprint?: string;
};

type ToolChatBlock = Extract<ChatBlock, { kind: "tool" }>;

type TranscriptRenderItem =
  | { kind: "explore_group"; id: string; tools: ToolChatBlock[] }
  | { kind: "single"; block: ChatBlock };

const finished = new Set(["completed", "failed", "cancelled"]);
const icon = "/icons/cairn-icon-128.png";

const SETTINGS_GROUPS: ReadonlyArray<{ key: SettingsGroupKey; label: string }> = [
  { key: "foundation", label: "基础设置" },
  { key: "capabilities", label: "Agent 能力" },
  { key: "diagnostics", label: "数据与诊断" },
];

const SETTINGS_NAV_ITEMS: readonly SettingsNavItem[] = [
  {
    id: "general",
    group: "foundation",
    label: "常规",
    subtitle: "管理当前工作区目录、Git 仓库状态、配置文件指纹与最近打开的项目。",
    icon: "⚙",
    keywords: "常规 通用 工作区 项目 目录 git 分支 配置 指纹 最近项目 归档 workspace project general config",
  },
  {
    id: "appearance",
    group: "foundation",
    label: "外观",
    subtitle: "定制宣纸暖石与玄武岩夜色双生主题、界面基础字号与代码阅读排版。",
    icon: "◐",
    keywords: "外观 主题 暗色 亮色 浅色 深色 跟随系统 宣纸白 玄武岩黑 字号 字体 排版 代码 等宽 终端 theme dark light font size appearance",
  },
  {
    id: "models",
    group: "foundation",
    label: "模型与供应商",
    subtitle: "配置模型预设、环境变量密钥引用与两阶段工具往返准入验证。",
    icon: "◈",
    keywords: "provider 模型 供应商 api key deepseek openai anthropic claude gemini ollama 探测 准入 预设 base url",
  },
  {
    id: "shortcuts",
    group: "foundation",
    label: "键盘快捷键",
    subtitle: "工作台真实绑定的键盘快捷键速查，所有输入框均内置中文 IME 组合态防误触保护。",
    icon: "⌘",
    keywords: "快捷键 键盘 enter esc escape cmd ctrl 热键 发送 换行 设置 diff 批注 shortcut keymap",
  },
  {
    id: "skills",
    group: "capabilities",
    label: "技能",
    subtitle: "管理工作区与用户级 Skill 扩展的安装、启用与移除；Skill 不得自动授予执行权限。",
    icon: "✦",
    keywords: "skill skills 技能 扩展 安装 启用 移除",
    capabilityKey: "skills",
  },
  {
    id: "mcp",
    group: "capabilities",
    label: "MCP 服务器",
    subtitle: "检视当前工作区声明的外部 MCP 服务状态；协议或 schema 漂移即失败关闭。",
    icon: "☍",
    keywords: "mcp 服务器 server 工具 协议 外部",
    capabilityKey: "mcp",
  },
  {
    id: "memory",
    group: "capabilities",
    label: "记忆",
    subtitle: "审阅运行沉淀的二级经验候选、执行巩固或凭明确理由撤销失真记忆。",
    icon: "◎",
    keywords: "memory 记忆 经验 巩固 撤销 候选 lesson",
    capabilityKey: "memory",
  },
  {
    id: "commands",
    group: "capabilities",
    label: "命令",
    subtitle: "查看当前内置与扩展注册的可调用命令目录及其生效作用域。",
    icon: "／",
    keywords: "command commands 命令 斜杠 快捷指令",
    capabilityKey: "commands",
  },
  {
    id: "hooks",
    group: "capabilities",
    label: "钩子",
    subtitle: "管理允许列表内的生命周期钩子；钩子只能收紧策略或入队候选，不可扩大权限。",
    icon: "⚡",
    keywords: "hook hooks 钩子 触发器 入队 自动 run_finished",
    capabilityKey: "hooks",
  },
  {
    id: "instructions",
    group: "capabilities",
    label: "项目规约",
    subtitle: "查看与维护工作区 AGENTS.md 等规约文档；规约仅作上下文约束，不授予执行权限。",
    icon: "▤",
    keywords: "agents agents.md 规约 指令 项目规约 项目指令 规则 instructions rules",
    capabilityKey: "instructions",
  },
  {
    id: "usage",
    group: "diagnostics",
    label: "用量与统计",
    subtitle: "基于本地已持久化运行证据汇总 Token 消耗、回合调用与估算标记，不对未知价格做猜测。",
    icon: "◔",
    keywords: "token tokens 用量 统计 费用 成本 消耗 回合 usage cost",
  },
  {
    id: "about",
    group: "diagnostics",
    label: "环境诊断与关于",
    subtitle: "查看客户端构建环境、本地优先隐私边界，并对当前工作区执行只读 Doctor 健康诊断。",
    icon: "ⓘ",
    keywords: "doctor 诊断 版本 关于 健康 环境 go 隐私 遥测 journal ~/.cairn about version health",
  },
];

/** 将任意错误对象转换为可读错误描述字符串。 */
function textError(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** 读取本机保存的外观配色偏好，异常或未设置时回退为跟随系统。 */
function readSavedColorScheme(): ColorSchemeMode {
  try {
    const saved =
      localStorage.getItem("cairn.gui.colorScheme") ??
      localStorage.getItem("cairn.v2.colorScheme");
    if (saved === "light" || saved === "dark") {
      return saved;
    }
  } catch {
    // 忽略受限环境下的本地存储异常。
  }
  return "system";
}

/** 读取本机保存的基础字号偏好，限制在合理排版区间内。 */
function readSavedFontSize(): number {
  try {
    const stored =
      localStorage.getItem("cairn.gui.fontSize") ??
      localStorage.getItem("cairn.v2.fontSize");
    const raw = Number(stored || 14);
    if (Number.isFinite(raw)) {
      return Math.min(19, Math.max(12, raw));
    }
  } catch {
    // 忽略本地存储读取异常。
  }
  return 14;
}

/**
 * 获取工作区变更文件的可审查差异文本。
 * Git 无法为未跟踪二进制文件生成文本差异；在真实文件预览权限内展示降级状态。
 */
async function reviewFileText(workspace: string, relative: string, status: string): Promise<string> {
  try {
    return await desktop.diffFile(workspace, relative);
  } catch (error) {
    if (status !== "untracked") throw error;
    const preview = await desktop.previewFile(workspace, relative);
    if (preview.binary) {
      return "未跟踪二进制文件（无法生成文本 Diff）\n请在「文件」页查看属性或使用外部编辑器打开。";
    }
    if (!preview.text) return "未跟踪文件无可预览的文本内容。";
    const lines = preview.text.replace(/\n$/, "").split("\n");
    return [
      "diff --git a/" + relative + " b/" + relative,
      "new file mode (preview only)",
      "--- /dev/null",
      "+++ b/" + relative,
      "@@ -0,0 +1," + lines.length + " @@",
      ...lines.map((line) => "+" + line),
      ...(preview.truncated ? ["… 内容已截断，仅预览前 256KB"] : []),
    ].join("\n");
  }
}

/** 提取运行状态的展示名称，优先消费服务端词表投影，不在前端猜测终态。 */
function statusName(run: RunHistoryView | undefined): string {
  if (!run) return "就绪";
  return run.phase_label || run.phase || run.state;
}

/** 判断对话块是否为已完成的只读探索工具块。 */
function isCompletedReadOnlyToolBlock(block: ChatBlock): block is ToolChatBlock {
  return (
    block.kind === "tool" &&
    block.status === "ok" &&
    Boolean(block.isReadOnly ?? isReadOnlyTool(block.name))
  );
}

/** 将连续两个及以上已完成的只读探索工具块折叠为聚合批次，减少对话流刷屏。 */
function groupTranscriptBlocks(blocks: ChatBlock[]): TranscriptRenderItem[] {
  const items: TranscriptRenderItem[] = [];
  let idx = 0;
  while (idx < blocks.length) {
    const current = blocks[idx];
    if (isCompletedReadOnlyToolBlock(current)) {
      const runSlice: ToolChatBlock[] = [current];
      let cursor = idx + 1;
      while (cursor < blocks.length) {
        const candidate = blocks[cursor];
        if (!isCompletedReadOnlyToolBlock(candidate)) break;
        runSlice.push(candidate);
        cursor += 1;
      }
      if (runSlice.length >= 2) {
        items.push({
          kind: "explore_group",
          id: `explore-${current.id}`,
          tools: runSlice,
        });
        idx = cursor;
        continue;
      }
    }
    items.push({ kind: "single", block: current });
    idx += 1;
  }
  return items;
}

/** 汇总探索批次内的工具调用分布文案。 */
function summarizeExploreGroup(tools: ToolChatBlock[]): string {
  const counts = new Map<string, number>();
  for (const item of tools) {
    counts.set(item.name, (counts.get(item.name) ?? 0) + 1);
  }
  const breakdown = Array.from(counts.entries())
    .slice(0, 3)
    .map(([name, count]) => `${name} ×${count}`)
    .join(" · ");
  return breakdown
    ? `已聚合 ${tools.length} 次只读探索（${breakdown}）`
    : `已聚合 ${tools.length} 次只读探索`;
}

/** 在工作区变更条目列表中定位与工具目标路径最匹配的相对路径。 */
function matchChangedRelativePath(entries: ChangeEntry[], targetPath: string): string {
  const normalized = targetPath.trim().replace(/^\.\//, "").replace(/^\/+/, "");
  if (!normalized) return entries[0]?.relative || "";
  const exact = entries.find((entry) => entry.relative === normalized || entry.relative === targetPath.trim());
  if (exact) return exact.relative;
  const suffix = entries.find(
    (entry) =>
      normalized.endsWith("/" + entry.relative) || entry.relative.endsWith("/" + normalized),
  );
  if (suffix) return suffix.relative;
  return normalized;
}

/** 计算会话树中各运行的层级深度与直接子运行数量。 */
function computeSessionTreeMeta(tree: SessionTreeView | null): {
  depthMap: Map<string, number>;
  childCountMap: Map<string, number>;
} {
  const depthMap = new Map<string, number>();
  const childCountMap = new Map<string, number>();
  if (!tree || !tree.nodes) {
    return { depthMap, childCountMap };
  }
  for (const [id, node] of Object.entries(tree.nodes)) {
    childCountMap.set(id, node.children?.length ?? 0);
  }
  const visited = new Set<string>();
  const walk = (id: string, depth: number) => {
    if (visited.has(id)) return;
    visited.add(id);
    depthMap.set(id, depth);
    const children = tree.nodes[id]?.children || [];
    for (const childId of children) {
      walk(childId, depth + 1);
    }
  };
  for (const rootId of tree.roots || []) {
    walk(rootId, 0);
  }
  return { depthMap, childCountMap };
}

/** 判断设置分类是否属于六个 Agent 能力子页之一。 */
function isCapabilitySection(category: SettingsCategory): category is CapabilitySection {
  return (
    category === "skills" ||
    category === "mcp" ||
    category === "memory" ||
    category === "commands" ||
    category === "hooks" ||
    category === "instructions"
  );
}

/** 按用户输入词元对设置导航项的名称、副标题与内置中英文关键词进行实时过滤。 */
function filterSettingsNavItems(
  items: readonly SettingsNavItem[],
  query: string,
): SettingsNavItem[] {
  const normalized = query.trim().toLowerCase();
  if (!normalized) return [...items];
  const tokens = normalized.split(/\s+/).filter(Boolean);
  return items.filter((item) => {
    const haystack = `${item.label} ${item.subtitle} ${item.id} ${item.keywords}`.toLowerCase();
    return tokens.every((token) => haystack.includes(token));
  });
}

/** 读取指定 Agent 能力分类对应的导航角标计数；未就绪或 MCP 非数组状态返回 null。 */
function readCapabilityCount(
  counts: CapabilityCounts | null,
  section: CapabilitySection | undefined,
): number | null {
  if (!counts || !section) return null;
  const value = counts[section];
  return typeof value === "number" ? value : null;
}

/**
 * 将本地 Date 实例格式化为 YYYY-MM-DD 自然日键，用于本机会话活跃度聚合。
 */
function formatLocalDayKey(date: Date): string {
  const y = date.getFullYear();
  const m = String(date.getMonth() + 1).padStart(2, "0");
  const d = String(date.getDate()).padStart(2, "0");
  return `${y}-${m}-${d}`;
}

/**
 * 基于本机已加载的运行记录聚合过去 24 周 × 7 天（168 格）真实会话热力图与运行状态概览，绝不伪造云端数据。
 */
function buildLocalActivitySummary(runs: RunHistoryView[]): LocalActivitySummary {
  const dayCounts = new Map<string, number>();
  let waitingCount = 0;
  let completedCount = 0;
  let activeRunCount = 0;

  for (const run of runs) {
    if (!run.archived && (run.waiting_approval || run.waiting_reconcile)) {
      waitingCount += 1;
    }
    if (run.state === "completed") {
      completedCount += 1;
    }
    if (!finished.has(run.state) && !run.waiting_approval && !run.waiting_reconcile) {
      activeRunCount += 1;
    }
    const rawTime = run.created_at || run.updated_at;
    if (!rawTime) continue;
    const parsed = Date.parse(rawTime);
    if (Number.isNaN(parsed)) continue;
    const key = formatLocalDayKey(new Date(parsed));
    dayCounts.set(key, (dayCounts.get(key) ?? 0) + 1);
  }

  const now = new Date();
  const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const totalDays = 24 * 7;
  const cells: HeatmapCell[] = [];
  const months: string[] = [];
  let lastMonthLabel = "";

  for (let i = 0; i < totalDays; i += 1) {
    const offset = totalDays - 1 - i;
    const d = new Date(today.getFullYear(), today.getMonth(), today.getDate() - offset);
    const dateKey = formatLocalDayKey(d);
    const count = dayCounts.get(dateKey) ?? 0;
    const level: 0 | 1 | 2 | 3 | 4 =
      count === 0 ? 0 : count === 1 ? 1 : count === 2 ? 2 : count <= 4 ? 3 : 4;
    cells.push({ dateKey, count, level });

    if (i % 28 === 0 || i === totalDays - 7) {
      const mLabel = `${d.getMonth() + 1}月`;
      if (mLabel !== lastMonthLabel) {
        months.push(mLabel);
        lastMonthLabel = mLabel;
      }
    }
  }

  return {
    cells,
    months,
    activeDays: dayCounts.size,
    waitingCount,
    completedCount,
    activeRunCount,
  };
}

/** 从只读 Doctor 诊断返回载荷中提取非敏感状态摘要，供状态徽标渲染使用。 */
function extractDoctorSummary(raw: unknown): DoctorSummaryView | null {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
    return null;
  }
  const source = raw as Partial<Record<keyof DoctorSummaryView, unknown>>;
  const summary: DoctorSummaryView = {};
  if (typeof source.ready === "boolean") summary.ready = source.ready;
  if (typeof source.config_valid === "boolean") summary.config_valid = source.config_valid;
  if (typeof source.journal_ok === "boolean") summary.journal_ok = source.journal_ok;
  if (typeof source.artifacts_ok === "boolean") summary.artifacts_ok = source.artifacts_ok;
  if (typeof source.memory_ok === "boolean") summary.memory_ok = source.memory_ok;
  if (typeof source.journal_state === "string") summary.journal_state = source.journal_state;
  if (typeof source.profile === "string") summary.profile = source.profile;
  if (typeof source.preset === "string") summary.preset = source.preset;
  if (typeof source.config_fingerprint === "string") {
    summary.config_fingerprint = source.config_fingerprint;
  }
  return Object.keys(summary).length > 0 ? summary : null;
}

/**
 * 渲染带图标弹簧缩放旋转与里程表滚轮文字翻转的一键复制微交互按钮。
 * 仅用于复制工作区目录与用户配置目录等非敏感路径，不接触任何密钥文本。
 */
function SettingsCopyButton({
  value,
  ariaLabel,
}: {
  value: string;
  ariaLabel: string;
}) {
  const [copied, setCopied] = useState(false);
  const timerRef = useRef<number | null>(null);

  useEffect(() => {
    return () => {
      if (timerRef.current !== null) {
        window.clearTimeout(timerRef.current);
      }
    };
  }, []);

  const handleCopy = async () => {
    if (!value) return;
    try {
      if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(value);
      }
    } catch {
      // 受限沙箱或无剪贴板权限环境下静默降级，不阻塞界面交互。
    }
    setCopied(true);
    if (timerRef.current !== null) {
      window.clearTimeout(timerRef.current);
    }
    timerRef.current = window.setTimeout(() => {
      setCopied(false);
      timerRef.current = null;
    }, 1600);
  };

  return (
    <button
      type="button"
      className={"wb-copy-btn" + (copied ? " is-copied" : "")}
      aria-label={ariaLabel}
      onClick={() => void handleCopy()}
    >
      <span className="wb-copy-icon-wrap" aria-hidden="true">
        <svg
          className="wb-icon-copy"
          width="12"
          height="12"
          viewBox="0 0 16 16"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.6"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <rect x="5.5" y="5.5" width="8" height="8" rx="1.8" />
          <path d="M10.5 5.5V3.8A1.3 1.3 0 0 0 9.2 2.5H3.8A1.3 1.3 0 0 0 2.5 3.8v5.4a1.3 1.3 0 0 0 1.3 1.3h1.7" />
        </svg>
        <svg
          className="wb-icon-check"
          width="12"
          height="12"
          viewBox="0 0 16 16"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.9"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <path d="M3.2 8.4l3.1 3.1 6.5-6.8" />
        </svg>
      </span>
      <span className="wb-copy-roller" aria-hidden="true">
        <span className={"wb-copy-label " + (copied ? "is-exit-up" : "is-enter-center")}>复制</span>
        <span className={"wb-copy-label " + (copied ? "is-enter-center" : "is-enter-down")}>
          已复制
        </span>
      </span>
    </button>
  );
}

/** 渲染对话时间线，包含探索聚合组、结构化补丁卡与指纹绑定审批门禁。 */
function Transcript({
  blocks,
  onApproval,
  onInspectFile,
}: {
  blocks: ChatBlock[];
  onApproval: (approve: boolean, fingerprint: string, runId: string) => void;
  onInspectFile: (targetPath: string) => void;
}) {
  const grouped = useMemo(() => groupTranscriptBlocks(blocks), [blocks]);

  return (
    <div className="wb-transcript" aria-live="polite">
      {grouped.map((entry) => {
        if (entry.kind === "explore_group") {
          return (
            <details className="wb-explore-group" key={entry.id}>
              <summary>
                <span className="wb-badge is-ok">✓ 探索</span>
                <strong>{summarizeExploreGroup(entry.tools)}</strong>
                <span>展开明细</span>
              </summary>
              <div className="wb-explore-list">
                {entry.tools.map((tool) => (
                  <div key={tool.id}>
                    <span className="wb-tool-mark">⌁</span>
                    <strong>{tool.name}</strong>
                    <span>{tool.summary}</span>
                  </div>
                ))}
              </div>
            </details>
          );
        }

        const { block } = entry;
        if (block.kind === "user") {
          return (
            <div className="wb-message-user" key={block.id}>
              {block.text}
            </div>
          );
        }
        if (block.kind === "agent") {
          return (
            <article className="wb-message-agent" key={block.id}>
              <ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml>
                {block.text}
              </ReactMarkdown>
            </article>
          );
        }
        if (block.kind === "tool") {
          if (isWritePatchTool(block.name)) {
            const statusTone =
              block.status === "ok" ? "is-ok" : block.status === "failed" ? "is-warn" : "is-accent";
            const statusText =
              block.status === "ok" ? "完成" : block.status === "failed" ? "失败" : "执行中";
            return (
              <div className="wb-patch-card" key={block.id}>
                <div>
                  <span className={"wb-badge " + statusTone}>{statusText}</span>
                  <strong>{block.name}</strong>
                  <span>{block.targetPath || block.summary}</span>
                  {block.targetPath && (
                    <button
                      type="button"
                      className="wb-secondary"
                      onClick={() => onInspectFile(block.targetPath!)}
                    >
                      审查 Diff →
                    </button>
                  )}
                </div>
                {block.detail && (
                  <details open={block.status === "failed"}>
                    <summary>查看工具载荷</summary>
                    <pre>{block.detail}</pre>
                  </details>
                )}
              </div>
            );
          }
          const isFailed = block.status === "failed";
          const isReadOnlyDone = isCompletedReadOnlyToolBlock(block);
          return (
            <details
              className={"wb-tool" + (isFailed ? " is-failed" : "")}
              data-status={block.status}
              key={block.id}
              open={isFailed}
            >
              <summary>
                <span className="wb-tool-mark" aria-hidden="true">
                  {isReadOnlyDone ? "✓" : isFailed ? "!" : "⌁"}
                </span>
                <strong>{block.name}</strong>
                <span>{block.summary || block.targetPath || "已执行工具调用"}</span>
                <em>
                  {block.status === "ok" ? "完成" : isFailed ? "失败" : "执行中"}
                </em>
              </summary>
              {block.detail && <pre>{block.detail}</pre>}
            </details>
          );
        }
        if (block.kind === "approval") {
          return (
            <section className="wb-approval" key={block.id}>
              <div>
                <div className="wb-label">等待你的授权</div>
                {block.operationId && (
                  <span className="wb-badge is-warn">#{block.operationId}</span>
                )}
              </div>
              <strong>{block.summary}</strong>
              <p>此操作具有副作用。仅批准当前指纹对应的操作，不会扩大后续运行权限。</p>
              <details>
                <summary>查看操作指纹</summary>
                <code className="wb-approval-code">{block.fingerprint}</code>
              </details>
              {block.resolved ? (
                <div className="wb-label">{block.resolved === "approved" ? "已授权" : "已拒绝"}</div>
              ) : (
                <div className="wb-actions">
                  <button
                    type="button"
                    className="wb-primary"
                    onClick={() => onApproval(true, block.fingerprint, block.runId)}
                  >
                    允许并继续
                  </button>
                  <button
                    type="button"
                    onClick={() => onApproval(false, block.fingerprint, block.runId)}
                  >
                    拒绝
                  </button>
                </div>
              )}
            </section>
          );
        }
        return (
          <div className="wb-status" key={block.id}>
            <span>●</span>
            {block.label}
            {block.detail ? " · " + block.detail : ""}
          </div>
        );
      })}
    </div>
  );
}

/** 渲染 Cairn Studio GUI 主工作台壳层、统一智能指令舱与右侧检视面板。 */
export default function App() {
  const [recent, setRecent] = useState<RecentProject[]>([]);
  const [workspace, setWorkspace] = useState("");
  const [workspaceStatus, setWorkspaceStatus] = useState<WorkspaceStatus | null>(null);
  const [bridgeStatus, setBridgeStatus] = useState<"connecting" | "healthy" | "disconnected">(
    "connecting",
  );
  const [runs, setRuns] = useState<RunHistoryView[]>([]);
  const [runId, setRunId] = useState<string | null>(null);
  const [detail, setDetail] = useState<RunDetail | null>(null);
  const [liveEvents, setLiveEvents] = useState<StreamEvent[]>([]);
  const [draft, setDraft] = useState("");
  const [sendingText, setSendingText] = useState("");
  const [sending, setSending] = useState(false);
  const [queueRevision, setQueueRevision] = useState(0);
  const [pendingQueueCount, setPendingQueueCount] = useState(0);
  const [runInputMode, setRunInputMode] = useState<RunInputMode>("steering");
  const [model, setModel] = useState("");
  const [readOnly, setReadOnly] = useState(false);
  const [colorScheme, setColorScheme] = useState<ColorSchemeMode>(readSavedColorScheme);
  const [fontSize, setFontSize] = useState(readSavedFontSize);
  const [page, setPage] = useState<Page>("home");
  const [panel, setPanel] = useState<Panel>("closed");
  const [expandedDiff, setExpandedDiff] = useState(false);
  const [changes, setChanges] = useState<ChangeSet | null>(null);
  const [selectedFile, setSelectedFile] = useState("");
  const [diff, setDiff] = useState("");
  const [evidence, setEvidence] = useState<EvidenceView | null>(null);
  const [timelineKind, setTimelineKind] = useState<TimelineKind>("all");
  const [settingsTab, setSettingsTab] = useState<SettingsCategory>("general");
  const [settingsQuery, setSettingsQuery] = useState("");
  const [capabilityCounts, setCapabilityCounts] = useState<CapabilityCounts | null>(null);
  const [showArchivedProjects, setShowArchivedProjects] = useState(false);
  const [archivedProjectsList, setArchivedProjectsList] = useState<RecentProject[] | null>(null);
  const [healthInfo, setHealthInfo] = useState<HealthResult | null>(null);
  const [doctorRunning, setDoctorRunning] = useState(false);
  const [doctorReport, setDoctorReport] = useState<unknown>(null);
  const [doctorError, setDoctorError] = useState("");
  const [themePill, setThemePill] = useState<{ left: number; width: number } | null>(null);
  const [stepperBump, setStepperBump] = useState<"min" | "max" | null>(null);
  const [notice, setNotice] = useState("");
  const [filter, setFilter] = useState("");
  const [showArchived, setShowArchived] = useState(false);
  const [needsAttentionOnly, setNeedsAttentionOnly] = useState(false);
  const [searchOpen, setSearchOpen] = useState(true);
  const [heroPopover, setHeroPopover] = useState<HeroPopover>("none");
  const [editorMenuOpen, setEditorMenuOpen] = useState(false);
  const [activityTab, setActivityTab] = useState<ActivityTab>("heatmap");
  const [providerPresets, setProviderPresets] = useState<ProviderPresetView[]>([]);
  const [openLocations, setOpenLocations] = useState<OpenLocationItem[]>([]);
  const [forkSource, setForkSource] = useState<string | null>(null);
  const [parentRun, setParentRun] = useState<string | null>(null);
  const [sessionTree, setSessionTree] = useState<SessionTreeView | null>(null);
  const [sidebarOpen, setSidebarOpen] = useState(
    () => typeof window === "undefined" || window.innerWidth > 760,
  );
  // 世代以状态镜像：重选同一工作区时路径不变，轮询与流订阅仍须随世代重建，否则旧闭包守卫会永久丢弃新投影。
  const [epoch, setEpoch] = useState(0);
  const projectRef = useRef("");
  const generationRef = useRef(0);
  const runRef = useRef<string | null>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const sessionSearchRef = useRef<HTMLInputElement>(null);
  const heroSentenceRef = useRef<HTMLDivElement>(null);
  const editorActionRef = useRef<HTMLDivElement>(null);
  const diffRequestRef = useRef(0);
  const themeBtnRefs = useRef<Record<ColorSchemeMode, HTMLButtonElement | null>>({
    system: null,
    light: null,
    dark: null,
  });
  const stepperBumpTimerRef = useRef<number | null>(null);

  useEffect(() => {
    return () => {
      if (stepperBumpTimerRef.current !== null) {
        window.clearTimeout(stepperBumpTimerRef.current);
      }
    };
  }, []);

  useLayoutEffect(() => {
    if (page !== "settings" || settingsTab !== "appearance") return;
    const syncPill = () => {
      const target = themeBtnRefs.current[colorScheme];
      if (target && target.offsetWidth > 0) {
        setThemePill({ left: target.offsetLeft, width: target.offsetWidth });
      } else {
        setThemePill(null);
      }
    };
    syncPill();
    window.addEventListener("resize", syncPill);
    return () => {
      window.removeEventListener("resize", syncPill);
    };
  }, [page, settingsTab, colorScheme, fontSize]);

  const triggerStepperBump = (direction: "min" | "max") => {
    if (stepperBumpTimerRef.current !== null) {
      window.clearTimeout(stepperBumpTimerRef.current);
    }
    setStepperBump(direction);
    stepperBumpTimerRef.current = window.setTimeout(() => {
      setStepperBump(null);
      stepperBumpTimerRef.current = null;
    }, 300);
  };

  const stepFontSize = (delta: -1 | 1) => {
    if (delta < 0) {
      if (fontSize <= 12) {
        triggerStepperBump("min");
        return;
      }
      setStepperBump(null);
      setFontSize((current) => Math.max(12, current - 1));
      return;
    }
    if (fontSize >= 19) {
      triggerStepperBump("max");
      return;
    }
    setStepperBump(null);
    setFontSize((current) => Math.min(19, current + 1));
  };

  const closeSmallNav = () => {
    if (typeof window !== "undefined" && window.innerWidth <= 760) setSidebarOpen(false);
  };

  useEffect(() => {
    const onResize = () => {
      if (window.innerWidth <= 760) setSidebarOpen(false);
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.isComposing || event.keyCode === 229) return;
      if ((event.metaKey || event.ctrlKey) && event.key === ",") {
        event.preventDefault();
        setPage((current) => {
          if (current === "settings") {
            return "home";
          }
          setPanel("closed");
          if (window.innerWidth <= 760) setSidebarOpen(false);
          return "settings";
        });
        return;
      }
      if (event.key === "Escape") {
        setSidebarOpen(false);
        setHeroPopover("none");
        setEditorMenuOpen(false);
        setPage((current) => (current === "settings" ? "home" : current));
      }
    };
    window.addEventListener("resize", onResize);
    window.addEventListener("keydown", onKeyDown);
    return () => {
      window.removeEventListener("resize", onResize);
      window.removeEventListener("keydown", onKeyDown);
    };
  }, []);

  useEffect(() => {
    if (heroPopover === "none" && !editorMenuOpen) return;
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target as Node;
      if (heroPopover !== "none" && heroSentenceRef.current && !heroSentenceRef.current.contains(target)) {
        setHeroPopover("none");
      }
      if (editorMenuOpen && editorActionRef.current && !editorActionRef.current.contains(target)) {
        setEditorMenuOpen(false);
      }
    };
    window.addEventListener("mousedown", onPointerDown);
    return () => {
      window.removeEventListener("mousedown", onPointerDown);
    };
  }, [heroPopover, editorMenuOpen]);

  useEffect(() => {
    if (typeof document === "undefined") return;
    let meta = document.querySelector('meta[name="color-scheme"]') as HTMLMetaElement | null;
    if (!meta) {
      meta = document.createElement("meta");
      meta.name = "color-scheme";
      document.head.appendChild(meta);
    }
    meta.content = colorScheme === "system" ? "light dark" : colorScheme;
    try {
      localStorage.setItem("cairn.gui.colorScheme", colorScheme);
    } catch {
      // 忽略本地存储写入受限场景。
    }
  }, [colorScheme]);

  const toggleQuickColorScheme = () => {
    setColorScheme((current) => {
      if (current === "dark") return "light";
      if (current === "light") return "dark";
      const systemDark =
        typeof window !== "undefined" &&
        typeof window.matchMedia === "function" &&
        window.matchMedia("(prefers-color-scheme: dark)").matches;
      return systemDark ? "light" : "dark";
    });
  };

  // 始终包含归档项：选中的归档会话仍须由 Journal 状态判定 running，归档视图也依赖同一数据源。
  const refreshRuns = useCallback(async (path: string, generation: number) => {
    const data = await desktop.listRuns({ workspace: path, include_archived: true });
    if (generation === generationRef.current && projectRef.current === path) setRuns(data);
    return data;
  }, []);

  const refreshQueueCount = useCallback((path: string, generation: number) => {
    const req = desktop.listQueue?.({ workspace: path });
    if (!req) return;
    void Promise.resolve(req)
      .then((items) => {
        if (generation !== generationRef.current || projectRef.current !== path) return;
        if (Array.isArray(items)) {
          const count = items.filter(
            (item) => item.state === "pending" || item.state === "submitted",
          ).length;
          setPendingQueueCount(count);
        }
      })
      .catch(() => {
        // 队列徽标计数失败不干扰主会话通知。
      });
  }, []);

  const refreshPresets = useCallback((path: string, generation: number) => {
    if (!path || typeof desktop.listProviderPresets !== "function") {
      setProviderPresets([]);
      return;
    }
    void Promise.resolve(desktop.listProviderPresets({ workspace: path }))
      .then((items) => {
        if (generation !== generationRef.current || projectRef.current !== path) return;
        setProviderPresets(Array.isArray(items) ? items : []);
      })
      .catch(() => {
        if (generation === generationRef.current && projectRef.current === path) {
          setProviderPresets([]);
        }
      });
  }, []);

  const refreshOpenLocations = useCallback((path: string, generation: number) => {
    if (!path || typeof desktop.listOpenLocations !== "function") {
      setOpenLocations([]);
      return;
    }
    void Promise.resolve(desktop.listOpenLocations(path))
      .then((items) => {
        if (generation !== generationRef.current || projectRef.current !== path) return;
        setOpenLocations(
          Array.isArray(items) ? items.filter((loc) => Boolean(loc && loc.available)) : [],
        );
      })
      .catch(() => {
        if (generation === generationRef.current && projectRef.current === path) {
          setOpenLocations([]);
        }
      });
  }, []);

  const switchProject = useCallback(
    async (path: string, options?: { preservePage?: boolean }) => {
      if (!path) return;
      const generation = ++generationRef.current;
      setEpoch(generation);
      projectRef.current = path;
      runRef.current = null;
      setWorkspace(path);
      setDraft("");
      setModel("");
      setReadOnly(false);
      setRunId(null);
      setDetail(null);
      setLiveEvents([]);
      setSending(false);
      setSendingText("");
      setPendingQueueCount(0);
      setWorkspaceStatus(null);
      setBridgeStatus("connecting");
      setRuns([]);
      setProviderPresets([]);
      setOpenLocations([]);
      setHeroPopover("none");
      setEditorMenuOpen(false);
      setForkSource(null);
      setParentRun(null);
      setSessionTree(null);
      setChanges(null);
      setSelectedFile("");
      setDiff("");
      setEvidence(null);
      setTimelineKind("all");
      setCapabilityCounts(null);
      setDoctorRunning(false);
      setDoctorReport(null);
      setDoctorError("");
      diffRequestRef.current += 1;
      if (!options?.preservePage) {
        setPage("home");
      }
      setPanel("closed");
      closeSmallNav();
      setNotice("");
      try {
        const [ws, history, health] = await Promise.all([
          desktop.observeWorkspace(path),
          desktop.listRuns({ workspace: path, include_archived: true }),
          desktop.health().catch(() => null),
        ]);
        if (generation !== generationRef.current) return;
        setWorkspaceStatus(ws);
        if (health) {
          setHealthInfo(health);
        }
        setBridgeStatus(health?.ok ? "healthy" : "disconnected");
        setRuns(history);
        if (typeof desktop.sessionTree === "function") {
          const treeCall = desktop.sessionTree({ workspace: path });
          if (treeCall) {
            void Promise.resolve(treeCall)
              .then((tree) => {
                if (generation === generationRef.current) setSessionTree(tree);
              })
              .catch(() => {
                if (generation === generationRef.current) setSessionTree(null);
              });
          }
        }
        refreshQueueCount(path, generation);
        refreshPresets(path, generation);
        refreshOpenLocations(path, generation);
        if (typeof desktop.rememberProject === "function") {
          void desktop.rememberProject(path);
        }
      } catch (err) {
        if (generation === generationRef.current) setNotice(textError(err));
      }
    },
    [refreshQueueCount, refreshPresets, refreshOpenLocations],
  );

  useEffect(() => {
    document.documentElement.style.setProperty("--wb-font-size-base", fontSize + "px");
    try {
      localStorage.setItem("cairn.gui.fontSize", String(fontSize));
    } catch {
      // 忽略本地存储写入受限场景。
    }
  }, [fontSize]);

  useEffect(() => {
    let alive = true;
    void desktop
      .listRecentProjects(false)
      .then((items) => {
        if (!alive) return;
        setRecent(items);
        if (items.length && !projectRef.current) void switchProject(items[0].path);
      })
      .catch((err) => {
        if (alive) setNotice(textError(err));
      });
    return () => {
      alive = false;
    };
  }, [switchProject]);

  useEffect(() => {
    if (!workspace) return;
    refreshQueueCount(workspace, generationRef.current);
  }, [workspace, queueRevision, refreshQueueCount]);

  const selectRun = useCallback((id: string | null) => {
    setForkSource(null);
    setParentRun(null);
    runRef.current = id;
    setRunId(id);
    setDetail(null);
    setEvidence(null);
    setTimelineKind("all");
    if (id && projectRef.current) {
      const currentWorkspace = projectRef.current;
      const generation = generationRef.current;
      void desktop
        .inspectRun({ workspace: currentWorkspace, run_id: id, include_events: true })
        .then((result) => {
          if (generation === generationRef.current && runRef.current === id) setDetail(result);
        })
        .catch((error) => {
          if (generation === generationRef.current && runRef.current === id) {
            setNotice(textError(error));
          }
        });
    }
    setLiveEvents([]);
    setSendingText("");
    setPanel("closed");
    setPage("home");
    if (typeof window !== "undefined" && window.innerWidth <= 760) setSidebarOpen(false);
  }, []);

  // HTTP bridge 也需要可用：按照现有 Journal 事实定期同步，不把 ACK 当成完成。
  useEffect(() => {
    if (!workspace) return;
    let cancelled = false;
    const generation = generationRef.current;
    const poll = async () => {
      try {
        const history = await refreshRuns(workspace, generation);
        const id = runRef.current;
        if (!id || !history.some((item) => item.run_id === id)) return;
        const data = await desktop.inspectRun({ workspace, run_id: id, include_events: true });
        if (!cancelled && generationRef.current === generation && runRef.current === id) {
          setDetail(data);
        }
      } catch (err) {
        // 已持久化运行的读取失败不可静默吞掉；特别是 Journal 损坏必须可见。
        if (!cancelled && generationRef.current === generation) setNotice(textError(err));
      }
    };
    void poll();
    const timer = window.setInterval(() => {
      void poll();
    }, 2000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [workspace, epoch, refreshRuns]);

  useEffect(() => {
    if (!workspace) return;
    const generation = generationRef.current;
    const unbind = desktop.onStream((evt) => {
      if (generationRef.current !== generation || projectRef.current !== workspace) return;
      if (evt.workspace && evt.workspace !== workspace) return;
      if (evt.run_id && evt.run_id === runRef.current) {
        setLiveEvents((previous) => {
          const key = evt.event?.event_id;
          if (key && previous.some((item) => item.event?.event_id === key)) return previous;
          return previous.concat(evt).slice(-500);
        });
      }
      if (evt.kind === "run.event" || evt.kind === "run.status") void refreshRuns(workspace, generation);
    });
    return unbind;
  }, [workspace, epoch, refreshRuns]);

  useEffect(() => {
    if (panel !== "evidence" || !workspace || !runId) return;
    const currentWorkspace = workspace;
    const currentRunId = runId;
    const evCall = desktop.evidence?.({ workspace: currentWorkspace, run_id: currentRunId });
    if (!evCall) return;
    void Promise.resolve(evCall)
      .then((result) => {
        if (projectRef.current === currentWorkspace && runRef.current === currentRunId) {
          setEvidence(result);
        }
      })
      .catch((err) => {
        if (projectRef.current === currentWorkspace && runRef.current === currentRunId) {
          setNotice(textError(err));
        }
      });
  }, [panel, workspace, runId, detail?.state]);

  const selected = runs.find((run) => run.run_id === runId);
  const running = selected
    ? !finished.has(selected.state) && !selected.waiting_approval && !selected.waiting_reconcile
    : sending || Boolean(runId);

  const subagents = useMemo(() => toSubagentViews(detail?.events || []), [detail?.events]);
  const compactions = useMemo(() => toContextCompactions(detail?.events || []), [detail?.events]);
  const contextUsage = useMemo(() => toContextUsage(detail?.events || []), [detail?.events]);
  const timelineItems = useMemo(() => toTimeline(detail?.events || []), [detail?.events]);
  const filteredTimeline = useMemo(
    () => filterTimelineByKind(timelineItems, timelineKind),
    [timelineItems, timelineKind],
  );
  const eventHashBySeq = useMemo(() => {
    const map = new Map<number, string>();
    for (const ev of detail?.events || []) {
      if (ev.event_hash) {
        map.set(ev.seq, ev.event_hash);
      }
    }
    return map;
  }, [detail?.events]);
  const treeMeta = useMemo(() => computeSessionTreeMeta(sessionTree), [sessionTree]);

  const onQueueError = useCallback((message: string) => setNotice(message), []);
  const onRunFromQueue = useCallback((id: string) => {
    if (id) {
      runRef.current = id;
      setRunId(id);
      setDetail(null);
      setEvidence(null);
      setLiveEvents([]);
      setPanel("closed");
      setPage("home");
    }
  }, []);

  const blocks = useMemo(() => {
    const projected = buildChatBlocks(detail?.events || [], liveEvents);
    if (sendingText && !projected.some((block) => block.kind === "user" && block.text === sendingText)) {
      projected.unshift({ kind: "user", id: "pending-user", text: sendingText, at: "" });
    }
    return projected;
  }, [detail, liveEvents, sendingText]);

  useEffect(() => {
    if (typeof bottomRef.current?.scrollIntoView === "function") {
      bottomRef.current.scrollIntoView({ block: "end", behavior: "auto" });
    }
  }, [blocks.length, runId]);

  const startTask = async () => {
    const content = draft.trim();
    if (
      !workspace ||
      !content ||
      sending ||
      running ||
      selected?.waiting_approval ||
      selected?.waiting_reconcile
    ) {
      return;
    }
    const generation = generationRef.current;
    const previous = !forkSource && selected && finished.has(selected.state) ? selected : null;
    setSending(true);
    setSendingText(content);
    setDraft("");
    try {
      const ack = await desktop.startRun({
        workspace,
        task: content,
        model: model.trim() || undefined,
        mode: readOnly ? "read-only" : "default",
        session_id: previous?.session_id,
        prev_run_id: previous?.run_id,
        require_session_tail: Boolean(previous),
        fork_source_run_id: forkSource || undefined,
      });
      if (generation !== generationRef.current) return;
      runRef.current = ack.run_id;
      setRunId(ack.run_id);
      setForkSource(null);
      setParentRun(null);
      setDetail(null);
      setEvidence(null);
      setLiveEvents([]);
      setPage("home");
      await refreshRuns(workspace, generation);
    } catch (err) {
      if (generation === generationRef.current) {
        setDraft((value) => (value ? content + "\n" + value : content));
        setNotice(textError(err));
        setSendingText("");
      }
    } finally {
      if (generation === generationRef.current) setSending(false);
    }
  };

  const submitQueued = async () => {
    const text = draft.trim();
    if (!workspace || !text || sending) return;
    const generation = generationRef.current;
    setSending(true);
    setDraft("");
    try {
      const ack = await desktop.submitQueue({ workspace, content: text });
      if (generation !== generationRef.current) return;
      setQueueRevision((n) => n + 1);
      setPanel("queue");
      setNotice(`已提交队列 ${ack.input_id}，仍需人工确认才会启动新运行。`);
    } catch (e) {
      if (generation === generationRef.current) {
        setDraft((current) => (current ? text + "\n" + current : text));
        setNotice(textError(e));
      }
    } finally {
      if (generation === generationRef.current) setSending(false);
    }
  };

  const submitSteeringFromDraft = async () => {
    const text = draft.trim();
    if (!text || !runId || !running || sending) return;
    const generation = generationRef.current;
    setSending(true);
    setDraft("");
    try {
      await desktop.submitSteering({ run_id: runId, content: text });
      if (generation !== generationRef.current) return;
      setNotice("补充指令已提交；由 Agent 在安全点消费。");
    } catch (e) {
      if (generation === generationRef.current) {
        setDraft((current) => (current ? text + "\n" + current : text));
        setNotice(textError(e));
      }
    } finally {
      if (generation === generationRef.current) setSending(false);
    }
  };

  const decide = async (approve: boolean, fingerprint: string, requestedRunId: string) => {
    try {
      await desktop.resumeRun({
        workspace,
        run_id: requestedRunId,
        fingerprint,
        approve,
        reason: approve ? "studio gui user approval" : "studio gui user rejection",
      });
      setNotice("审批决定已提交；等待 Journal 更新。");
    } catch (err) {
      setNotice(textError(err));
    }
  };

  const openPanel = async (next: Panel) => {
    const willOpen = panel !== next;
    setPanel(willOpen ? next : "closed");
    setExpandedDiff(false);
    if (!workspace || !willOpen || next === "closed") return;
    const request = ++diffRequestRef.current;
    try {
      if (next === "changes" || next === "workbench") {
        const result = await desktop.listChanges(workspace);
        if (projectRef.current !== workspace || request !== diffRequestRef.current) return;
        setChanges(result);
        const first = result.entries[0]?.relative || "";
        setSelectedFile(first);
        const resultDiff = first
          ? await reviewFileText(workspace, first, result.entries[0]?.status || "")
          : "";
        if (projectRef.current === workspace && request === diffRequestRef.current) {
          setDiff(resultDiff);
        }
      }
      if (next === "evidence" && runId) {
        const result = await desktop.evidence({ workspace, run_id: runId });
        if (projectRef.current === workspace && runRef.current === runId) setEvidence(result);
      }
    } catch (err) {
      setNotice(textError(err));
    }
  };

  const inspectChangedFile = async (targetPath: string) => {
    if (!workspace) return;
    setPanel("changes");
    setExpandedDiff(false);
    const request = ++diffRequestRef.current;
    try {
      const result = await desktop.listChanges(workspace);
      if (projectRef.current !== workspace || request !== diffRequestRef.current) return;
      setChanges(result);
      const matched = matchChangedRelativePath(result.entries, targetPath);
      setSelectedFile(matched);
      const entryStatus = result.entries.find((e) => e.relative === matched)?.status || "";
      const resultDiff = matched ? await reviewFileText(workspace, matched, entryStatus) : "";
      if (projectRef.current === workspace && request === diffRequestRef.current) {
        setDiff(resultDiff);
      }
    } catch (err) {
      setNotice(textError(err));
    }
  };

  const chooseFile = async (relative: string) => {
    const request = ++diffRequestRef.current;
    setSelectedFile(relative);
    setDiff("");
    try {
      const result = await reviewFileText(
        workspace,
        relative,
        changes?.entries.find((e) => e.relative === relative)?.status || "",
      );
      if (projectRef.current === workspace && request === diffRequestRef.current) setDiff(result);
    } catch (err) {
      setNotice(textError(err));
    }
  };

  const pickProject = async (preservePage = false) => {
    try {
      const path = await desktop.pickDirectory("选择 Cairn 工作区", workspace);
      if (path) {
        await switchProject(path, { preservePage });
        const items = await desktop.listRecentProjects(false);
        setRecent(items);
        if (showArchivedProjects) {
          const allItems = await desktop.listRecentProjects(true);
          setArchivedProjectsList(allItems);
        }
      }
    } catch (err) {
      setNotice(textError(err));
    }
  };

  const toggleArchiveProject = async (project: RecentProject) => {
    try {
      await desktop.archiveProject(project.path, !project.archived);
      const activeItems = await desktop.listRecentProjects(false);
      setRecent(activeItems);
      if (showArchivedProjects) {
        const allItems = await desktop.listRecentProjects(true);
        setArchivedProjectsList(allItems);
      } else {
        setArchivedProjectsList(null);
      }
    } catch (err) {
      setNotice(textError(err));
    }
  };

  const runWorkspaceDoctor = async () => {
    if (!workspace || doctorRunning) return;
    const targetWorkspace = workspace;
    const generation = generationRef.current;
    setDoctorRunning(true);
    setDoctorError("");
    try {
      const report = await desktop.doctor({ workspace: targetWorkspace });
      if (generation === generationRef.current && projectRef.current === targetWorkspace) {
        setDoctorReport(report);
      }
    } catch (err) {
      if (generation === generationRef.current && projectRef.current === targetWorkspace) {
        const message = textError(err);
        setDoctorError(message);
        setNotice(message);
      }
    } finally {
      if (generation === generationRef.current && projectRef.current === targetWorkspace) {
        setDoctorRunning(false);
      }
    }
  };

  useEffect(() => {
    if (page !== "settings" || !showArchivedProjects || !desktop.listRecentProjects) return;
    let alive = true;
    void desktop
      .listRecentProjects(true)
      .then((items) => {
        if (alive) setArchivedProjectsList(items);
      })
      .catch((err) => {
        if (alive) setNotice(textError(err));
      });
    return () => {
      alive = false;
    };
  }, [page, showArchivedProjects]);

  useEffect(() => {
    if (page !== "settings" || settingsTab !== "about" || !desktop.health) return;
    let alive = true;
    void desktop
      .health()
      .then((result) => {
        if (!alive) return;
        setHealthInfo(result);
        setBridgeStatus(result?.ok ? "healthy" : "disconnected");
      })
      .catch(() => {
        if (alive) setBridgeStatus("disconnected");
      });
    return () => {
      alive = false;
    };
  }, [page, settingsTab]);

  useEffect(() => {
    if (
      page !== "settings" ||
      !workspace ||
      capabilityCounts !== null ||
      isCapabilitySection(settingsTab) ||
      !desktop.listSkills ||
      !desktop.listHooks ||
      !desktop.listMemories ||
      !desktop.listCommands ||
      !desktop.listProjectInstructions ||
      !desktop.mcpStatus
    ) {
      return;
    }
    let alive = true;
    const targetWorkspace = workspace;
    void Promise.allSettled([
      desktop.listSkills({ workspace: targetWorkspace }),
      desktop.listHooks(targetWorkspace),
      desktop.listMemories({ workspace: targetWorkspace }),
      desktop.listCommands(),
      desktop.listProjectInstructions(targetWorkspace),
      desktop.mcpStatus({ workspace: targetWorkspace }),
    ]).then((results) => {
      if (!alive || projectRef.current !== targetWorkspace) return;
      setCapabilityCounts({
        skills:
          results[0].status === "fulfilled" && Array.isArray(results[0].value)
            ? results[0].value.length
            : 0,
        hooks:
          results[1].status === "fulfilled" && Array.isArray(results[1].value)
            ? results[1].value.length
            : 0,
        memory:
          results[2].status === "fulfilled" && Array.isArray(results[2].value)
            ? results[2].value.length
            : 0,
        commands:
          results[3].status === "fulfilled" && Array.isArray(results[3].value)
            ? results[3].value.length
            : 0,
        instructions:
          results[4].status === "fulfilled" && Array.isArray(results[4].value?.documents)
            ? results[4].value.documents.length
            : 0,
        mcp:
          results[5].status === "fulfilled" && Array.isArray(results[5].value)
            ? results[5].value.length
            : null,
      });
    });
    return () => {
      alive = false;
    };
  }, [page, workspace, capabilityCounts, settingsTab]);

  const openSettings = () => {
    setPage("settings");
    setPanel("closed");
    closeSmallNav();
  };

  const openWorkflow = () => {
    setPage("workflow");
    setPanel("closed");
    closeSmallNav();
  };

  const updateSession = async (
    run: RunHistoryView,
    title?: string,
    flags?: { pinned: boolean; archived: boolean },
  ) => {
    if (!run.session_id) throw new Error("运行缺少会话 ID，不能修改会话元数据。");
    const generation = generationRef.current;
    try {
      if (title !== undefined) await desktop.renameSession(run.session_id, title);
      if (flags) await desktop.setSessionFlags(run.session_id, flags.pinned, flags.archived);
      await refreshRuns(workspace, generation);
    } catch (e) {
      setNotice(textError(e));
      throw e;
    }
  };

  const forkRun = (id: string) => {
    selectRun(null);
    setForkSource(id);
    setDraft("");
    setNotice("新会话将从选定 Run 分支创建，而不是续接原会话。");
  };

  const openChild = (childId: string) => {
    if (!runId) return;
    const currentParent = runId;
    selectRun(childId);
    setParentRun(currentParent);
  };

  const stickyGate =
    runId && selected && (selected.waiting_approval || selected.waiting_reconcile) ? (
      <div className="wb-sticky-gate" role="status">
        <span>
          {selected.waiting_reconcile
            ? "⚠ 当前运行存在未决副作用（waiting_reconcile），请在上方完成人工对账或终止运行，禁止盲目重试。"
            : "⚡ 当前运行处于授权门禁停点，请核对上方审批卡片中的操作指纹后决定。"}
        </span>
        {selected.waiting_reconcile && (
          <button
            type="button"
            className="wb-secondary"
            onClick={() => {
              if (panel !== "changes") void openPanel("changes");
            }}
          >
            核实工作区变更
          </button>
        )}
      </div>
    ) : null;

  const workspaceName = workspace.split("/").filter(Boolean).pop() || "选择工作区";
  const activePreset = useMemo(
    () => providerPresets.find((item) => item.active) ?? null,
    [providerPresets],
  );
  const currentModelOrPresetLabel =
    model.trim() ||
    (activePreset
      ? `${activePreset.name}${activePreset.model ? ` · ${activePreset.model}` : ""}`
      : "默认模型");
  const attentionCount = useMemo(
    () =>
      runs.filter((run) => !run.archived && (run.waiting_approval || run.waiting_reconcile)).length,
    [runs],
  );
  const visibleRuns = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return runs
      .filter((run) => {
        if (needsAttentionOnly) {
          if (run.archived || (!run.waiting_approval && !run.waiting_reconcile)) return false;
        } else if (run.archived !== showArchived) {
          return false;
        }
        if (!q) return true;
        return (run.title + " " + run.task).toLowerCase().includes(q);
      })
      .slice(0, 50);
  }, [runs, needsAttentionOnly, showArchived, filter]);
  const activitySummary = useMemo(() => buildLocalActivitySummary(runs), [runs]);
  const contextMeterText =
    runId && contextUsage.compactionCount > 0
      ? `上下文：${contextUsage.estimatedUnitsAfter > 0 ? `${contextUsage.estimatedUnitsAfter.toLocaleString()} units · ` : ""}已压缩 ${contextUsage.compactionCount} 次`
      : readOnly
        ? "只读会话 · 禁止写入与命令副作用"
        : "写入与命令默认 ask";

  const canOpenInEditor =
    Boolean(workspace) &&
    (openLocations.length > 0 || typeof desktop.openPathInEditor === "function");

  const handleOpenWorkspaceInEditor = async (locationId?: string) => {
    if (!workspace) return;
    setEditorMenuOpen(false);
    try {
      if (locationId && typeof desktop.openInLocation === "function") {
        await desktop.openInLocation(workspace, ".", locationId);
        return;
      }
      if (openLocations.length === 1 && typeof desktop.openInLocation === "function") {
        await desktop.openInLocation(workspace, ".", openLocations[0].id);
        return;
      }
      if (typeof desktop.openPathInEditor === "function") {
        await desktop.openPathInEditor(workspace, ".");
      }
    } catch (err) {
      setNotice(textError(err));
    }
  };

  const handleActivatePresetFromHero = async (preset: ProviderPresetView) => {
    setHeroPopover("none");
    if (!workspace) return;
    if (typeof desktop.useProviderPreset === "function") {
      try {
        const targetWorkspace = workspace;
        const generation = generationRef.current;
        await desktop.useProviderPreset(targetWorkspace, preset.name);
        setModel("");
        refreshPresets(targetWorkspace, generation);
        const ws = await desktop.observeWorkspace(targetWorkspace);
        if (generation === generationRef.current && projectRef.current === targetWorkspace) {
          setWorkspaceStatus(ws);
        }
        return;
      } catch (err) {
        setNotice(textError(err));
        return;
      }
    }
    if (preset.model) {
      setModel(preset.model);
    }
  };

  const compose = (
    <div style={{ width: "100%", maxWidth: 780, margin: "0 auto" }}>
      <div className="wb-composer">
        {stickyGate}
        {forkSource && (
          <div className="wb-fork-source">
            从 Run {forkSource} 创建新分支{" "}
            <button type="button" onClick={() => setForkSource(null)} aria-label="取消分支">
              ×
            </button>
          </div>
        )}
        <textarea
          aria-label="描述任务"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
          placeholder={
            running && runId
              ? runInputMode === "steering"
                ? "输入补充指导，Agent 将在下一安全点消费…（Ctrl/Cmd+Enter 加入候选队列）"
                : "输入后续任务候选，提交后需人工确认启动…"
              : "描述一个任务、问题或你希望实现的功能…"
          }
          onKeyDown={(event) => {
            const ime = event.nativeEvent.isComposing || event.nativeEvent.keyCode === 229;
            if (ime) return;
            if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
              event.preventDefault();
              void submitQueued();
              return;
            }
            if (event.key === "Enter" && !event.shiftKey) {
              event.preventDefault();
              if (running && runId) {
                if (runInputMode === "queue") {
                  void submitQueued();
                } else {
                  void submitSteeringFromDraft();
                }
                return;
              }
              void startTask();
            }
          }}
        />
        <div className="wb-composer-toolbar">
          <div>
            <button
              type="button"
              title="切换工作区目录"
              aria-label="切换工作区目录"
              onClick={() => void pickProject()}
            >
              ＋
            </button>
            <label className={"wb-perm-pill" + (readOnly ? " is-readonly" : "")}>
              <input
                type="checkbox"
                className="wb-sr-only"
                aria-label="只读"
                checked={readOnly}
                onChange={(e) => setReadOnly(e.target.checked)}
              />
              {readOnly ? "🔒 只读探索" : "🛡 默认审批 ask"}
            </label>
            {running && runId && (
              <div className="wb-mode-switch" role="group" aria-label="运行中投递模式">
                <button
                  type="button"
                  className={runInputMode === "steering" ? "current" : ""}
                  onClick={() => setRunInputMode("steering")}
                >
                  ⚡ 安全点补充
                </button>
                <button
                  type="button"
                  className={runInputMode === "queue" ? "current" : ""}
                  onClick={() => setRunInputMode("queue")}
                >
                  📥 候选队列
                </button>
              </div>
            )}
          </div>
          <div>
            <label className="wb-model-capsule">
              <span aria-hidden="true">◈</span>
              <input
                aria-label="模型覆盖"
                placeholder={activePreset?.model || "默认模型"}
                value={model}
                onChange={(event) => setModel(event.target.value)}
              />
            </label>
            {running && runId && runInputMode === "steering" && (
              <button
                type="button"
                className="wb-secondary wb-queue-draft"
                style={{ marginTop: 0, padding: "5px 10px" }}
                disabled={!draft.trim() || sending}
                onClick={() => void submitSteeringFromDraft()}
              >
                提交 Steering
              </button>
            )}
            {running && runId && (
              <button
                type="button"
                className="wb-secondary wb-queue-draft"
                style={{ marginTop: 0, padding: "5px 10px" }}
                disabled={!draft.trim() || sending}
                onClick={() => void submitQueued()}
              >
                加入队列
              </button>
            )}
            {running && runId ? (
              <button
                type="button"
                className="wb-stop"
                onClick={() =>
                  void desktop
                    .cancelRun({ run_id: runId, reason: "user stop" })
                    .catch((err) => setNotice(textError(err)))
                }
              >
                停止
              </button>
            ) : (
              <button
                type="button"
                className="wb-send"
                title="提交任务"
                aria-label="提交任务"
                disabled={
                  !workspace ||
                  !draft.trim() ||
                  sending ||
                  selected?.waiting_approval ||
                  selected?.waiting_reconcile
                }
                onClick={() => void startTask()}
              >
                ↑
              </button>
            )}
          </div>
        </div>
      </div>
      <div className="wb-env-strip">
        <div>
          <span>📁 {workspaceName}</span>
          {workspaceStatus?.branch && (
            <>
              <span aria-hidden="true">·</span>
              <span>⎇ {workspaceStatus.branch}</span>
            </>
          )}
          <span aria-hidden="true">·</span>
          <span>{workspaceStatus?.dirty ? "工作区有未提交修改" : "工作区干净"}</span>
        </div>
        <span>{contextMeterText}</span>
      </div>
    </div>
  );

  const closeToSession = () => {
    setPage("home");
    if (workspace) {
      refreshPresets(workspace, generationRef.current);
    }
  };

  const filteredSettingsItems = useMemo(
    () => filterSettingsNavItems(SETTINGS_NAV_ITEMS, settingsQuery),
    [settingsQuery],
  );
  const activeSettingsMeta = useMemo(
    () => SETTINGS_NAV_ITEMS.find((item) => item.id === settingsTab) ?? SETTINGS_NAV_ITEMS[0],
    [settingsTab],
  );
  const displayedRecentProjects = showArchivedProjects ? archivedProjectsList ?? recent : recent;
  const doctorSummary = useMemo(() => extractDoctorSummary(doctorReport), [doctorReport]);
  const bridgeStatusText =
    bridgeStatus === "healthy"
      ? "桥接已连接"
      : bridgeStatus === "connecting"
        ? "连接中"
        : "桥接未连接";
  const bridgeDotTone =
    bridgeStatus === "healthy" ? "is-ok" : bridgeStatus === "connecting" ? "is-muted" : "is-warn";

  const settingsShell = (
    <div className="wb-settings-shell">
      <nav className="wb-settings-rail" aria-label="设置分类">
        <div className="wb-settings-rail-top">
          <button type="button" className="wb-settings-back" onClick={closeToSession}>
            ← 返回工作区
          </button>
          <input
            className="wb-settings-search"
            aria-label="搜索设置"
            placeholder="搜索设置…"
            value={settingsQuery}
            onChange={(event) => setSettingsQuery(event.target.value)}
          />
        </div>
        <div className="wb-settings-rail-scroll">
          {filteredSettingsItems.length > 0 ? (
            SETTINGS_GROUPS.map((group) => {
              const groupItems = filteredSettingsItems.filter((item) => item.group === group.key);
              if (!groupItems.length) return null;
              return (
                <div className="wb-settings-nav-group" key={group.key}>
                  <div className="wb-settings-nav-label">{group.label}</div>
                  {groupItems.map((item) => {
                    const count = readCapabilityCount(capabilityCounts, item.capabilityKey);
                    const isActive = settingsTab === item.id;
                    return (
                      <button
                        type="button"
                        key={item.id}
                        className={"wb-settings-nav-item" + (isActive ? " current" : "")}
                        aria-current={isActive ? "page" : undefined}
                        onClick={() => setSettingsTab(item.id)}
                      >
                        <span className="wb-settings-nav-icon" aria-hidden="true">
                          {item.icon}
                        </span>
                        <span>{item.label}</span>
                        {count !== null && <span className="wb-settings-nav-count">{count}</span>}
                      </button>
                    );
                  })}
                </div>
              );
            })
          ) : (
            <>
              <div className="wb-hint" role="status">
                未找到与“{settingsQuery.trim()}”匹配的设置项。
              </div>
              <button type="button" className="wb-secondary" onClick={() => setSettingsQuery("")}>
                清空搜索
              </button>
            </>
          )}
        </div>
        <div className="wb-settings-rail-foot">
          <strong>Cairn Studio</strong>
          <span>
            <i className={"wb-status-dot " + bridgeDotTone} aria-hidden="true" /> {bridgeStatusText}
          </span>
        </div>
      </nav>
      <main className="wb-settings-stage">
        <div
          key={settingsTab}
          className={"wb-settings-canvas" + (settingsTab === "models" ? " is-wide" : "")}
        >
          {notice && (
            <div className="wb-notice" role="alert">
              <span>{notice}</span>
              <button type="button" aria-label="关闭提示" onClick={() => setNotice("")}>
                ×
              </button>
            </div>
          )}
          <header className="wb-settings-header">
            <div className="wb-settings-header-top">
              <div>
                <h1>{activeSettingsMeta.label}</h1>
                <p className="wb-settings-subtitle">{activeSettingsMeta.subtitle}</p>
              </div>
              <button
                type="button"
                className="wb-settings-close"
                aria-label="关闭设置"
                title="关闭设置 (Esc)"
                onClick={closeToSession}
              >
                ×
              </button>
            </div>
          </header>

          {settingsTab === "general" && (
            <>
              <section className="wb-settings-section">
                <h2 className="wb-settings-section-title">当前工作区</h2>
                <div className="wb-settings-card">
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>工作区路径</strong>
                      <p>
                        {workspace ? (
                          <code className="wb-setting-key">{workspace}</code>
                        ) : (
                          "尚未选择本地工作区目录。"
                        )}
                      </p>
                    </div>
                    <div className="wb-actions">
                      {workspace && (
                        <SettingsCopyButton value={workspace} ariaLabel="复制当前工作区路径" />
                      )}
                      <button
                        type="button"
                        className="wb-secondary"
                        onClick={() => void pickProject(true)}
                      >
                        切换目录
                      </button>
                    </div>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>Git 分支与工作树状态</strong>
                      <p>
                        {workspaceStatus?.git_root
                          ? `仓库根目录：${workspaceStatus.git_root}`
                          : workspace
                            ? "只读观测工作区当前分支与未提交修改。"
                            : "选择工作区后自动读取仓库分支状态。"}
                      </p>
                    </div>
                    <div className="wb-actions">
                      {workspaceStatus?.branch ? (
                        <span className="wb-branch-pill">⎇ {workspaceStatus.branch}</span>
                      ) : (
                        <span className="wb-badge is-muted">无分支信息</span>
                      )}
                      {workspaceStatus && (
                        <span
                          className={"wb-badge " + (workspaceStatus.dirty ? "is-warn" : "is-ok")}
                        >
                          {workspaceStatus.dirty ? "存在未提交改动" : "工作区干净"}
                        </span>
                      )}
                    </div>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>
                        Agent 配置文件 <code className="wb-setting-key">.agent/config.yaml</code>
                      </strong>
                      <p>
                        {workspaceStatus?.config_error
                          ? `配置校验失败：${workspaceStatus.config_error}`
                          : workspaceStatus?.config_source ||
                            workspaceStatus?.config_path ||
                            "严格校验工作区与用户级配置合并结果，未知配置项直接报错。"}
                      </p>
                    </div>
                    <div className="wb-actions">
                      <span
                        className={
                          "wb-badge " +
                          (workspaceStatus?.config_loaded
                            ? "is-ok"
                            : workspaceStatus?.has_config
                              ? "is-warn"
                              : "is-muted")
                        }
                      >
                        {workspaceStatus?.config_loaded
                          ? "已加载"
                          : workspaceStatus?.has_config
                            ? "加载异常"
                            : "待配置"}
                      </span>
                      {workspaceStatus?.config_fingerprint && (
                        <span
                          className="wb-hash-pill"
                          title={`配置指纹：${workspaceStatus.config_fingerprint}`}
                        >
                          {workspaceStatus.config_fingerprint.slice(0, 12)}
                        </span>
                      )}
                    </div>
                  </div>
                </div>
              </section>

              <section className="wb-settings-section">
                <h2 className="wb-settings-section-title">最近项目</h2>
                <div className="wb-settings-card">
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>显示已归档项目</strong>
                      <p>在下方最近打开的工作区列表中一并展示已归档的项目记录。</p>
                    </div>
                    <button
                      type="button"
                      role="switch"
                      aria-checked={showArchivedProjects}
                      aria-label="显示已归档项目"
                      className={"wb-toggle-switch" + (showArchivedProjects ? " is-checked" : "")}
                      onClick={() => setShowArchivedProjects((current) => !current)}
                    >
                      <span className="wb-toggle-thumb" />
                    </button>
                  </div>
                  {displayedRecentProjects.length > 0 ? (
                    displayedRecentProjects.map((project) => {
                      const isCurrent = workspace === project.path;
                      const displayName =
                        project.name || project.path.split("/").pop() || project.path;
                      return (
                        <div className="wb-setting-row" key={project.path}>
                          <div className="wb-setting-meta">
                            <strong>
                              {displayName}
                              {isCurrent && <span className="wb-badge is-ok">当前</span>}
                              {project.archived && <span className="wb-badge is-muted">已归档</span>}
                            </strong>
                            <p>
                              <code className="wb-setting-key">{project.path}</code>
                            </p>
                          </div>
                          <div className="wb-actions">
                            <button
                              type="button"
                              className="wb-secondary"
                              disabled={isCurrent}
                              onClick={() =>
                                void switchProject(project.path, { preservePage: true })
                              }
                            >
                              {isCurrent ? "当前工作区" : "切换"}
                            </button>
                            <button
                              type="button"
                              className="wb-secondary"
                              onClick={() => void toggleArchiveProject(project)}
                            >
                              {project.archived ? "取消归档" : "归档"}
                            </button>
                          </div>
                        </div>
                      );
                    })
                  ) : (
                    <div className="wb-setting-row">
                      <div className="wb-setting-meta">
                        <strong>暂无最近项目</strong>
                        <p>点击上方「切换目录」添加并打开你的第一个本地 Cairn 工作区。</p>
                      </div>
                    </div>
                  )}
                </div>
              </section>

              <p className="wb-settings-footnote">
                桌面偏好与最近项目列表保存在用户配置目录（<code>~/.cairn</code>
                ），仅用于工作台导航展示，不替代各工作区的 Event Journal 运行事实。
              </p>
            </>
          )}

          {settingsTab === "appearance" && (
            <>
              <section className="wb-settings-section">
                <h2 className="wb-settings-section-title">主题</h2>
                <div className="wb-settings-card">
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>
                        外观模式 <code className="wb-setting-key">cairn.gui.colorScheme</code>
                      </strong>
                      <p>
                        宣纸暖石（Paper Stone）与玄武岩夜色（Basalt Night）双生主题，支持跟随系统或手动锁定。
                      </p>
                    </div>
                    <div
                      className={
                        "wb-segmented" + (themePill && themePill.width > 0 ? " has-pill" : "")
                      }
                      role="group"
                      aria-label="外观模式"
                    >
                      {themePill && themePill.width > 0 && (
                        <span
                          className="wb-segmented-pill"
                          aria-hidden="true"
                          style={{ left: themePill.left, width: themePill.width }}
                        />
                      )}
                      {(
                        [
                          ["system", "跟随系统"],
                          ["light", "宣纸白"],
                          ["dark", "玄武岩黑"],
                        ] as const
                      ).map(([mode, label]) => (
                        <button
                          key={mode}
                          ref={(node) => {
                            themeBtnRefs.current[mode] = node;
                          }}
                          type="button"
                          className={colorScheme === mode ? "current" : ""}
                          aria-pressed={colorScheme === mode}
                          onClick={() => setColorScheme(mode)}
                        >
                          {label}
                        </button>
                      ))}
                    </div>
                  </div>
                </div>
              </section>

              <section className="wb-settings-section">
                <h2 className="wb-settings-section-title">界面排版</h2>
                <div className="wb-settings-card">
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>
                        界面字体栈 <code className="wb-setting-key">--wb-font</code>
                      </strong>
                      <p>
                        调用系统原生高可读性无衬线字体栈：SF Pro Text、PingFang SC、Inter、Noto Sans
                        CJK SC。
                      </p>
                    </div>
                    <span className="wb-badge is-muted">系统原生（只读）</span>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>
                        基础字号 <code className="wb-setting-key">cairn.gui.fontSize</code>
                      </strong>
                      <p>
                        调节桌面端全局排版基准字号（当前 {fontSize}px，范围 12px – 19px），不影响
                        Agent 运行策略。
                      </p>
                    </div>
                    <div
                      className={
                        "wb-stepper" +
                        (stepperBump === "min"
                          ? " is-bump-min"
                          : stepperBump === "max"
                            ? " is-bump-max"
                            : "")
                      }
                      role="group"
                      aria-label="基础字号"
                    >
                      <button
                        type="button"
                        aria-label="减小基础字号"
                        aria-disabled={fontSize <= 12}
                        onClick={() => stepFontSize(-1)}
                      >
                        -
                      </button>
                      <span>{fontSize}</span>
                      <button
                        type="button"
                        aria-label="增大基础字号"
                        aria-disabled={fontSize >= 19}
                        onClick={() => stepFontSize(1)}
                      >
                        +
                      </button>
                    </div>
                  </div>
                </div>
              </section>

              <section className="wb-settings-section">
                <h2 className="wb-settings-section-title">代码与终端文字</h2>
                <div className="wb-settings-card">
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>
                        等宽字体栈 <code className="wb-setting-key">--wb-mono</code>
                      </strong>
                      <p>
                        用于代码预览、工具载荷、操作指纹与日志视图：SFMono-Regular、JetBrains
                        Mono、Menlo、Consolas。
                      </p>
                    </div>
                    <span className="wb-badge is-muted">等宽对齐（只读）</span>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>Diff 与代码块排版策略</strong>
                      <p>
                        统一 Diff 阅读器严格保留原始行号锚点与空白结构；行级审查批注作为本地元数据独立挂载，不改写工作区文件。
                      </p>
                    </div>
                    <span className="wb-badge is-ok">只读安全渲染</span>
                  </div>
                </div>
              </section>

              <p className="wb-settings-footnote">
                主题与字号偏好持久化于本机 <code>localStorage</code>（
                <code>cairn.gui.colorScheme</code>、<code>cairn.gui.fontSize</code>），并在{" "}
                <code>index.html</code> 首屏阶段同步写入 <code>&lt;meta name=&quot;color-scheme&quot;&gt;</code>{" "}
                以实现明暗主题零闪烁启动。
              </p>
            </>
          )}

          {settingsTab === "models" && (
            <ProviderSettings key={workspace} workspace={workspace} notify={onQueueError} />
          )}

          {settingsTab === "shortcuts" && (
            <>
              <section className="wb-settings-section">
                <h2 className="wb-settings-section-title">全局与导航</h2>
                <div className="wb-settings-card">
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>打开或关闭设置中心</strong>
                      <p>在当前工作区会话与全幅设置中心之间切换（Windows / Linux 为 Ctrl + ,）。</p>
                    </div>
                    <span className="wb-kbd-group">
                      <kbd className="wb-kbd">⌘</kbd>
                      <kbd className="wb-kbd">,</kbd>
                    </span>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>返回工作区或收起抽屉</strong>
                      <p>在设置中心按下时返回工作区；在窄屏工作台按下时收起侧栏导航抽屉。</p>
                    </div>
                    <span className="wb-kbd-group">
                      <kbd className="wb-kbd">Esc</kbd>
                    </span>
                  </div>
                </div>
              </section>

              <section className="wb-settings-section">
                <h2 className="wb-settings-section-title">对话与指令舱</h2>
                <div className="wb-settings-card">
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>发送任务 / 提交安全点引导</strong>
                      <p>空闲时启动新任务；运行中按当前投递模式提交安全点补充引导或候选队列。</p>
                    </div>
                    <span className="wb-kbd-group">
                      <kbd className="wb-kbd">Enter</kbd>
                    </span>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>提交至候选任务队列</strong>
                      <p>将草稿加入工作区候选队列，仍须人工明确确认后才会启动新运行。</p>
                    </div>
                    <span className="wb-kbd-group">
                      <kbd className="wb-kbd">⌘</kbd>
                      <kbd className="wb-kbd">Enter</kbd>
                    </span>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>任务输入框换行</strong>
                      <p>在多行任务描述或审查补充说明中插入换行而不触发提交。</p>
                    </div>
                    <span className="wb-kbd-group">
                      <kbd className="wb-kbd">Shift</kbd>
                      <kbd className="wb-kbd">Enter</kbd>
                    </span>
                  </div>
                </div>
              </section>

              <section className="wb-settings-section">
                <h2 className="wb-settings-section-title">代码审查（Diff）</h2>
                <div className="wb-settings-card">
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>保存行级审查批注</strong>
                      <p>在 Diff 行内评论框中保存审查备注并自动带回当前会话草稿，不直接执行修改。</p>
                    </div>
                    <span className="wb-kbd-group">
                      <kbd className="wb-kbd">⌘</kbd>
                      <kbd className="wb-kbd">Enter</kbd>
                    </span>
                  </div>
                </div>
              </section>

              <p className="wb-settings-footnote">
                以上列表仅展示客户端已真实绑定的键盘快捷键；所有文本输入快捷键均识别中文输入法组合态（
                <code>isComposing</code> / <code>keyCode 229</code>），避免选词回车误触发提交。
              </p>
            </>
          )}

          {isCapabilitySection(settingsTab) && (
            <CapabilitySettings
              key={workspace}
              workspace={workspace}
              notify={onQueueError}
              section={settingsTab}
              onCountsChange={setCapabilityCounts}
            />
          )}

          {settingsTab === "usage" && (
            <UsageView key={workspace} workspace={workspace} notify={onQueueError} />
          )}

          {settingsTab === "about" && (
            <>
              <section className="wb-settings-section">
                <h2 className="wb-settings-section-title">客户端与运行环境</h2>
                <div className="wb-settings-card">
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>应用名称</strong>
                      <p>基于 Go 事件溯源内核与 Wails 桌面桥接构建的本地编码工作台。</p>
                    </div>
                    <div className="wb-actions">
                      <strong>{healthInfo?.app_name || "Cairn"}</strong>
                      <span
                        className={
                          "wb-badge " + (bridgeStatus === "healthy" ? "is-ok" : "is-warn")
                        }
                      >
                        {bridgeStatusText}
                      </span>
                    </div>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>应用版本</strong>
                      <p>当前桌面宿主与桥接协议版本。</p>
                    </div>
                    <code className="wb-setting-key">{healthInfo?.version || "0.1.0"}</code>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>Go 运行时版本</strong>
                      <p>编译并承载单写入者 Agent Loop 与 SQLite 存储适配层的 Go 工具链版本。</p>
                    </div>
                    <code className="wb-setting-key">{healthInfo?.go_version || "—"}</code>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>
                        用户配置根目录 <code className="wb-setting-key">~/.cairn</code>
                      </strong>
                      <p>统一存放用户级配置、桌面偏好与本机 0600 权限环境变量引用文件。</p>
                    </div>
                    <div className="wb-actions">
                      <code className="wb-setting-key">{healthInfo?.user_root || "~/.cairn"}</code>
                      <SettingsCopyButton
                        value={healthInfo?.user_root || "~/.cairn"}
                        ariaLabel="复制用户配置目录"
                      />
                    </div>
                  </div>
                </div>
              </section>

              <section className="wb-settings-section">
                <h2 className="wb-settings-section-title">本地优先与隐私边界</h2>
                <div className="wb-settings-card">
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>Event Journal 本机事实源</strong>
                      <p>
                        所有运行事件、审批指纹与对账决议均以防篡改哈希链记录在本地 SQLite，UI
                        与二级记忆永不替代事实源。
                      </p>
                    </div>
                    <span className="wb-badge is-ok">本机 SQLite</span>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>遥测默认关闭</strong>
                      <p>
                        遥测仅为可选只读旁路，默认关闭；任何情况下均不导出 Prompt、密钥、原始工具输出或本机绝对路径。
                      </p>
                    </div>
                    <span className="wb-badge is-muted">默认关闭</span>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>密钥零落盘引用</strong>
                      <p>
                        供应商凭据只通过环境变量名称（如{" "}
                        <code className="wb-setting-key">provider.api_key_env</code>
                        ）引用，明文密钥绝不写入配置文件、事件日志或诊断载荷。
                      </p>
                    </div>
                    <span className="wb-badge is-ok">仅环境变量引用</span>
                  </div>
                </div>
              </section>

              <section className="wb-settings-section">
                <h2 className="wb-settings-section-title">工作区健康诊断（Doctor）</h2>
                <div className="wb-settings-card">
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>执行只读环境与配置自检</strong>
                      <p>
                        {workspace
                          ? `检查工作区 ${workspace} 的配置有效性、Provider 准入、沙箱后端与 Journal 完整性。`
                          : "请先在「常规」中选择工作区后再运行 Doctor 诊断。"}
                      </p>
                    </div>
                    <button
                      type="button"
                      className="wb-primary"
                      disabled={!workspace || doctorRunning}
                      onClick={() => void runWorkspaceDoctor()}
                    >
                      {doctorRunning ? "诊断中…" : "运行 Doctor 诊断"}
                    </button>
                  </div>
                  {doctorError && (
                    <div className="wb-settings-card-pad">
                      <div className="wb-actions">
                        <span className="wb-badge is-warn">诊断失败</span>
                        <span>{doctorError}</span>
                      </div>
                    </div>
                  )}
                  {doctorReport !== null && (
                    <div className="wb-settings-card-pad">
                      {doctorSummary && (
                        <div className="wb-actions" style={{ marginBottom: 12 }}>
                          {doctorSummary.ready !== undefined && (
                            <span
                              className={"wb-badge " + (doctorSummary.ready ? "is-ok" : "is-warn")}
                            >
                              {doctorSummary.ready ? "总体就绪" : "未就绪"}
                            </span>
                          )}
                          {doctorSummary.config_valid !== undefined && (
                            <span
                              className={
                                "wb-badge " + (doctorSummary.config_valid ? "is-ok" : "is-warn")
                              }
                            >
                              {doctorSummary.config_valid ? "配置有效" : "配置异常"}
                            </span>
                          )}
                          {doctorSummary.journal_ok !== undefined && (
                            <span
                              className={
                                "wb-badge " + (doctorSummary.journal_ok ? "is-ok" : "is-warn")
                              }
                            >
                              {doctorSummary.journal_ok ? "Journal 完整" : "Journal 异常"}
                            </span>
                          )}
                          {doctorSummary.artifacts_ok !== undefined && (
                            <span
                              className={
                                "wb-badge " + (doctorSummary.artifacts_ok ? "is-ok" : "is-warn")
                              }
                            >
                              {doctorSummary.artifacts_ok ? "制品完整" : "制品异常"}
                            </span>
                          )}
                          {doctorSummary.memory_ok !== undefined && (
                            <span
                              className={
                                "wb-badge " + (doctorSummary.memory_ok ? "is-ok" : "is-muted")
                              }
                            >
                              {doctorSummary.memory_ok ? "记忆正常" : "记忆待检"}
                            </span>
                          )}
                          {doctorSummary.config_fingerprint && (
                            <span
                              className="wb-hash-pill"
                              title={doctorSummary.config_fingerprint}
                            >
                              {doctorSummary.config_fingerprint.slice(0, 12)}
                            </span>
                          )}
                        </div>
                      )}
                      <pre className="wb-code">
                        {typeof doctorReport === "string"
                          ? doctorReport
                          : JSON.stringify(doctorReport, null, 2)}
                      </pre>
                    </div>
                  )}
                </div>
              </section>

              <p className="wb-settings-footnote">
                Doctor 诊断为纯只读预检，不会修改工作区源码、配置文件或 SQLite Event Journal。
              </p>
            </>
          )}
        </div>
      </main>
    </div>
  );

  return (
    <div
      className={
        "wb-shell" +
        (page !== "settings" && !sidebarOpen ? " wb-sidebar-collapsed" : "") +
        (page !== "settings" && expandedDiff ? " wb-review-expanded" : "")
      }
      data-testid="cairn-workbench-shell"
    >
      {page === "settings" ? (
        settingsShell
      ) : (
        <>
      {sidebarOpen && (
        <button
          type="button"
          className="wb-sidebar-backdrop"
          aria-label="关闭导航抽屉"
          onClick={() => setSidebarOpen(false)}
        />
      )}
      <aside className="wb-sidebar" aria-label="项目与会话">
        <div className="wb-sidebar-top">
          <div className="wb-traffic">
            <span />
            <span />
            <span />
          </div>
          <div className="wb-sidebar-top-actions">
            <button
              type="button"
              title="搜索会话"
              aria-label="切换会话搜索"
              aria-pressed={searchOpen}
              onClick={() => {
                setSearchOpen((open) => {
                  const next = !open;
                  if (next) {
                    window.setTimeout(() => sessionSearchRef.current?.focus(), 0);
                  }
                  return next;
                });
              }}
            >
              ⌕
            </button>
          </div>
        </div>
        <nav className="wb-nav">
          <button
            type="button"
            className={page === "home" && !runId && !needsAttentionOnly ? "active" : ""}
            onClick={() => {
              setNeedsAttentionOnly(false);
              selectRun(null);
              setPage("home");
            }}
          >
            ＋ <span>新建任务</span>
          </button>
          <button
            type="button"
            className={page === "home" && needsAttentionOnly ? "active" : ""}
            onClick={() => {
              setNeedsAttentionOnly((prev) => !prev);
              setShowArchived(false);
              setPage("home");
            }}
          >
            ⚑ <span>需要我</span>
            {attentionCount > 0 && <span className="wb-nav-badge is-warn">{attentionCount}</span>}
          </button>
          <button
            type="button"
            className={page === "workflow" ? "active" : ""}
            onClick={() => void openWorkflow()}
          >
            ◇ <span>工作流</span>
          </button>
        </nav>
        <div className="wb-sidebar-title">
          <span>工作区</span>
          <div>
            <button
              type="button"
              aria-label="切换已归档会话"
              onClick={() => {
                setNeedsAttentionOnly(false);
                setShowArchived((v) => !v);
              }}
            >
              {showArchived ? "显示活跃" : "查看归档"}
            </button>
            <button
              type="button"
              title="添加项目"
              aria-label="添加项目"
              onClick={() => void pickProject()}
            >
              ＋
            </button>
          </div>
        </div>
        <div className="wb-project-list">
          {recent.map((project) => {
            const isCurrent = workspace === project.path;
            return (
              <div
                key={project.path}
                className={"wb-project-node" + (isCurrent ? " is-current" : "")}
              >
                <button
                  type="button"
                  className={isCurrent ? "current" : ""}
                  onClick={() => void switchProject(project.path)}
                >
                  <span aria-hidden="true">{isCurrent ? "▾" : "▸"}</span>
                  <span>{project.name || project.path.split("/").pop()}</span>
                </button>
                {isCurrent && (
                  <div className="wb-project-runs">
                    {searchOpen && (
                      <div className="wb-sidebar-search" style={{ padding: "2px 0 4px" }}>
                        <input
                          ref={sessionSearchRef}
                          aria-label="搜索会话"
                          placeholder="搜索会话…"
                          value={filter}
                          onChange={(event) => setFilter(event.target.value)}
                        />
                      </div>
                    )}
                    {visibleRuns.length > 0 ? (
                      visibleRuns.map((run) => (
                        <SessionRow
                          key={run.run_id}
                          run={run}
                          selected={runId === run.run_id}
                          indent={treeMeta.depthMap.get(run.run_id) ?? 0}
                          childCount={treeMeta.childCountMap.get(run.run_id) ?? 0}
                          onSelect={() => selectRun(run.run_id)}
                          onRename={(title) => updateSession(run, title)}
                          onFlags={(pinned, archived) =>
                            updateSession(run, undefined, { pinned, archived })
                          }
                          onFork={() => forkRun(run.run_id)}
                        />
                      ))
                    ) : (
                      <div className="wb-empty-runs">暂无任务</div>
                    )}
                  </div>
                )}
              </div>
            );
          })}
          {workspace && !recent.some((project) => project.path === workspace) && (
            <div className="wb-project-node is-current">
              <button
                type="button"
                className="current"
                onClick={() => void switchProject(workspace)}
              >
                <span aria-hidden="true">▾</span>
                <span>{workspaceName}</span>
              </button>
              <div className="wb-project-runs">
                {searchOpen && (
                  <div className="wb-sidebar-search" style={{ padding: "2px 0 4px" }}>
                    <input
                      ref={sessionSearchRef}
                      aria-label="搜索会话"
                      placeholder="搜索会话…"
                      value={filter}
                      onChange={(event) => setFilter(event.target.value)}
                    />
                  </div>
                )}
                {visibleRuns.length > 0 ? (
                  visibleRuns.map((run) => (
                    <SessionRow
                      key={run.run_id}
                      run={run}
                      selected={runId === run.run_id}
                      indent={treeMeta.depthMap.get(run.run_id) ?? 0}
                      childCount={treeMeta.childCountMap.get(run.run_id) ?? 0}
                      onSelect={() => selectRun(run.run_id)}
                      onRename={(title) => updateSession(run, title)}
                      onFlags={(pinned, archived) =>
                        updateSession(run, undefined, { pinned, archived })
                      }
                      onFork={() => forkRun(run.run_id)}
                    />
                  ))
                ) : (
                  <div className="wb-empty-runs">暂无任务</div>
                )}
              </div>
            </div>
          )}
          {!recent.length && !workspace && <div className="wb-hint">尚未添加工作区</div>}
        </div>
        <div
          className="wb-sidebar-bottom"
          style={{
            flexDirection: "row",
            alignItems: "center",
            justifyContent: "space-between",
            gap: 8,
          }}
        >
          <span className="wb-sidebar-version" style={{ padding: "0 6px" }}>
            <i
              className={"wb-status-dot " + bridgeDotTone}
              aria-hidden="true"
              style={{
                display: "inline-block",
                width: 7,
                height: 7,
                borderRadius: "50%",
                marginRight: 6,
                verticalAlign: "middle",
                background:
                  bridgeStatus === "healthy"
                    ? "var(--wb-moss)"
                    : bridgeStatus === "connecting"
                      ? "var(--wb-subtle)"
                      : "var(--wb-amber)",
              }}
            />
            Cairn · {bridgeStatusText}
          </span>
          <button type="button" onClick={() => void openSettings()}>
            ⚙ <span>设置</span>
          </button>
        </div>
      </aside>
      <div className="wb-workspace-frame">
        <main className="wb-stage">
          <header className="wb-toolbar">
            <div>
              <button
                type="button"
                className="wb-plain"
                title="显示或隐藏侧栏"
                aria-label="显示或隐藏侧栏"
                aria-expanded={sidebarOpen}
                onClick={() => setSidebarOpen((open) => !open)}
              >
                ☷
              </button>
              <span className="wb-crumb">
                {workspace.split("/").pop() || "Cairn"}
                {workspaceStatus?.dirty && (
                  <span className="wb-dirty" title="工作区存在未提交修改">
                    {" "}
                    ● 未提交
                  </span>
                )}
              </span>
              {(runId || page === "workflow") && <span className="wb-divider">/</span>}
              <strong>
                {page === "workflow"
                  ? "工作流"
                  : runId
                    ? selected?.title || "当前会话"
                    : "新建任务"}
              </strong>
              {page === "home" && runId && (
                <span className="wb-run-status">{statusName(selected)}</span>
              )}
            </div>
            <div className="wb-toolbar-actions">
              {canOpenInEditor && (
                <div ref={editorActionRef} className="wb-inline-select">
                  <button
                    type="button"
                    aria-expanded={openLocations.length > 1 ? editorMenuOpen : undefined}
                    onClick={() => {
                      if (openLocations.length > 1) {
                        setEditorMenuOpen((open) => !open);
                      } else {
                        void handleOpenWorkspaceInEditor();
                      }
                    }}
                  >
                    <OpenLocationIcon
                      id={openLocations.length === 1 ? openLocations[0].id : "default"}
                      size={15}
                    />
                    <span>编辑器打开 ↗</span>
                  </button>
                  {editorMenuOpen && openLocations.length > 1 && (
                    <div className="wb-inline-popover is-right wb-open-loc-popover" role="menu">
                      {openLocations.map((loc) => (
                        <button
                          key={loc.id}
                          type="button"
                          onClick={() => void handleOpenWorkspaceInEditor(loc.id)}
                        >
                          <span className="wb-open-loc-main">
                            <OpenLocationIcon id={loc.id} size={18} />
                            <span className="wb-open-loc-label">用 {loc.label} 打开</span>
                          </span>
                          <span className="wb-open-loc-arrow" aria-hidden="true">↗</span>
                        </button>
                      ))}
                      {typeof desktop.openPathInEditor === "function" && (
                        <button
                          type="button"
                          onClick={() => void handleOpenWorkspaceInEditor()}
                        >
                          <span className="wb-open-loc-main">
                            <OpenLocationIcon id="default" size={18} />
                            <span className="wb-open-loc-label">系统默认编辑器</span>
                          </span>
                          <span className="wb-open-loc-arrow" aria-hidden="true">↗</span>
                        </button>
                      )}
                    </div>
                  )}
                </div>
              )}
              <button
                type="button"
                className="wb-icon-btn"
                title="切换明暗主题"
                aria-label="切换明暗主题"
                onClick={toggleQuickColorScheme}
              >
                ◐
              </button>
            </div>
          </header>
          {notice && (
            <div className="wb-notice" role="alert">
              <span>{notice}</span>
              <button type="button" aria-label="关闭提示" onClick={() => setNotice("")}>
                ×
              </button>
            </div>
          )}
          {page === "home" &&
            (!runId ? (
              <div className="wb-home">
                <div className="wb-welcome">
                  <img src={icon} alt="Cairn 石灵图标" />
                  <h1>今天想让 Cairn 做什么？</h1>
                  <div className="wb-hero-sentence" ref={heroSentenceRef}>
                    <span>在</span>
                    <div
                      className={
                        "wb-inline-select" + (heroPopover === "workspace" ? " is-open" : "")
                      }
                    >
                      <button
                        type="button"
                        aria-haspopup="listbox"
                        aria-expanded={heroPopover === "workspace"}
                        onClick={() =>
                          setHeroPopover((cur) => (cur === "workspace" ? "none" : "workspace"))
                        }
                      >
                        <span aria-hidden="true">📁</span>
                        <span>{workspaceName}</span>
                        <span aria-hidden="true">▾</span>
                      </button>
                      {heroPopover === "workspace" && (
                        <div className="wb-inline-popover" role="listbox" aria-label="快速切换工作区">
                          {recent.map((item) => (
                            <button
                              key={item.path}
                              type="button"
                              className={workspace === item.path ? "current" : ""}
                              onClick={() => {
                                setHeroPopover("none");
                                void switchProject(item.path);
                              }}
                            >
                              <span>📁 {item.name || item.path.split("/").pop()}</span>
                              {workspace === item.path && <span aria-hidden="true">✓</span>}
                            </button>
                          ))}
                          <button
                            type="button"
                            onClick={() => {
                              setHeroPopover("none");
                              void pickProject();
                            }}
                          >
                            <span>＋ 打开其他目录…</span>
                          </button>
                        </div>
                      )}
                    </div>
                    <span>用</span>
                    <div
                      className={"wb-inline-select" + (heroPopover === "model" ? " is-open" : "")}
                    >
                      <button
                        type="button"
                        aria-haspopup="listbox"
                        aria-expanded={heroPopover === "model"}
                        onClick={() =>
                          setHeroPopover((cur) => (cur === "model" ? "none" : "model"))
                        }
                      >
                        <span aria-hidden="true">◈</span>
                        <span>{currentModelOrPresetLabel}</span>
                        <span aria-hidden="true">▾</span>
                      </button>
                      {heroPopover === "model" && (
                        <div
                          className="wb-inline-popover"
                          role="listbox"
                          aria-label="快速选择模型或预设"
                        >
                          <button
                            type="button"
                            className={!model.trim() && !activePreset ? "current" : ""}
                            onClick={() => {
                              setModel("");
                              setHeroPopover("none");
                            }}
                          >
                            <span>跟随工作区默认模型</span>
                            {!model.trim() && !activePreset && <span aria-hidden="true">✓</span>}
                          </button>
                          {providerPresets.map((preset) => (
                            <button
                              key={preset.name}
                              type="button"
                              className={preset.active && !model.trim() ? "current" : ""}
                              onClick={() => void handleActivatePresetFromHero(preset)}
                            >
                              <span>
                                ◈ {preset.name}
                                {preset.model ? ` · ${preset.model}` : ""}
                              </span>
                              {preset.active && !model.trim() && <span aria-hidden="true">✓</span>}
                            </button>
                          ))}
                          <button
                            type="button"
                            onClick={() => {
                              setHeroPopover("none");
                              setSettingsTab("models");
                              openSettings();
                            }}
                          >
                            <span>⚙ 前往模型与供应商设置…</span>
                          </button>
                        </div>
                      )}
                    </div>
                    <span>可靠推进每一次变更</span>
                  </div>
                </div>
                {compose}
                <div className="wb-suggestions">
                  <span>从一个目标开始</span>
                  <button
                    type="button"
                    onClick={() =>
                      setDraft("帮我检查当前项目中的潜在问题，并给出有证据的修复建议。")
                    }
                  >
                    分析项目
                  </button>
                  <button
                    type="button"
                    onClick={() =>
                      setDraft("帮我审查当前 Git 变更，指出潜在风险和测试缺口。")
                    }
                  >
                    审查变更
                  </button>
                  <button
                    type="button"
                    onClick={() => setDraft("为这个功能制定实现计划，先不要修改文件。")}
                  >
                    制定计划
                  </button>
                </div>
                {workspaceStatus && !workspaceStatus.has_config && (
                  <p className="wb-config-warning">
                    当前工作区尚未完成 Agent 配置。请先在设置中检查 Provider 与准入状态。
                  </p>
                )}
                <section className="wb-activity-card" aria-label="本地会话活跃与运行概览">
                  <div className="wb-activity-head">
                    <div role="tablist" aria-label="本地运行视图切换">
                      <button
                        type="button"
                        className={activityTab === "heatmap" ? "current" : ""}
                        aria-pressed={activityTab === "heatmap"}
                        onClick={() => setActivityTab("heatmap")}
                      >
                        会话活跃
                      </button>
                      <button
                        type="button"
                        className={activityTab === "overview" ? "current" : ""}
                        aria-pressed={activityTab === "overview"}
                        onClick={() => setActivityTab("overview")}
                      >
                        运行概览
                      </button>
                    </div>
                    <span>
                      共 {runs.length} 次记录 · 活跃 {activitySummary.activeDays} 天
                    </span>
                  </div>
                  {activityTab === "heatmap" ? (
                    <div className="wb-heatmap-wrap">
                      <div className="wb-heatmap-months" aria-hidden="true">
                        {activitySummary.months.map((monthLabel, idx) => (
                          <span key={`${monthLabel}-${idx}`}>{monthLabel}</span>
                        ))}
                      </div>
                      <div
                        className="wb-heatmap-grid"
                        role="img"
                        aria-label={`本地过去 24 周会话活跃度，活跃 ${activitySummary.activeDays} 天`}
                      >
                        {activitySummary.cells.map((cell) => (
                          <span
                            key={cell.dateKey}
                            className={`wb-heat-cell lv-${cell.level}`}
                            title={`${cell.dateKey}：${cell.count} 次本地运行`}
                          />
                        ))}
                      </div>
                    </div>
                  ) : (
                    <div className="wb-activity-stats">
                      <div>
                        <span>待人工介入</span>
                        <strong>{activitySummary.waitingCount}</strong>
                      </div>
                      <div>
                        <span>运行中</span>
                        <strong>{activitySummary.activeRunCount}</strong>
                      </div>
                      <div>
                        <span>已完成</span>
                        <strong>{activitySummary.completedCount}</strong>
                      </div>
                      <div>
                        <span>配置状态</span>
                        <strong>{workspaceStatus?.has_config ? "已就绪" : "待配置"}</strong>
                      </div>
                    </div>
                  )}
                </section>
              </div>
            ) : (
              <div className="wb-session">
                <div className="wb-session-scroll">
                  <div className="wb-session-content">
                    {parentRun && (
                      <button
                        type="button"
                        className="wb-parent-run"
                        onClick={() => selectRun(parentRun)}
                      >
                        ← 返回父运行 {parentRun}
                      </button>
                    )}
                    {selected?.waiting_reconcile && runId && (
                      <>
                        <ReconciliationPanel
                          key={runId}
                          workspace={workspace}
                          runId={runId}
                          events={detail?.events || []}
                          onDone={() => {
                            setNotice("已提交人工对账，请查看 Journal 中的后续状态。");
                            void refreshRuns(workspace, generationRef.current);
                          }}
                          onError={onQueueError}
                        />
                        <AbortRunPanel
                          key={"abort-" + runId}
                          workspace={workspace}
                          runId={runId}
                          onDone={() => {
                            setNotice("终止请求已提交，等待运行事实更新。");
                            void refreshRuns(workspace, generationRef.current);
                          }}
                          onError={onQueueError}
                        />
                      </>
                    )}
                    {blocks.length ? (
                      <Transcript
                        blocks={blocks}
                        onApproval={decide}
                        onInspectFile={(targetPath) => void inspectChangedFile(targetPath)}
                      />
                    ) : (
                      <p className="wb-hint">正在等待 Journal 中的运行记录…</p>
                    )}
                    <div ref={bottomRef} />
                  </div>
                </div>
                <div className="wb-session-composer">{compose}</div>
              </div>
            ))}
          {page === "workflow" && (
            <section className="wb-content-page">
              <div className="wb-page-head">
                <span>WORKFLOWS</span>
                <h1>工作流</h1>
                <p>创建可重复执行的有序步骤；预演不是成功验证。</p>
              </div>
              {workspace ? (
                <WorkflowsView key={workspace} workspace={workspace} notify={onQueueError} />
              ) : (
                <p>请先选择工作区。</p>
              )}
              <button type="button" className="wb-secondary" onClick={closeToSession}>
                返回工作区
              </button>
            </section>
          )}
        </main>
      {page === "home" && runId && panel !== "closed" && (
        <aside className="wb-inspector" aria-label="会话右侧工作台">
          <div className="wb-inspector-head">
            <strong>
              {panel === "workbench"
                ? "Agent 工作台"
                : panel === "changes"
                  ? "检查变更"
                  : panel === "queue"
                    ? "任务队列"
                    : panel === "subagents"
                      ? "子智能体"
                      : panel === "files"
                        ? "项目文件"
                        : "验证与证据"}
            </strong>
            <div>
              {panel === "changes" && (
                <button
                  type="button"
                  title={expandedDiff ? "恢复侧边栏" : "放大审查"}
                  onClick={() => setExpandedDiff((open) => !open)}
                >
                  {expandedDiff ? "还原" : "⛶"}
                </button>
              )}
              <button
                type="button"
                aria-label="关闭右侧面板"
                onClick={() => {
                  setPanel("closed");
                  setExpandedDiff(false);
                }}
              >
                ×
              </button>
            </div>
          </div>
          {panel === "files" && <FilesPanel workspace={workspace} notify={onQueueError} />}
          {panel === "queue" && (
            <QueuePanel
              workspace={workspace}
              revision={queueRevision}
              onRun={onRunFromQueue}
              onError={onQueueError}
            />
          )}
          {panel === "subagents" && (
            <div className="wb-panel-body">
              <div className="wb-label">Agent Delegate</div>
              <h2>子智能体与委派树</h2>
              <p className="wb-hint">
                会话树：{sessionTree ? Object.keys(sessionTree.nodes).length + " 个 Run" : "暂无可用投影"}
              </p>
              <div className="wb-tree-card">
                <div>
                  <span className="wb-badge is-accent">当前 Run</span>
                  <strong>{runId}</strong>
                </div>
                <p>{selected?.title || selected?.task || "当前会话任务"}</p>
                {subagents.length > 0 ? (
                  <div className="wb-tree-children">
                    {subagents.map((child) => {
                      const isDone = child.state === "completed";
                      return (
                        <div className="wb-tree-card" key={child.childRunId}>
                          <div>
                            <span className={"wb-badge " + (isDone ? "is-ok" : "is-warn")}>
                              {!isDone && <i className="wb-pulse-dot" aria-hidden="true" />}
                              {child.state}
                            </span>
                            <strong>{child.childRunId}</strong>
                          </div>
                          {child.task && <p>{child.task}</p>}
                          <p>
                            {child.outcome || "尚无完成结论"} ·{" "}
                            {child.usageReported
                              ? `${child.toolCalls} 次工具调用 · 输入 ${child.promptTokens.toLocaleString()} / 输出 ${child.completionTokens.toLocaleString()} tokens`
                              : "用量未回报"}
                          </p>
                          <button
                            type="button"
                            className="wb-secondary"
                            onClick={() => openChild(child.childRunId)}
                          >
                            查看子运行
                          </button>
                        </div>
                      );
                    })}
                  </div>
                ) : (
                  <div className="wb-empty">当前 Run 没有已记录的子智能体委派。</div>
                )}
              </div>
              {sessionTree && (sessionTree.nodes[runId]?.children?.length ?? 0) > 0 && (
                <>
                  <div className="wb-panel-separator" />
                  <div className="wb-label">后续与分支运行</div>
                  <div className="wb-tree-children">
                    {sessionTree.nodes[runId]?.children?.map((childId) => {
                      const childNode = sessionTree.nodes[childId];
                      return (
                        <div className="wb-tree-card" key={childId}>
                          <div>
                            <span className="wb-badge is-muted">
                              {childNode?.phase || childNode?.state || "关联"}
                            </span>
                            <strong>{childId}</strong>
                          </div>
                          {childNode?.task && <p>{childNode.task}</p>}
                          <button
                            type="button"
                            className="wb-secondary"
                            onClick={() => openChild(childId)}
                          >
                            打开关联 Run
                          </button>
                        </div>
                      );
                    })}
                  </div>
                </>
              )}
            </div>
          )}
          {panel === "workbench" && (
            <div className="wb-panel-body">
              <div className="wb-label">当前运行</div>
              <h2>{selected?.title || "Agent 任务"}</h2>
              <p>{statusName(selected)}</p>
              <div className="wb-panel-separator" />
              <div className="wb-label">工作区变更</div>
              <p>{changes ? changes.entries.length + " 个文件（工作区实时状态）" : "读取中…"}</p>
              {changes && (
                <p data-testid="change-summary">
                  新增 {changes.added} · 修改 {changes.modified} · 删除 {changes.deleted} · 未跟踪{" "}
                  {changes.untracked}
                </p>
              )}
              <button
                type="button"
                className="wb-secondary"
                onClick={() => void openPanel("changes")}
              >
                检查变更 →
              </button>
            </div>
          )}
          {panel === "changes" && (
            <div className="wb-change-layout">
              <div className="wb-file-list">
                {changes?.entries.map((item) => (
                  <button
                    type="button"
                    key={item.relative}
                    className={selectedFile === item.relative ? "current" : ""}
                    onClick={() => void chooseFile(item.relative)}
                  >
                    <span>{item.status}</span>
                    <span>{item.relative}</span>
                    <em className={"wb-owner-tag " + (item.agent_owned ? "is-agent" : "is-local")}>
                      {item.agent_owned ? "Agent 修改" : "本地已有"}
                    </em>
                  </button>
                ))}
                {changes?.entries.length === 0 && <div className="wb-hint">当前没有文件变更。</div>}
              </div>
              <div className="wb-diff">
                <div className="wb-diff-title">{selectedFile || "请选择文件"}</div>
                {selectedFile ? (
                  <DiffViewer
                    text={diff}
                    relative={selectedFile}
                    workspace={workspace}
                    onComment={async (line, body) => {
                      await desktop.saveReviewNote({ workspace, relative: selectedFile, line, body });
                      setDraft((old) =>
                        old
                          ? old + "\n\n审查意见：" + selectedFile + ":" + line + " — " + body
                          : "请根据这条代码审查意见进一步检查：" +
                            selectedFile +
                            ":" +
                            line +
                            " — " +
                            body,
                      );
                      setPanel("closed");
                      setExpandedDiff(false);
                      setNotice("已保存行级审查备注，意见已带入当前会话输入框；尚未执行修改。");
                    }}
                  />
                ) : (
                  <div className="wb-empty">选择文件后查看变更。</div>
                )}
              </div>
            </div>
          )}
          {panel === "evidence" && (
            <div className="wb-panel-body">
              {!evidence ? (
                <p>暂无已读取的证据。</p>
              ) : (
                <>
                  <div
                    className={
                      "wb-trust-banner " +
                      (evidence.trusted && evidence.verification?.verification_ok
                        ? "is-trusted"
                        : "is-unverified")
                    }
                  >
                    <div>
                      <div className="wb-label">
                        运行终态：{evidence.state_label || evidence.state}
                        {evidence.outcome_label || evidence.outcome
                          ? ` · ${evidence.outcome_label || evidence.outcome}`
                          : ""}
                      </div>
                      <strong>
                        验证可信度：{evidence.trust_label || (evidence.trusted ? "可信" : "未确认")}
                        {evidence.verification
                          ? ` (verification_ok: ${String(Boolean(evidence.verification.verification_ok))}${evidence.verification.verification_seq ? ` · seq #${evidence.verification.verification_seq}` : ""})`
                          : ""}
                      </strong>
                      <p>运行完成与验证可信严格分离；无 Verifier 证据绝不宣称已验证。</p>
                    </div>
                  </div>
                  <div className="wb-metric-grid">
                    <div>
                      <span>输入 Token</span>
                      <strong>{(evidence.metrics?.input_tokens ?? 0).toLocaleString()}</strong>
                    </div>
                    <div>
                      <span>输出 Token</span>
                      <strong>{(evidence.metrics?.output_tokens ?? 0).toLocaleString()}</strong>
                    </div>
                    <div>
                      <span>回合 / 工具调用</span>
                      <strong>
                        {evidence.metrics?.turns ?? 0} 回合 · {evidence.metrics?.tool_calls ?? 0} 次调用
                      </strong>
                    </div>
                    <div>
                      <span>上下文压缩</span>
                      <strong>{evidence.metrics?.context_compactions ?? compactions.length} 次</strong>
                    </div>
                  </div>
                  <p className="wb-hint">
                    {evidence.metrics?.usage_estimated
                      ? "以上用量包含估算数据。"
                      : "用量来自已记录的运行证据。"}
                    费用不可用时不做推断。
                  </p>
                </>
              )}
              {compactions.length > 0 && (
                <>
                  <div className="wb-panel-separator" />
                  <div className="wb-label">上下文压缩检查点</div>
                  {compactions.map((item) => (
                    <div className="wb-compaction-card" key={item.seq}>
                      <div>
                        <strong>
                          #{item.seq} · 触发 {item.trigger}
                        </strong>
                        <span className="wb-badge is-accent">{item.strategy}</span>
                      </div>
                      <p>
                        {item.unitsBefore !== null || item.unitsAfter !== null
                          ? `估算单位 ${item.unitsBefore ?? "—"} → ${item.unitsAfter ?? "—"} · `
                          : ""}
                        折叠 {item.compactedTurns} 个回合
                        {item.lastProviderPromptTokens > 0
                          ? ` · 最近 Prompt ${item.lastProviderPromptTokens.toLocaleString()} tokens`
                          : ""}
                      </p>
                      {item.summaryPreview && <p>{item.summaryPreview}</p>}
                    </div>
                  ))}
                </>
              )}
              <div className="wb-panel-separator" />
              <div className="wb-label">Event Journal 时间线</div>
              <div className="wb-timeline-filters" role="group" aria-label="时间线筛选">
                {(
                  [
                    ["all", "全部"],
                    ["tool", "工具"],
                    ["subagent", "子运行"],
                    ["compaction", "压缩"],
                  ] as const
                ).map(([kind, label]) => (
                  <button
                    key={kind}
                    type="button"
                    className={timelineKind === kind ? "current" : ""}
                    onClick={() => setTimelineKind(kind)}
                  >
                    {label}
                  </button>
                ))}
              </div>
              {filteredTimeline.length > 0 ? (
                <div className="wb-timeline-list">
                  {filteredTimeline.map((item) => {
                    const hash = eventHashBySeq.get(item.seq);
                    return (
                      <div className={`wb-timeline-item tone-${item.tone}`} key={item.id}>
                        <div>
                          <strong>
                            #{item.seq} · {item.label}
                          </strong>
                          {hash && (
                            <span className="wb-hash-pill" title={hash}>
                              {hash.slice(0, 10)}
                            </span>
                          )}
                        </div>
                        {item.detail && <p>{item.detail}</p>}
                      </div>
                    );
                  })}
                </div>
              ) : (
                <p className="wb-hint">当前筛选条件下暂无已记录的领域事件。</p>
              )}
            </div>
          )}
        </aside>
      )}
      {page === "home" && runId && (
        <nav className="wb-right-dock" aria-label="工作台面板导航">
          <button
            type="button"
            className={"wb-dock-btn" + (panel === "workbench" ? " current" : "")}
            data-tooltip="工作台"
            aria-label="工作台"
            aria-pressed={panel === "workbench"}
            onClick={() => void openPanel("workbench")}
          >
            <svg
              width="16"
              height="16"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.45"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <rect x="2" y="2.5" width="5" height="5" rx="1.2" />
              <rect x="9" y="2.5" width="5" height="5" rx="1.2" />
              <rect x="2" y="9" width="12" height="4.5" rx="1.2" />
            </svg>
          </button>
          <button
            type="button"
            className={"wb-dock-btn" + (panel === "changes" ? " current" : "")}
            data-tooltip="检查变更"
            aria-label="检查变更"
            aria-pressed={panel === "changes"}
            onClick={() => void openPanel("changes")}
          >
            <svg
              width="16"
              height="16"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.45"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <circle cx="4.5" cy="4.2" r="1.7" />
              <circle cx="4.5" cy="11.8" r="1.7" />
              <circle cx="11.5" cy="8" r="1.7" />
              <path d="M4.5 5.9v4.2M6.1 4.6c2.2 0 2.6 3.4 3.7 3.4" />
            </svg>
            {changes && changes.entries.length > 0 && (
              <span className="wb-dock-badge" aria-hidden="true">
                {changes.entries.length}
              </span>
            )}
          </button>
          <button
            type="button"
            className={"wb-dock-btn" + (panel === "evidence" ? " current" : "")}
            data-tooltip="验证证据"
            aria-label="验证证据"
            aria-pressed={panel === "evidence"}
            onClick={() => void openPanel("evidence")}
          >
            <svg
              width="16"
              height="16"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.45"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <path d="M8 2.2 12.8 4v3.8c0 3.1-2.1 5.1-4.8 6-2.7-.9-4.8-2.9-4.8-6V4L8 2.2Z" />
              <path d="m6 8.1 1.4 1.4L10.3 6.6" />
            </svg>
            {evidence && (
              <span
                className={
                  "wb-dock-badge " +
                  (evidence.trusted && evidence.verification?.verification_ok ? "is-ok" : "is-warn")
                }
                aria-hidden="true"
              >
                {evidence.trusted && evidence.verification?.verification_ok ? "✓" : "…"}
              </span>
            )}
          </button>
          <button
            type="button"
            className={"wb-dock-btn" + (panel === "subagents" ? " current" : "")}
            data-tooltip="子智能体"
            aria-label="子智能体"
            aria-pressed={panel === "subagents"}
            onClick={() => void openPanel("subagents")}
          >
            <svg
              width="16"
              height="16"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.45"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <circle cx="8" cy="3.5" r="1.6" />
              <circle cx="4" cy="12" r="1.6" />
              <circle cx="12" cy="12" r="1.6" />
              <path d="M8 5.1v2.6M8 7.7 4.6 10.5M8 7.7l3.4 2.8" />
            </svg>
            {subagents.length > 0 && (
              <span className="wb-dock-badge" aria-hidden="true">
                {subagents.length}
              </span>
            )}
          </button>
          <button
            type="button"
            className={"wb-dock-btn" + (panel === "queue" ? " current" : "")}
            data-tooltip="队列"
            aria-label="队列"
            aria-pressed={panel === "queue"}
            onClick={() => void openPanel("queue")}
          >
            <svg
              width="16"
              height="16"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.45"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <path d="M2.5 4.2h11M2.5 8h11M2.5 11.8h7" />
              <path d="m11.5 10.3 2 1.5-2 1.5" />
            </svg>
            {pendingQueueCount > 0 && (
              <span className="wb-dock-badge is-warn" aria-hidden="true">
                {pendingQueueCount}
              </span>
            )}
          </button>
          <button
            type="button"
            className={"wb-dock-btn" + (panel === "files" ? " current" : "")}
            data-tooltip="文件"
            aria-label="文件"
            aria-pressed={panel === "files"}
            onClick={() => void openPanel("files")}
          >
            <svg
              width="16"
              height="16"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.45"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <path d="M2.3 4.3c0-.7.5-1.3 1.2-1.3h2.6l1.3 1.5h5.1c.7 0 1.2.5 1.2 1.2v6.1c0 .7-.5 1.2-1.2 1.2H3.5c-.7 0-1.2-.5-1.2-1.2V4.3Z" />
            </svg>
          </button>
        </nav>
      )}
      </div>
        </>
      )}
    </div>
  );
}
