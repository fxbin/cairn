/**
 * Cairn Studio GUI：主工作台（Workbench）入口。
 * 只消费现有 desktop 桥接与 Journal 投影，不复制任何 Agent 运行逻辑。
 * Author: fxbin
 */
import {
  isValidElement,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { desktop } from "./desktop/client";
import { bridgeMode, demoBridgeActive } from "./desktop/wails-api";
import { ReconciliationPanel, AbortRunPanel } from "./workbench/RecoveryPanel";
import CapabilitySettings, {
  type CapabilityCounts,
  type CapabilitySection,
} from "./workbench/CapabilitySettings";
import CommandPalette from "./workbench/CommandPalette";
import ProviderSettings from "./workbench/ProviderSettings";
import UsageView from "./workbench/UsageView";
import OpenLocationIcon from "./workbench/OpenLocationIcon";
import SessionRow from "./workbench/SessionRow";
import SessionRecallRail, {
  buildSessionRecallStops,
  makeHeadingRecallId,
} from "./workbench/SessionRecallRail";
import SidecarWorkbench, {
  buildBuiltinSidecarTab,
  buildFileSidecarTab,
  type SidecarTab,
  type SidecarTabKind,
} from "./workbench/SidecarWorkbench";
import WorkflowsView from "./workbench/WorkflowsView";
import {
  WorkflowBlueprintCard,
  WorkflowExecutionCard,
} from "./workbench/WorkflowChatCards";
import {
  expandTaskWithWorkflowProtocol,
  loadWorkflowBindings,
  parseCairnWorkflowBlock,
  resolveWorkflowExecution,
  saveWorkflowBindings,
  stripWorkflowProtocolSuffix,
  type WorkflowBlueprintSpec,
  type WorkflowComposeContext,
  type WorkflowDefinitionItem,
  type WorkflowSessionBinding,
} from "./workbench/workflowConversation";
import {
  applyMentionSelection,
  detectComposerTrigger,
  expandComposerTaskWithContext,
  filterSlashCommands,
  flattenWorkspaceFiles,
  matchWorkspaceFiles,
  parseUserMessageAttachments,
  type AttachedFileChip,
  type ComposerActiveTrigger,
  type ComposerSlashCommand,
} from "./workbench/composerContext";
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
  deriveLiveRunVital,
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

type Panel = "closed" | SidecarTabKind;
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

/** 从 ReactMarkdown 代码块子节点递归提取纯文本源码供一键复制使用。 */
function extractMarkdownCodeText(node: ReactNode): string {
  if (typeof node === "string" || typeof node === "number") {
    return String(node);
  }
  if (Array.isArray(node)) {
    return node.map(extractMarkdownCodeText).join("");
  }
  if (isValidElement<{ children?: ReactNode }>(node)) {
    return extractMarkdownCodeText(node.props.children);
  }
  return "";
}

/** 从 ReactMarkdown pre 子节点提取围栏代码块语言标识与源码文本。 */
function inspectMarkdownPreBlock(children: ReactNode): { lang: string; code: string } {
  const firstChild = Array.isArray(children) ? children[0] : children;
  if (isValidElement<{ className?: string; children?: ReactNode }>(firstChild)) {
    const className = typeof firstChild.props.className === "string" ? firstChild.props.className : "";
    const langMatch = /language-([A-Za-z0-9_+-]+)/.exec(className);
    return {
      lang: langMatch?.[1] || "",
      code: extractMarkdownCodeText(firstChild.props.children).replace(/\n$/, ""),
    };
  }
  return {
    lang: "",
    code: extractMarkdownCodeText(children).replace(/\n$/, ""),
  };
}

/** 渲染对话时间线，包含引用文件胶囊、代码块复制栏、工作流草案卡、探索聚合组、补丁卡、知情审批门禁与召回锚点。 */
function Transcript({
  blocks,
  flashedAnchorId,
  workflows,
  onApproval,
  onInspectFile,
  onSaveWorkflow,
  onSaveAndRunWorkflow,
  onRefineWorkflow,
  onOpenWorkflowPortal,
}: {
  blocks: ChatBlock[];
  flashedAnchorId?: string | null;
  workflows: readonly WorkflowDefinitionItem[];
  onApproval: (approve: boolean, fingerprint: string, runId: string) => void;
  onInspectFile: (targetPath: string) => void;
  onSaveWorkflow: (spec: WorkflowBlueprintSpec) => Promise<void>;
  onSaveAndRunWorkflow: (spec: WorkflowBlueprintSpec) => Promise<void>;
  onRefineWorkflow: (spec: WorkflowBlueprintSpec) => void;
  onOpenWorkflowPortal: (workflowId?: string) => void;
}) {
  const grouped = useMemo(() => groupTranscriptBlocks(blocks), [blocks]);
  const renderRecallHeading =
    (Tag: "h1" | "h2" | "h3", blockId: string) =>
    ({ children }: { children?: ReactNode }) => {
      const text = extractMarkdownCodeText(children).trim();
      const anchor = text ? makeHeadingRecallId(blockId, text) : undefined;
      return (
        <Tag
          data-recall-anchor={anchor}
          className={anchor && flashedAnchorId === anchor ? "is-recall-flash" : undefined}
        >
          {children}
        </Tag>
      );
    };

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
        const flashCls = flashedAnchorId === block.id ? " is-recall-flash" : "";
        if (block.kind === "user") {
          const strippedWorkflow = stripWorkflowProtocolSuffix(block.text);
          const parsedUser = parseUserMessageAttachments(strippedWorkflow.body);
          return (
            <div
              className={"wb-message-user" + flashCls}
              data-recall-anchor={block.id}
              key={block.id}
            >
              {strippedWorkflow.hasWorkflowProtocol && (
                <div className="wb-user-wf-pill">
                  <span aria-hidden="true">☍</span> 工作流编排
                </div>
              )}
              {parsedUser.refs.length > 0 && (
                <div className="wb-user-refs">
                  {parsedUser.refs.map((ref) => (
                    <details className="wb-user-ref-chip" key={ref.path}>
                      <summary>
                        <span aria-hidden="true">@</span>
                        <span>{ref.path}</span>
                      </summary>
                      <pre>{ref.snippet}</pre>
                    </details>
                  ))}
                </div>
              )}
              <div className="wb-message-user-body">{parsedUser.body}</div>
            </div>
          );
        }
        if (block.kind === "agent") {
          return (
            <article
              className={"wb-message-agent" + flashCls}
              data-testid="assistant-body"
              data-recall-anchor={block.id}
              key={block.id}
            >
              <ReactMarkdown
                remarkPlugins={[remarkGfm]}
                skipHtml
                components={{
                  h1: renderRecallHeading("h1", block.id),
                  h2: renderRecallHeading("h2", block.id),
                  h3: renderRecallHeading("h3", block.id),
                  pre({ children }) {
                    const { lang, code } = inspectMarkdownPreBlock(children);
                    const normalizedLang = lang.toLowerCase();
                    if (normalizedLang === "cairn-workflow" || normalizedLang === "workflow") {
                      const wfSpec = parseCairnWorkflowBlock(code);
                      if (wfSpec) {
                        const matched =
                          workflows.find((w) => wfSpec.id && w.id === wfSpec.id) ||
                          workflows.find((w) => w.name === wfSpec.name);
                        return (
                          <WorkflowBlueprintCard
                            spec={wfSpec}
                            rawJson={code}
                            matchedExistingId={matched?.id}
                            onSave={onSaveWorkflow}
                            onSaveAndRun={onSaveAndRunWorkflow}
                            onRefine={onRefineWorkflow}
                            onOpenPortal={onOpenWorkflowPortal}
                          />
                        );
                      }
                    }
                    return (
                      <div className="wb-md-codeblock">
                        <div className="wb-md-codehead">
                          <span className="wb-md-codelang">{lang || "code"}</span>
                          <SettingsCopyButton
                            value={code}
                            ariaLabel={`复制 ${lang || "代码"} 代码块`}
                          />
                        </div>
                        <pre>{children}</pre>
                      </div>
                    );
                  },
                }}
              >
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
              <div
                className={"wb-patch-card" + flashCls}
                data-recall-anchor={block.id}
                key={block.id}
              >
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
          const proposal = block.proposal;
          return (
            <section
              className={"wb-approval" + flashCls}
              data-testid="approval-card"
              data-recall-anchor={block.id}
              key={block.id}
            >
              <div className="wb-approval-top">
                <div className="wb-label">等待你的授权</div>
                {block.operationId && (
                  <span className="wb-badge is-warn">#{block.operationId}</span>
                )}
              </div>
              <strong>{block.summary}</strong>
              <p>此操作具有副作用。仅批准当前指纹对应的操作，不会扩大后续运行权限。</p>
              {proposal && (
                <div className="wb-approval-proposal" data-testid="approval-proposal">
                  <div className="wb-approval-proposal-head">
                    <span className="wb-badge is-muted">{proposal.toolName}</span>
                    {proposal.mode && <span className="wb-badge is-accent">{proposal.mode}</span>}
                    {proposal.targetPath && (
                      <code className="wb-approval-path">{proposal.targetPath}</code>
                    )}
                    {proposal.expectedSha && (
                      <span className="wb-hash-pill" title="基线文件 SHA-256 摘要">
                        基线 {proposal.expectedSha}
                      </span>
                    )}
                    {proposal.targetPath && (
                      <button
                        type="button"
                        className="wb-approval-inspect-btn"
                        onClick={() => onInspectFile(proposal.targetPath!)}
                      >
                        审查 Diff →
                      </button>
                    )}
                  </div>
                  {proposal.commandArgv && (
                    <div className="wb-approval-argv">
                      <span className="wb-approval-argv-tag">argv（无 Shell 直传）</span>
                      <code>{proposal.commandArgv}</code>
                    </div>
                  )}
                  {proposal.patchLines && proposal.patchLines.length > 0 && (
                    <div className="wb-approval-patch" aria-label="提议变更预览">
                      {proposal.patchLines.map((line, lineIdx) => (
                        <div
                          key={lineIdx}
                          className={"wb-approval-patch-line is-" + line.kind}
                        >
                          {line.text}
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              )}
              <details>
                <summary>查看操作指纹</summary>
                <code className="wb-approval-code">{block.fingerprint}</code>
              </details>
              {block.resolved ? (
                <div className="wb-approval-resolved-row">
                  <div className="wb-label">
                    {block.resolved === "approved" ? "已授权" : "已拒绝"}
                  </div>
                  <span className="wb-approval-resolved-note">
                    {block.resolved === "approved" ? "已允许并继续" : "已拒绝执行"}
                  </span>
                </div>
              ) : (
                <div className="wb-actions">
                  <button
                    type="button"
                    className="wb-primary"
                    data-testid="approval-allow"
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
  const [sidecarTabs, setSidecarTabs] = useState<SidecarTab[]>(() => [
    buildBuiltinSidecarTab("workbench"),
  ]);
  const [activeSidecarTabId, setActiveSidecarTabId] = useState("workbench");
  const [omniboxFocusRev, setOmniboxFocusRev] = useState(0);
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
  const [searchOpen, setSearchOpen] = useState(false);
  const [projectCollapsed, setProjectCollapsed] = useState(false);
  const [heroPopover, setHeroPopover] = useState<HeroPopover>("none");
  const [editorMenuOpen, setEditorMenuOpen] = useState(false);
  const [activityTab, setActivityTab] = useState<ActivityTab>("heatmap");
  const [providerPresets, setProviderPresets] = useState<ProviderPresetView[]>([]);
  const [openLocations, setOpenLocations] = useState<OpenLocationItem[]>([]);
  const [forkSource, setForkSource] = useState<string | null>(null);
  const [parentRun, setParentRun] = useState<string | null>(null);
  const [sessionTree, setSessionTree] = useState<SessionTreeView | null>(null);
  const [attachedFiles, setAttachedFiles] = useState<AttachedFileChip[]>([]);
  const [workspaceFiles, setWorkspaceFiles] = useState<AttachedFileChip[] | null>(null);
  const [loadingWorkspaceFiles, setLoadingWorkspaceFiles] = useState(false);
  const [composerTrigger, setComposerTrigger] = useState<ComposerActiveTrigger | null>(null);
  const [composerMenuIndex, setComposerMenuIndex] = useState(0);
  const [commandPaletteOpen, setCommandPaletteOpen] = useState(false);
  const [showScrollBottom, setShowScrollBottom] = useState(false);
  const [ancestorDetails, setAncestorDetails] = useState<RunDetail[]>([]);
  const [activeRecallId, setActiveRecallId] = useState<string | null>(null);
  const [flashedRecallId, setFlashedRecallId] = useState<string | null>(null);
  const [workflows, setWorkflows] = useState<WorkflowDefinitionItem[]>([]);
  const [workflowComposeCtx, setWorkflowComposeCtx] = useState<WorkflowComposeContext | null>(null);
  const [focusedWorkflowId, setFocusedWorkflowId] = useState<string | null>(null);
  const [workflowBindings, setWorkflowBindings] = useState<WorkflowSessionBinding[]>([]);
  const [sidebarOpen, setSidebarOpen] = useState(
    () => typeof window === "undefined" || window.innerWidth > 760,
  );
  // 世代以状态镜像：重选同一工作区时路径不变，轮询与流订阅仍须随世代重建，否则旧闭包守卫会永久丢弃新投影。
  const [epoch, setEpoch] = useState(0);
  const projectRef = useRef("");
  const generationRef = useRef(0);
  const runRef = useRef<string | null>(null);
  const ancestorCacheRef = useRef<Map<string, RunDetail>>(new Map());
  const autoFollowWorkflowRunRef = useRef<string | null>(null);
  const flashTimerRef = useRef<number | null>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const sessionScrollRef = useRef<HTMLDivElement>(null);
  const autoFollowRef = useRef(true);
  const composerTextareaRef = useRef<HTMLTextAreaElement>(null);
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
  const shortcutHandlerRef = useRef<(event: KeyboardEvent) => void>(() => {});

  useEffect(() => {
    return () => {
      if (stepperBumpTimerRef.current !== null) {
        window.clearTimeout(stepperBumpTimerRef.current);
      }
      if (flashTimerRef.current !== null) {
        window.clearTimeout(flashTimerRef.current);
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
      shortcutHandlerRef.current(event);
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

  const refreshWorkflows = useCallback(async (path: string, generation: number) => {
    if (!path || typeof desktop.listWorkflows !== "function") {
      setWorkflows([]);
      return [];
    }
    try {
      const items = await desktop.listWorkflows(path);
      const normalized = Array.isArray(items)
        ? items.map((w) => ({ ...w, steps: w.steps ?? [] }))
        : [];
      if (generation === generationRef.current && projectRef.current === path) {
        setWorkflows(normalized);
      }
      return normalized;
    } catch {
      if (generation === generationRef.current && projectRef.current === path) {
        setWorkflows([]);
      }
      return [];
    }
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

  const refreshChanges = useCallback((path: string, generation: number) => {
    if (!path || typeof desktop.listChanges !== "function") {
      setChanges(null);
      return;
    }
    void Promise.resolve(desktop.listChanges(path))
      .then((result) => {
        if (!result || generation !== generationRef.current || projectRef.current !== path) return;
        setChanges(result);
      })
      .catch(() => {
        // 静默忽略后台变更概览预刷新错误，显式打开面板时再走错误提示。
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
      setAttachedFiles([]);
      setWorkspaceFiles(null);
      setLoadingWorkspaceFiles(false);
      setComposerTrigger(null);
      setComposerMenuIndex(0);
      setShowScrollBottom(false);
      setAncestorDetails([]);
      ancestorCacheRef.current.clear();
      setActiveRecallId(null);
      setFlashedRecallId(null);
      setWorkflowComposeCtx(null);
      setFocusedWorkflowId(null);
      setProjectCollapsed(false);
      setWorkflows([]);
      setWorkflowBindings(loadWorkflowBindings(path));
      autoFollowWorkflowRunRef.current = null;
      autoFollowRef.current = true;
      setChanges(null);
      setSelectedFile("");
      setDiff("");
      setEvidence(null);
      setTimelineKind("all");
      setCapabilityCounts(null);
      setDoctorRunning(false);
      setDoctorReport(null);
      setDoctorError("");
      setSidecarTabs([buildBuiltinSidecarTab("workbench")]);
      setActiveSidecarTabId("workbench");
      setExpandedDiff(false);
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
        refreshChanges(path, generation);
        void refreshWorkflows(path, generation);
        if (typeof desktop.rememberProject === "function") {
          void desktop.rememberProject(path);
        }
      } catch (err) {
        if (generation === generationRef.current) setNotice(textError(err));
      }
    },
    [refreshQueueCount, refreshPresets, refreshOpenLocations, refreshChanges, refreshWorkflows],
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
    autoFollowRef.current = true;
    setShowScrollBottom(false);
    setAncestorDetails([]);
    setActiveRecallId(null);
    setFlashedRecallId(null);
    setComposerTrigger(null);
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

  useEffect(() => {
    if (!workspace || !detail?.state) return;
    refreshChanges(workspace, generationRef.current);
  }, [workspace, detail?.state, refreshChanges]);

  const selected = runs.find((run) => run.run_id === runId);

  const activeWorkflowExecution = useMemo(() => {
    if (!runId || workflowBindings.length === 0) return null;
    for (const binding of workflowBindings) {
      const exec = resolveWorkflowExecution(binding, runs);
      const matchesRun =
        binding.firstRunId === runId ||
        (Boolean(binding.sessionId) && selected?.session_id === binding.sessionId) ||
        exec.stages.some((stage) => stage.run?.run_id === runId);
      if (matchesRun) {
        return exec;
      }
    }
    return null;
  }, [runId, runs, selected?.session_id, workflowBindings]);

  useEffect(() => {
    if (!workspace || !activeWorkflowExecution) return;
    const discoveredSession = activeWorkflowExecution.sessionId;
    if (
      discoveredSession &&
      activeWorkflowExecution.binding.sessionId !== discoveredSession
    ) {
      setWorkflowBindings((prev) => {
        const next = prev.map((item) =>
          item.firstRunId === activeWorkflowExecution.binding.firstRunId
            ? { ...item, sessionId: discoveredSession }
            : item,
        );
        saveWorkflowBindings(workspace, next);
        return next;
      });
    }
    if (
      runId &&
      autoFollowWorkflowRunRef.current === runId &&
      activeWorkflowExecution.latestRunId !== runId
    ) {
      autoFollowWorkflowRunRef.current = activeWorkflowExecution.latestRunId;
      selectRun(activeWorkflowExecution.latestRunId);
    }
  }, [workspace, runId, activeWorkflowExecution, selectRun]);

  // 当选中运行已进入 completed 终态时后台静默读取一次 Verifier 证据，支撑对话末尾结案路标卡真实渲染。
  useEffect(() => {
    if (panel === "evidence" || !workspace || !runId || selected?.state !== "completed") return;
    if (typeof desktop.evidence !== "function") return;
    const currentWorkspace = workspace;
    const currentRunId = runId;
    void Promise.resolve(desktop.evidence({ workspace: currentWorkspace, run_id: currentRunId }))
      .then((result) => {
        if (projectRef.current === currentWorkspace && runRef.current === currentRunId) {
          setEvidence(result);
        }
      })
      .catch(() => {
        // 结案路标卡后台静默读取失败不污染告警横幅；显式打开证据标签页时仍正常提示。
      });
  }, [panel, workspace, runId, selected?.state]);

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

  const ancestorRunIds = useMemo(() => {
    if (!runId) return [] as string[];
    const prevMap = new Map<string, string>();
    for (const r of runs) {
      if (r.run_id && r.prev_run_id) {
        prevMap.set(r.run_id, r.prev_run_id);
      }
    }
    if (sessionTree?.nodes) {
      for (const [id, node] of Object.entries(sessionTree.nodes)) {
        if (node.prev_run_id && !prevMap.has(id)) {
          prevMap.set(id, node.prev_run_id);
        }
      }
    }
    const chain: string[] = [];
    const seen = new Set<string>([runId]);
    let cursor = prevMap.get(runId);
    let hops = 0;
    while (cursor && !seen.has(cursor) && hops < 40) {
      seen.add(cursor);
      chain.push(cursor);
      cursor = prevMap.get(cursor);
      hops += 1;
    }
    chain.reverse();
    return chain;
  }, [runId, runs, sessionTree]);

  useEffect(() => {
    if (!workspace || !runId || ancestorRunIds.length === 0) {
      setAncestorDetails([]);
      return;
    }
    let alive = true;
    const targetWorkspace = workspace;
    const targetRunId = runId;
    const generation = generationRef.current;

    const loadAncestors = async () => {
      const resolved = await Promise.all(
        ancestorRunIds.map(async (ancId) => {
          const cached = ancestorCacheRef.current.get(ancId);
          if (cached) return cached;
          try {
            const loaded = await desktop.inspectRun({
              workspace: targetWorkspace,
              run_id: ancId,
              include_events: true,
            });
            if (loaded && finished.has(loaded.state)) {
              ancestorCacheRef.current.set(ancId, loaded);
            }
            return loaded;
          } catch {
            return null;
          }
        }),
      );
      if (!alive || generation !== generationRef.current || runRef.current !== targetRunId) {
        return;
      }
      setAncestorDetails(resolved.filter((item): item is RunDetail => item !== null));
    };

    void loadAncestors();
    return () => {
      alive = false;
    };
  }, [workspace, runId, ancestorRunIds]);

  const blocks = useMemo(() => {
    const historyBlocks = ancestorDetails.flatMap((anc) =>
      buildChatBlocks(anc.events || []).map((item) => ({ ...item, id: `${anc.run_id}:${item.id}` })),
    );
    const projected = buildChatBlocks(detail?.events || [], liveEvents);
    if (sendingText && !projected.some((block) => block.kind === "user" && block.text === sendingText)) {
      projected.unshift({ kind: "user", id: "pending-user", text: sendingText, at: "" });
    }
    return historyBlocks.length > 0 ? [...historyBlocks, ...projected] : projected;
  }, [ancestorDetails, detail, liveEvents, sendingText]);

  const recallStops = useMemo(() => buildSessionRecallStops(blocks), [blocks]);

  const liveVital = useMemo(
    () => deriveLiveRunVital(detail?.events || [], liveEvents),
    [detail?.events, liveEvents],
  );

  const syncActiveRecallFromScroll = useCallback(() => {
    const el = sessionScrollRef.current;
    if (!el || recallStops.length < 2) {
      return;
    }
    const anchors = el.querySelectorAll<HTMLElement>("[data-recall-anchor]");
    if (anchors.length === 0) return;

    const anchorMap = new Map<string, HTMLElement>();
    anchors.forEach((node) => {
      const key = node.getAttribute("data-recall-anchor");
      if (key) anchorMap.set(key, node);
    });

    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
    if (distanceFromBottom <= 24) {
      setActiveRecallId(recallStops[recallStops.length - 1].id);
      return;
    }

    const containerTop = el.getBoundingClientRect().top;
    const threshold = containerTop + Math.min(180, Math.max(72, el.clientHeight * 0.32));
    let candidate = recallStops[0].id;
    for (const stop of recallStops) {
      const node = anchorMap.get(stop.id);
      if (!node) continue;
      if (node.getBoundingClientRect().top <= threshold) {
        candidate = stop.id;
      }
    }
    setActiveRecallId(candidate);
  }, [recallStops]);

  useEffect(() => {
    if (autoFollowRef.current && typeof bottomRef.current?.scrollIntoView === "function") {
      bottomRef.current.scrollIntoView({ block: "end", behavior: "auto" });
    }
    syncActiveRecallFromScroll();
  }, [blocks.length, runId, syncActiveRecallFromScroll]);

  const handleSessionScroll = () => {
    const el = sessionScrollRef.current;
    if (!el) return;
    const distance = el.scrollHeight - el.scrollTop - el.clientHeight;
    const nearBottom = distance <= 72;
    autoFollowRef.current = nearBottom;
    setShowScrollBottom(!nearBottom);
    syncActiveRecallFromScroll();
  };

  const jumpToRecallStop = (anchorId: string) => {
    const container = sessionScrollRef.current;
    if (!container) return;
    let target: HTMLElement | null = null;
    const nodes = container.querySelectorAll<HTMLElement>("[data-recall-anchor]");
    nodes.forEach((node) => {
      if (!target && node.getAttribute("data-recall-anchor") === anchorId) {
        target = node;
      }
    });
    if (!target) return;

    const isLastStop =
      recallStops.length > 0 && recallStops[recallStops.length - 1].id === anchorId;
    autoFollowRef.current = isLastStop;
    setShowScrollBottom(!isLastStop);
    setActiveRecallId(anchorId);
    setFlashedRecallId(anchorId);
    if (flashTimerRef.current !== null) {
      window.clearTimeout(flashTimerRef.current);
    }
    flashTimerRef.current = window.setTimeout(() => {
      setFlashedRecallId((curr) => (curr === anchorId ? null : curr));
      flashTimerRef.current = null;
    }, 1400);

    if (typeof (target as HTMLElement).scrollIntoView === "function") {
      (target as HTMLElement).scrollIntoView({ behavior: "smooth", block: "start" });
    }
  };

  const scrollToSessionBottom = () => {
    autoFollowRef.current = true;
    setShowScrollBottom(false);
    if (typeof bottomRef.current?.scrollIntoView === "function") {
      bottomRef.current.scrollIntoView({ block: "end", behavior: "smooth" });
    }
  };

  const ensureWorkspaceFilesLoaded = useCallback(() => {
    if (
      !workspace ||
      workspaceFiles !== null ||
      loadingWorkspaceFiles ||
      typeof desktop.listFileTree !== "function"
    ) {
      return;
    }
    const targetWorkspace = workspace;
    const generation = generationRef.current;
    setLoadingWorkspaceFiles(true);
    void Promise.resolve(desktop.listFileTree(targetWorkspace))
      .then((tree) => {
        if (generation === generationRef.current && projectRef.current === targetWorkspace) {
          setWorkspaceFiles(flattenWorkspaceFiles(tree));
        }
      })
      .catch(() => {
        if (generation === generationRef.current && projectRef.current === targetWorkspace) {
          setWorkspaceFiles([]);
        }
      })
      .finally(() => {
        if (generation === generationRef.current && projectRef.current === targetWorkspace) {
          setLoadingWorkspaceFiles(false);
        }
      });
  }, [workspace, workspaceFiles, loadingWorkspaceFiles]);

  useEffect(() => {
    if (composerTrigger?.kind === "mention") {
      ensureWorkspaceFilesLoaded();
    }
  }, [composerTrigger?.kind, ensureWorkspaceFilesLoaded]);

  useLayoutEffect(() => {
    const ta = composerTextareaRef.current;
    if (!ta) return;
    ta.style.height = "auto";
    const minHeight = runId ? 58 : 76;
    const nextHeight = Math.min(240, Math.max(minHeight, ta.scrollHeight || minHeight));
    ta.style.height = `${nextHeight}px`;
  }, [draft, runId, page]);

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
    const pendingChips = attachedFiles;
    const pendingWorkflowCtx = workflowComposeCtx;
    let clearedDraft = false;
    setSending(true);
    try {
      const expandedRefsTask = await expandComposerTaskWithContext({
        workspace,
        rawTask: content,
        attachedFiles: pendingChips,
        previewFile: typeof desktop.previewFile === "function" ? desktop.previewFile : undefined,
      });
      const expandedTask = expandTaskWithWorkflowProtocol(
        expandedRefsTask,
        pendingWorkflowCtx,
        workflows,
      );
      if (generation !== generationRef.current) return;
      clearedDraft = true;
      setSendingText(expandedTask);
      setDraft("");
      setAttachedFiles([]);
      setWorkflowComposeCtx(null);
      setComposerTrigger(null);
      const ack = await desktop.startRun({
        workspace,
        task: expandedTask,
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
        if (clearedDraft) {
          setDraft((value) => (value ? content + "\n" + value : content));
          setAttachedFiles((prev) => {
            const seen = new Set(prev.map((item) => item.relative));
            const restored = pendingChips.filter((item) => !seen.has(item.relative));
            return restored.length > 0 ? [...restored, ...prev] : prev;
          });
          if (pendingWorkflowCtx) {
            setWorkflowComposeCtx(pendingWorkflowCtx);
          }
        }
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

  const loadSidecarDataForKind = async (kind: SidecarTabKind) => {
    if (!workspace) return;
    const request = ++diffRequestRef.current;
    try {
      if ((kind === "changes" || kind === "workbench") && typeof desktop.listChanges === "function") {
        const result = await desktop.listChanges(workspace);
        if (!result || projectRef.current !== workspace || request !== diffRequestRef.current) return;
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
      if (kind === "evidence" && runRef.current && typeof desktop.evidence === "function") {
        const activeRun = runRef.current;
        const result = await desktop.evidence({ workspace, run_id: activeRun });
        if (projectRef.current === workspace && runRef.current === activeRun) {
          setEvidence(result);
        }
      }
    } catch (err) {
      setNotice(textError(err));
    }
  };

  const activateBuiltinTab = async (kind: Exclude<SidecarTabKind, "file">) => {
    const tab = buildBuiltinSidecarTab(kind);
    setSidecarTabs((prev) => (prev.some((item) => item.id === tab.id) ? prev : [...prev, tab]));
    setActiveSidecarTabId(tab.id);
    setPanel(kind);
    await loadSidecarDataForKind(kind);
  };

  const openPanel = async (next: Exclude<SidecarTabKind, "file">) => {
    if (panel !== "closed" && activeSidecarTabId === next) {
      setPanel("closed");
      setExpandedDiff(false);
      return;
    }
    await activateBuiltinTab(next);
  };

  const toggleSidecar = () => {
    if (!projectRef.current) return;
    if (panel !== "closed") {
      setPanel("closed");
      setExpandedDiff(false);
      return;
    }
    const currentTab =
      sidecarTabs.find((item) => item.id === activeSidecarTabId) ||
      sidecarTabs[0] ||
      buildBuiltinSidecarTab("workbench");
    if (!sidecarTabs.some((item) => item.id === currentTab.id)) {
      setSidecarTabs([currentTab]);
    }
    setActiveSidecarTabId(currentTab.id);
    setPanel(currentTab.kind);
    if (currentTab.kind !== "file") {
      void loadSidecarDataForKind(currentTab.kind);
    }
  };

  const openFileTab = (relativePath: string) => {
    const clean = relativePath.trim();
    if (!clean) return;
    const tab = buildFileSidecarTab(clean);
    setSidecarTabs((prev) => (prev.some((item) => item.id === tab.id) ? prev : [...prev, tab]));
    setActiveSidecarTabId(tab.id);
    setPanel("file");
  };

  const activateSidecarTab = (tabId: string) => {
    const target = sidecarTabs.find((item) => item.id === tabId);
    if (!target) return;
    setActiveSidecarTabId(target.id);
    setPanel(target.kind);
    if (target.kind !== "file") {
      void loadSidecarDataForKind(target.kind);
    }
  };

  const closeSidecarTab = (tabId: string) => {
    const idx = sidecarTabs.findIndex((item) => item.id === tabId);
    if (idx < 0) return;
    const remaining = sidecarTabs.filter((item) => item.id !== tabId);
    if (remaining.length === 0) {
      const fallback = buildBuiltinSidecarTab("workbench");
      setSidecarTabs([fallback]);
      setActiveSidecarTabId(fallback.id);
      setPanel("closed");
      setExpandedDiff(false);
      return;
    }
    setSidecarTabs(remaining);
    if (activeSidecarTabId === tabId) {
      const nextTab = remaining[Math.min(idx, remaining.length - 1)];
      setActiveSidecarTabId(nextTab.id);
      setPanel(nextTab.kind);
      if (nextTab.kind !== "file") {
        void loadSidecarDataForKind(nextTab.kind);
      }
    }
  };

  const openNewLauncherTab = () => {
    void activateBuiltinTab("workbench");
    setOmniboxFocusRev((rev) => rev + 1);
  };

  const inspectChangedFile = async (targetPath: string) => {
    if (!workspace) return;
    const changesTab = buildBuiltinSidecarTab("changes");
    setSidecarTabs((prev) =>
      prev.some((item) => item.id === changesTab.id) ? prev : [...prev, changesTab],
    );
    setActiveSidecarTabId(changesTab.id);
    setPanel("changes");
    const request = ++diffRequestRef.current;
    try {
      const result = await desktop.listChanges(workspace);
      if (!result || projectRef.current !== workspace || request !== diffRequestRef.current) return;
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

  const saveDiffComment = async (line: string, body: string, relativeOverride?: string) => {
    const targetFile = relativeOverride || selectedFile;
    if (!targetFile) return;
    await desktop.saveReviewNote({ workspace, relative: targetFile, line, body });
    setDraft((old) =>
      old
        ? old + "\n\n审查意见：" + targetFile + ":" + line + " — " + body
        : "请根据这条代码审查意见进一步检查：" +
          targetFile +
          ":" +
          line +
          " — " +
          body,
    );
    setNotice("已保存行级审查备注，意见已带入当前会话输入框；尚未执行修改。");
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

  const focusComposerSoon = () => {
    window.setTimeout(() => {
      composerTextareaRef.current?.focus();
    }, 0);
  };

  const startConversationalWorkflowCreate = (initialPrompt?: string) => {
    setNeedsAttentionOnly(false);
    selectRun(null);
    setPage("home");
    setWorkflowComposeCtx({ mode: "create" });
    if (initialPrompt !== undefined) {
      setDraft(initialPrompt);
    }
    focusComposerSoon();
  };

  const startConversationalWorkflowEdit = (wf: WorkflowDefinitionItem, note?: string) => {
    setNeedsAttentionOnly(false);
    selectRun(null);
    setPage("home");
    setWorkflowComposeCtx({ mode: "edit", workflow: wf, note });
    setDraft((prev) =>
      prev.trim()
        ? prev
        : note
          ? `请帮我结合运行反馈调试并优化工作流「${wf.name}」：`
          : `我想调整工作流「${wf.name}」：`,
    );
    focusComposerSoon();
  };

  const handleSaveWorkflowSpec = async (
    spec: WorkflowBlueprintSpec,
  ): Promise<WorkflowDefinitionItem | undefined> => {
    if (!workspace || typeof desktop.saveWorkflow !== "function") return undefined;
    const generation = generationRef.current;
    try {
      await desktop.saveWorkflow({
        id: spec.id || undefined,
        workspace,
        name: spec.name,
        steps: spec.steps,
      });
      const updatedList = await refreshWorkflows(workspace, generation);
      const matched =
        (spec.id && updatedList.find((w) => w.id === spec.id)) ||
        updatedList.find((w) => w.name === spec.name) ||
        updatedList[0];
      if (matched) setFocusedWorkflowId(matched.id);
      setNotice(`已保存工作流「${spec.name}」；可随时在「工作流」目录只读检视或一键运行。`);
      return matched;
    } catch (err) {
      setNotice(textError(err));
      return undefined;
    }
  };

  const launchWorkflowInSession = async (wf: WorkflowDefinitionItem) => {
    if (!workspace || typeof desktop.runWorkflow !== "function") return;
    const generation = generationRef.current;
    try {
      const ack = await desktop.runWorkflow(workspace, wf.id, false);
      const firstRunId = ack.run_ids?.[0] || "";
      if (firstRunId) {
        const nextBinding: WorkflowSessionBinding = {
          workflowId: wf.id,
          workflowName: wf.name,
          workspace,
          firstRunId,
          steps: wf.steps,
          startedAt: new Date().toISOString(),
        };
        setWorkflowBindings((prev) => {
          const updated = [nextBinding, ...prev.filter((b) => b.firstRunId !== firstRunId)];
          saveWorkflowBindings(workspace, updated);
          return updated;
        });
        autoFollowWorkflowRunRef.current = firstRunId;
        selectRun(firstRunId);
        setPage("home");
      }
      await refreshRuns(workspace, generation);
    } catch (err) {
      setNotice(textError(err));
    }
  };

  const handleSaveAndRunWorkflowSpec = async (spec: WorkflowBlueprintSpec) => {
    const saved = await handleSaveWorkflowSpec(spec);
    if (!saved) return;
    await launchWorkflowInSession(saved);
  };

  const handleRefineWorkflowSpec = (spec: WorkflowBlueprintSpec) => {
    setWorkflowComposeCtx({
      mode: "edit",
      workflow: {
        id: spec.id || "",
        name: spec.name,
        steps: spec.steps,
      },
    });
    setDraft((prev) => (prev.trim() ? prev : `调整「${spec.name}」的步骤或规则：`));
    focusComposerSoon();
  };

  const stopWorkflowStageRun = (targetRunId: string) => {
    void desktop
      .cancelRun({ run_id: targetRunId, reason: "user stop" })
      .catch((err) => setNotice(textError(err)));
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

  shortcutHandlerRef.current = (event: KeyboardEvent) => {
    if (event.isComposing || event.keyCode === 229) return;
    const keyLower = event.key.toLowerCase();
    if ((event.metaKey || event.ctrlKey) && !event.shiftKey && keyLower === "k") {
      event.preventDefault();
      setCommandPaletteOpen((open) => !open);
      return;
    }
    if ((event.metaKey || event.ctrlKey) && event.key === ",") {
      event.preventDefault();
      setCommandPaletteOpen(false);
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
    if (page === "home" && workspace && !commandPaletteOpen) {
      if ((event.metaKey || event.ctrlKey) && !event.shiftKey && (keyLower === "b" || event.key === "\\")) {
        event.preventDefault();
        toggleSidecar();
        return;
      }
      if ((event.metaKey || event.ctrlKey) && !event.shiftKey && keyLower === "p") {
        event.preventDefault();
        openNewLauncherTab();
        return;
      }
      if (event.ctrlKey && event.shiftKey && keyLower === "g") {
        event.preventDefault();
        void activateBuiltinTab("changes");
        return;
      }
      if ((event.metaKey || event.ctrlKey) && !event.shiftKey && keyLower === "e" && runId) {
        event.preventDefault();
        void activateBuiltinTab("evidence");
        return;
      }
      if ((event.metaKey || event.ctrlKey) && event.shiftKey && keyLower === "s" && runId) {
        event.preventDefault();
        forkRun(runId);
        return;
      }
    }
    if (event.key === "Escape") {
      if (commandPaletteOpen) {
        setCommandPaletteOpen(false);
        return;
      }
      if (composerTrigger !== null) {
        setComposerTrigger(null);
        return;
      }
      if (heroPopover !== "none" || editorMenuOpen) {
        setHeroPopover("none");
        setEditorMenuOpen(false);
        return;
      }
      setSidebarOpen(false);
      setPage((current) => (current === "settings" ? "home" : current));
    }
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

  const slashMatches = useMemo(
    () => (composerTrigger?.kind === "slash" ? filterSlashCommands(composerTrigger.query) : []),
    [composerTrigger],
  );
  const mentionMatches = useMemo(
    () =>
      composerTrigger?.kind === "mention" && workspaceFiles
        ? matchWorkspaceFiles(workspaceFiles, composerTrigger.query, 8)
        : [],
    [composerTrigger, workspaceFiles],
  );
  const activePopoverCount =
    composerTrigger?.kind === "slash"
      ? slashMatches.length
      : composerTrigger?.kind === "mention"
        ? mentionMatches.length
        : 0;

  const executeComposerSlashCommand = (cmd: ComposerSlashCommand) => {
    setDraft("");
    setComposerTrigger(null);
    switch (cmd.id) {
      case "review":
        setDraft("帮我审查当前 Git 变更，指出潜在风险和测试缺口。");
        break;
      case "plan":
        setReadOnly(true);
        setDraft("为这个功能制定实现计划，先不要修改文件。");
        break;
      case "changes":
        void activateBuiltinTab("changes");
        break;
      case "evidence":
        if (runId) {
          void activateBuiltinTab("evidence");
        } else {
          setNotice("请先在左侧选择一个已有会话，或发起运行后查看验证证据。");
        }
        break;
      case "files":
        void activateBuiltinTab("files");
        break;
      case "readonly":
        setReadOnly((prev) => !prev);
        break;
      case "fork":
        if (runId) {
          forkRun(runId);
        } else {
          setNotice("请先选择一个已有 Run 后再创建分支。");
        }
        break;
      case "workflow":
        openWorkflow();
        break;
      case "doctor":
        setSettingsTab("about");
        openSettings();
        void runWorkspaceDoctor();
        break;
      case "clear":
        setNeedsAttentionOnly(false);
        setAttachedFiles([]);
        selectRun(null);
        setPage("home");
        break;
    }
  };

  const selectMentionChip = (chip: AttachedFileChip) => {
    setAttachedFiles((prev) =>
      prev.some((item) => item.relative === chip.relative) ? prev : [...prev, chip],
    );
    if (composerTrigger?.kind === "mention") {
      const applied = applyMentionSelection(draft, composerTrigger, chip.relative);
      setDraft(applied.text);
      setComposerTrigger(null);
      window.setTimeout(() => {
        const ta = composerTextareaRef.current;
        if (ta) {
          ta.focus();
          ta.setSelectionRange(applied.caret, applied.caret);
        }
      }, 0);
      return;
    }
    setComposerTrigger(null);
  };

  const openMentionPicker = () => {
    if (!workspace) return;
    ensureWorkspaceFilesLoaded();
    const ta = composerTextareaRef.current;
    const caret = ta?.selectionStart ?? draft.length;
    const needsSpace = caret > 0 && !/\s$/.test(draft.slice(0, caret));
    const prefix = needsSpace ? " @" : "@";
    const nextText = draft.slice(0, caret) + prefix + draft.slice(caret);
    const nextCaret = caret + prefix.length;
    setDraft(nextText);
    setComposerTrigger(detectComposerTrigger(nextText, nextCaret));
    setComposerMenuIndex(0);
    window.setTimeout(() => {
      const target = composerTextareaRef.current;
      if (target) {
        target.focus();
        target.setSelectionRange(nextCaret, nextCaret);
      }
    }, 0);
  };

  const compose = (
    <div style={{ width: "100%", maxWidth: 780, margin: "0 auto" }}>
      {changes && changes.entries.length > 0 && (
        <div className="wb-composer-change-bar">
          <button
            type="button"
            className="wb-composer-change-summary"
            onClick={() => void activateBuiltinTab("changes")}
          >
            <span className="wb-composer-change-count">
              {changes.entries.length} 个文件已更改
            </span>
            <span className="wb-composer-change-stats">
              {(changes.total_additions ?? 0) > 0 || (changes.total_deletions ?? 0) > 0 ? (
                <>
                  {(changes.total_additions ?? 0) > 0 && (
                    <ins>+{changes.total_additions}</ins>
                  )}
                  {(changes.total_deletions ?? 0) > 0 && (
                    <del>-{changes.total_deletions}</del>
                  )}
                </>
              ) : (
                <>
                  {changes.added > 0 && <ins>+{changes.added}</ins>}
                  {changes.modified > 0 && <em>~{changes.modified}</em>}
                  {changes.deleted > 0 && <del>-{changes.deleted}</del>}
                  {changes.untracked > 0 && <small>?{changes.untracked}</small>}
                </>
              )}
            </span>
            <span className="wb-composer-change-chevron" aria-hidden="true">›</span>
          </button>
          <button
            type="button"
            className="wb-composer-review-btn"
            onClick={() => void activateBuiltinTab("changes")}
          >
            <svg
              width="13"
              height="13"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <rect x="2.5" y="2.2" width="11" height="11.6" rx="1.8" />
              <path d="M6.2 6h3.6M8 4.2v3.6M6.2 10.6h3.6" />
            </svg>
            <span>审查变更</span>
          </button>
        </div>
      )}
      <div className="wb-composer">
        {composerTrigger && (
          <div
            className="wb-composer-popover"
            role="listbox"
            aria-label={composerTrigger.kind === "slash" ? "快捷指令菜单" : "引用工作区文件"}
          >
            <div className="wb-composer-popover-head">
              <span>
                {composerTrigger.kind === "slash" ? "/ 快捷指令" : "@ 引用工作区文件"}
              </span>
              <small>↑↓ 选择 · Enter / Tab 插入 · Esc 关闭</small>
            </div>
            {composerTrigger.kind === "slash" ? (
              slashMatches.length > 0 ? (
                slashMatches.map((cmd, idx) => (
                  <button
                    key={cmd.id}
                    type="button"
                    role="option"
                    aria-selected={idx === composerMenuIndex}
                    className={
                      "wb-composer-popover-item" + (idx === composerMenuIndex ? " is-active" : "")
                    }
                    onMouseDown={(e) => e.preventDefault()}
                    onMouseEnter={() => setComposerMenuIndex(idx)}
                    onClick={() => executeComposerSlashCommand(cmd)}
                  >
                    <code className="wb-composer-pop-cmd">{cmd.name}</code>
                    <span className="wb-composer-pop-desc">{cmd.description}</span>
                    {cmd.hint && <kbd className="wb-launcher-kbd">{cmd.hint}</kbd>}
                  </button>
                ))
              ) : (
                <div className="wb-composer-popover-empty">
                  未找到匹配的快捷指令，可继续输入普通任务或按 Esc 关闭
                </div>
              )
            ) : loadingWorkspaceFiles ? (
              <div className="wb-composer-popover-empty">正在读取工作区文件索引…</div>
            ) : mentionMatches.length > 0 ? (
              mentionMatches.map((item, idx) => (
                <button
                  key={item.relative}
                  type="button"
                  role="option"
                  aria-selected={idx === composerMenuIndex}
                  className={
                    "wb-composer-popover-item" + (idx === composerMenuIndex ? " is-active" : "")
                  }
                  onMouseDown={(e) => e.preventDefault()}
                  onMouseEnter={() => setComposerMenuIndex(idx)}
                  onClick={() => selectMentionChip(item)}
                >
                  <span className="wb-composer-pop-at" aria-hidden="true">@</span>
                  <span className="wb-composer-pop-file">{item.name}</span>
                  <span className="wb-composer-pop-dir">{item.dir || "根目录"}</span>
                </button>
              ))
            ) : (
              <div className="wb-composer-popover-empty">
                {workspaceFiles === null
                  ? "未检测到文件索引能力"
                  : `未找到匹配「${composerTrigger.query}」的工作区文件`}
              </div>
            )}
          </div>
        )}
        {stickyGate}
        {workflowComposeCtx && (
          <div className="wb-workflow-compose-banner" role="status">
            <span className="wb-wf-icon" aria-hidden="true">
              ☍
            </span>
            <span className="wb-workflow-compose-label">
              {workflowComposeCtx.mode === "edit" && workflowComposeCtx.workflow
                ? `正在对话调整工作流：${workflowComposeCtx.workflow.name}`
                : "正在对话创建受控工作流（描述需求即可生成可保存草案）"}
            </span>
            <button
              type="button"
              aria-label="退出工作流编排上下文"
              onClick={() => setWorkflowComposeCtx(null)}
            >
              ×
            </button>
          </div>
        )}
        {forkSource && (
          <div className="wb-fork-source">
            从 Run {forkSource} 创建新分支{" "}
            <button type="button" onClick={() => setForkSource(null)} aria-label="取消分支">
              ×
            </button>
          </div>
        )}
        {attachedFiles.length > 0 && (
          <div className="wb-composer-chips" aria-label="已挂载文件上下文">
            {attachedFiles.map((chip) => (
              <span className="wb-context-chip" key={chip.relative}>
                <span aria-hidden="true">@</span>
                <span title={chip.relative}>{chip.relative}</span>
                <button
                  type="button"
                  aria-label={`移除引用 ${chip.relative}`}
                  onClick={() =>
                    setAttachedFiles((prev) =>
                      prev.filter((item) => item.relative !== chip.relative),
                    )
                  }
                >
                  ×
                </button>
              </span>
            ))}
          </div>
        )}
        <textarea
          ref={composerTextareaRef}
          aria-label="描述任务"
          data-testid="composer-input"
          value={draft}
          onChange={(event) => {
            const nextVal = event.target.value;
            const caret = event.target.selectionStart ?? nextVal.length;
            setDraft(nextVal);
            setComposerTrigger(detectComposerTrigger(nextVal, caret));
            setComposerMenuIndex(0);
          }}
          onClick={(event) => {
            const ta = event.currentTarget;
            setComposerTrigger(detectComposerTrigger(ta.value, ta.selectionStart ?? ta.value.length));
          }}
          placeholder={
            running && runId
              ? runInputMode === "steering"
                ? "输入补充指导，Agent 将在下一安全点消费…（Ctrl/Cmd+Enter 加入候选队列）"
                : "输入后续任务候选，提交后需人工确认启动…"
              : "描述一个任务、输入 @ 引用文件，或输入 / 唤醒快捷指令…"
          }
          onKeyDown={(event) => {
            const ime = event.nativeEvent.isComposing || event.nativeEvent.keyCode === 229;
            if (ime) return;
            if (composerTrigger && activePopoverCount > 0) {
              if (event.key === "ArrowDown") {
                event.preventDefault();
                setComposerMenuIndex((idx) => (idx + 1) % activePopoverCount);
                return;
              }
              if (event.key === "ArrowUp") {
                event.preventDefault();
                setComposerMenuIndex((idx) => (idx - 1 + activePopoverCount) % activePopoverCount);
                return;
              }
              if (event.key === "Escape") {
                event.preventDefault();
                event.stopPropagation();
                setComposerTrigger(null);
                return;
              }
              if (
                event.key === "Tab" ||
                (event.key === "Enter" && !event.shiftKey && !event.ctrlKey && !event.metaKey)
              ) {
                event.preventDefault();
                if (composerTrigger.kind === "slash") {
                  const chosen = slashMatches[composerMenuIndex] ?? slashMatches[0];
                  if (chosen) executeComposerSlashCommand(chosen);
                } else {
                  const chosen = mentionMatches[composerMenuIndex] ?? mentionMatches[0];
                  if (chosen) selectMentionChip(chosen);
                }
                return;
              }
            } else if (composerTrigger && event.key === "Escape") {
              event.preventDefault();
              event.stopPropagation();
              setComposerTrigger(null);
              return;
            }
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
            <button
              type="button"
              title="引用工作区文件 (@)"
              aria-label="引用文件"
              disabled={!workspace || typeof desktop.listFileTree !== "function"}
              onClick={openMentionPicker}
            >
              @ 引用
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
                data-testid="composer-stop"
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
                data-testid="composer-send"
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
  const isDemoMode = demoBridgeActive();
  const currentBridgeMode = bridgeMode();
  const showBridgeUnavailable =
    !isDemoMode &&
    (currentBridgeMode === "unavailable" ||
      (currentBridgeMode === "http" && bridgeStatus === "disconnected"));

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
                      <strong>打开全局命令面板</strong>
                      <p>在任意页面呼出命令面板，快速切换动作、最近会话、模型预设与工作区文件。</p>
                    </div>
                    <span className="wb-kbd-group">
                      <kbd className="wb-kbd">⌘</kbd>
                      <kbd className="wb-kbd">K</kbd>
                    </span>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>展开或收起右侧审查工作台</strong>
                      <p>在聊天主画布与右侧工作台之间切换焦点。</p>
                    </div>
                    <span className="wb-kbd-group">
                      <kbd className="wb-kbd">⌘</kbd>
                      <kbd className="wb-kbd">B</kbd>
                    </span>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>打开工作台文件定位框</strong>
                      <p>在右侧审查工作台中快速跳转到任意源码文件或面板。</p>
                    </div>
                    <span className="wb-kbd-group">
                      <kbd className="wb-kbd">⌘</kbd>
                      <kbd className="wb-kbd">P</kbd>
                    </span>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>打开变更审查 / 验证证据</strong>
                      <p>直达 Git 变更审阅或 Verifier 可信证据裁决面板。</p>
                    </div>
                    <span className="wb-kbd-group">
                      <kbd className="wb-kbd">⌃⇧G</kbd>
                      <kbd className="wb-kbd">⌘E</kbd>
                    </span>
                  </div>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>输入框引用文件或斜杠命令</strong>
                      <p>输入 @ 搜索并附加工作区文件，输入 / 触发常用操作与模式指令。</p>
                    </div>
                    <span className="wb-kbd-group">
                      <kbd className="wb-kbd">@</kbd>
                      <kbd className="wb-kbd">/</kbd>
                    </span>
                  </div>
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
                      <p>在命令面板、设置中心或窄屏工作台按下 Esc 逐级退回。</p>
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

  const renderProjectBranch = (project: { path: string; name: string }, isCurrent: boolean) => {
    const expanded = isCurrent && !projectCollapsed;
    return (
      <div
        key={project.path}
        className={"wb-project-node" + (isCurrent ? " is-current" : "")}
      >
        <button
          type="button"
          className={"wb-project-header" + (isCurrent ? " current" : "")}
          aria-expanded={isCurrent ? expanded : undefined}
          onClick={() => {
            if (isCurrent) {
              setProjectCollapsed((prev) => !prev);
            } else {
              void switchProject(project.path);
            }
          }}
        >
          <svg
            className={"wb-project-chevron" + (expanded ? " is-expanded" : "")}
            width="12"
            height="12"
            viewBox="0 0 16 16"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.75"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <path d="m6 4 4 4-4 4" />
          </svg>
          <svg
            className="wb-project-folder"
            width="14"
            height="14"
            viewBox="0 0 16 16"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <path d="M2 4.5A1.5 1.5 0 0 1 3.5 3H6l1.4 1.5h5.1A1.5 1.5 0 0 1 14 6v5.5a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 2 11.5z" />
          </svg>
          <span className="wb-project-name">{project.name || project.path.split("/").pop()}</span>
          {isCurrent && visibleRuns.length > 0 && (
            <span className="wb-project-count">{visibleRuns.length}</span>
          )}
        </button>
        {expanded && (
          <div className="wb-project-runs">
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
              <div className="wb-empty-runs">{filter ? "无匹配会话" : "暂无任务"}</div>
            )}
          </div>
        )}
      </div>
    );
  };

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
              title={searchOpen ? "收起会话搜索" : "搜索会话"}
              aria-label="切换会话搜索"
              aria-pressed={searchOpen}
              onClick={() => {
                setSearchOpen((open) => {
                  const next = !open;
                  if (next) {
                    window.setTimeout(() => sessionSearchRef.current?.focus(), 0);
                  } else {
                    setFilter("");
                  }
                  return next;
                });
              }}
            >
              <svg
                width="15"
                height="15"
                viewBox="0 0 16 16"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.6"
                strokeLinecap="round"
                aria-hidden="true"
              >
                <circle cx="7.2" cy="7.2" r="4.4" />
                <path d="m10.5 10.5 3 3" />
              </svg>
            </button>
          </div>
        </div>
        <nav className="wb-nav">
          <button
            type="button"
            data-testid="new-session"
            className={page === "home" && !runId && !needsAttentionOnly ? "active" : ""}
            onClick={() => {
              setNeedsAttentionOnly(false);
              selectRun(null);
              setPage("home");
            }}
          >
            <svg
              className="wb-nav-icon"
              width="15"
              height="15"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.6"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <path d="M8.5 3H4.5A1.5 1.5 0 0 0 3 4.5v7A1.5 1.5 0 0 0 4.5 13h7a1.5 1.5 0 0 0 1.5-1.5V7.5" />
              <path d="M12.6 2.4a1.2 1.2 0 0 1 1.7 1.7L8.2 10.2l-2.3.6.6-2.3z" />
            </svg>
            <span>新建任务</span>
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
            <svg
              className="wb-nav-icon"
              width="15"
              height="15"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.6"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <path d="M3.5 2.5v11" />
              <path d="M3.5 3h8l-1.5 2.8L11.5 8.5h-8" />
            </svg>
            <span>需要我</span>
            {attentionCount > 0 && <span className="wb-nav-badge is-warn">{attentionCount}</span>}
          </button>
          <button
            type="button"
            className={page === "workflow" ? "active" : ""}
            onClick={() => void openWorkflow()}
          >
            <svg
              className="wb-nav-icon"
              width="15"
              height="15"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <rect x="2.2" y="2.4" width="4.4" height="4.4" rx="1.1" />
              <rect x="9.4" y="2.4" width="4.4" height="4.4" rx="1.1" />
              <rect x="5.8" y="9.2" width="4.4" height="4.4" rx="1.1" />
              <path d="M6.6 4.6h2.8M8 6.8v2.4" />
            </svg>
            <span>工作流</span>
          </button>
        </nav>
        <div className="wb-sidebar-title">
          <span>{showArchived ? "工作区 · 归档" : "工作区"}</span>
          <div className="wb-sidebar-head-tools">
            <button
              type="button"
              aria-label="切换已归档会话"
              aria-pressed={showArchived}
              title={showArchived ? "返回活跃会话" : "查看已归档会话"}
              onClick={() => {
                setNeedsAttentionOnly(false);
                setShowArchived((v) => !v);
              }}
            >
              <svg
                width="13.5"
                height="13.5"
                viewBox="0 0 16 16"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden="true"
              >
                <rect x="2" y="2.8" width="12" height="3" rx="0.8" />
                <path d="M3 5.8v6.7A1.2 1.2 0 0 0 4.2 13.7h7.6a1.2 1.2 0 0 0 1.2-1.2V5.8" />
                <path d="M6.3 8.8h3.4" />
              </svg>
            </button>
            <button
              type="button"
              title="添加项目"
              aria-label="添加项目"
              onClick={() => void pickProject()}
            >
              <svg
                width="13.5"
                height="13.5"
                viewBox="0 0 16 16"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.65"
                strokeLinecap="round"
                aria-hidden="true"
              >
                <path d="M8 3.2v9.6M3.2 8h9.6" />
              </svg>
            </button>
          </div>
        </div>
        {(searchOpen || Boolean(filter)) && (
          <div className="wb-sidebar-search">
            <div className="wb-sidebar-search-box">
              <svg
                width="12.5"
                height="12.5"
                viewBox="0 0 16 16"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.6"
                strokeLinecap="round"
                aria-hidden="true"
              >
                <circle cx="7.2" cy="7.2" r="4.3" />
                <path d="m10.5 10.5 3 3" />
              </svg>
              <input
                ref={sessionSearchRef}
                aria-label="搜索会话"
                placeholder="筛选会话…"
                value={filter}
                onChange={(event) => setFilter(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === "Escape") {
                    setFilter("");
                    setSearchOpen(false);
                  }
                }}
              />
              {filter && (
                <button
                  type="button"
                  aria-label="清空搜索"
                  onClick={() => setFilter("")}
                >
                  ×
                </button>
              )}
            </div>
          </div>
        )}
        <div className="wb-project-list">
          {recent.map((project) => renderProjectBranch(project, workspace === project.path))}
          {workspace &&
            !recent.some((project) => project.path === workspace) &&
            renderProjectBranch({ path: workspace, name: workspaceName }, true)}
          {!recent.length && !workspace && <div className="wb-hint">尚未添加工作区</div>}
        </div>
        <div className="wb-sidebar-bottom">
          <span className="wb-sidebar-version">
            <i className={"wb-status-dot " + bridgeDotTone} aria-hidden="true" />
            Cairn · {bridgeStatusText}
          </span>
          <button type="button" onClick={() => void openSettings()}>
            <svg
              width="14"
              height="14"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden="true"
            >
              <circle cx="8" cy="8" r="2.2" />
              <path d="M8 2v1.4M8 12.6V14M3.8 3.8l1 1M11.2 11.2l1 1M2 8h1.4M12.6 8H14M3.8 12.2l1-1M11.2 4.8l1-1" />
            </svg>
            <span>设置</span>
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
              <span className="wb-crumb" data-testid="workspace-chip">
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
              {page === "home" && activeWorkflowExecution && (
                <button
                  type="button"
                  className="wb-toolbar-wf-pill"
                  title="查看当前关联工作流"
                  onClick={() => {
                    setFocusedWorkflowId(activeWorkflowExecution.workflowId);
                    openWorkflow();
                  }}
                >
                  <span aria-hidden="true">☍</span>
                  <span className="wb-toolbar-wf-name">
                    {activeWorkflowExecution.workflowName}
                  </span>
                  <span className="wb-toolbar-wf-stage">
                    {activeWorkflowExecution.stages[activeWorkflowExecution.activeStepIndex]
                      ?.step.task || ""}
                  </span>
                </button>
              )}
              <button
                type="button"
                className="wb-cmdk-trigger"
                title="打开全局命令面板 (⌘K)"
                aria-label="打开全局命令面板"
                onClick={() => setCommandPaletteOpen(true)}
              >
                <span>命令面板</span>
                <kbd className="wb-kbd">⌘K</kbd>
              </button>
              <button
                type="button"
                className="wb-icon-btn"
                title="切换明暗主题"
                aria-label="切换明暗主题"
                onClick={toggleQuickColorScheme}
              >
                ◐
              </button>
              {page === "home" && workspace && (
                <button
                  type="button"
                  className={
                    "wb-icon-btn wb-sidecar-toggle-btn" + (panel !== "closed" ? " is-active" : "")
                  }
                  title="切换右侧工作台 (⌘B)"
                  aria-label="切换右侧工作台"
                  aria-expanded={panel !== "closed"}
                  onClick={toggleSidecar}
                >
                  <svg
                    width="15"
                    height="15"
                    viewBox="0 0 16 16"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="1.45"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    aria-hidden="true"
                  >
                    <rect x="2" y="2.5" width="12" height="11" rx="2" />
                    <path d="M9.8 2.5v11" />
                  </svg>
                  {panel === "closed" && changes && changes.entries.length > 0 && (
                    <span className="wb-sidecar-toggle-dot" aria-hidden="true" />
                  )}
                </button>
              )}
            </div>
          </header>
          {isDemoMode && (
            <div className="wb-demo-banner" data-testid="demo-banner" role="status">
              <span className="wb-badge is-warn">离线演示模式</span>
              <span>
                当前通过显式 ?cairn-demo=1 运行浏览器内存演示桥接，数据不会写入真实 SQLite Event Journal。
              </span>
            </div>
          )}
          {showBridgeUnavailable && (
            <div
              className="wb-bridge-banner"
              data-testid="bridge-unavailable-banner"
              role="alert"
            >
              <span className="wb-badge is-warn">桌面桥接不可用</span>
              <span>
                未检测到 Wails 宿主或本地桥接服务；请通过 Cairn Studio 桌面端启动，或追加 ?cairn-demo=1 进入离线演示。
              </span>
            </div>
          )}
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
              <div className={"wb-session" + (recallStops.length >= 2 ? " has-recall-rail" : "")}>
                <SessionRecallRail
                  stops={recallStops}
                  activeId={activeRecallId}
                  onSelect={jumpToRecallStop}
                />
                <div
                  ref={sessionScrollRef}
                  className="wb-session-scroll"
                  onScroll={handleSessionScroll}
                >
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
                        flashedAnchorId={flashedRecallId}
                        workflows={workflows}
                        onApproval={decide}
                        onInspectFile={(targetPath) => void inspectChangedFile(targetPath)}
                        onSaveWorkflow={(spec) => handleSaveWorkflowSpec(spec).then(() => {})}
                        onSaveAndRunWorkflow={handleSaveAndRunWorkflowSpec}
                        onRefineWorkflow={handleRefineWorkflowSpec}
                        onOpenWorkflowPortal={(wfId) => {
                          if (wfId) setFocusedWorkflowId(wfId);
                          openWorkflow();
                        }}
                      />
                    ) : (
                      <p className="wb-hint">正在等待 Journal 中的运行记录…</p>
                    )}
                    {activeWorkflowExecution && (
                      <WorkflowExecutionCard
                        execution={activeWorkflowExecution}
                        activeRunId={runId}
                        onSelectStageRun={(stageRunId) => {
                          autoFollowWorkflowRunRef.current = null;
                          selectRun(stageRunId);
                        }}
                        onOpenPortal={(wfId) => {
                          setFocusedWorkflowId(wfId);
                          openWorkflow();
                        }}
                        onDebugInChat={() => {
                          const targetWf =
                            workflows.find(
                              (w) => w.id === activeWorkflowExecution.workflowId,
                            ) || {
                              id: activeWorkflowExecution.workflowId,
                              name: activeWorkflowExecution.workflowName,
                              steps: activeWorkflowExecution.binding.steps,
                            };
                          setWorkflowComposeCtx({
                            mode: "edit",
                            workflow: targetWf,
                            note: `关联运行 ${activeWorkflowExecution.latestRunId}（当前阶段 ${
                              activeWorkflowExecution.activeStepIndex + 1
                            }/${activeWorkflowExecution.stages.length}）`,
                          });
                          setDraft((prev) =>
                            prev.trim()
                              ? prev
                              : `帮我分析或调整工作流「${activeWorkflowExecution.workflowName}」：`,
                          );
                          focusComposerSoon();
                        }}
                        onStopRun={stopWorkflowStageRun}
                      />
                    )}
                    {running &&
                      !selected?.waiting_approval &&
                      !selected?.waiting_reconcile && (
                        <div className="wb-live-vital" role="status" aria-live="polite">
                          <span className="wb-live-vital-dot" aria-hidden="true" />
                          <strong className="wb-live-vital-stage">{liveVital.stageLabel}</strong>
                          {liveVital.detail && (
                            <span className="wb-live-vital-detail">{liveVital.detail}</span>
                          )}
                          <span className="wb-live-vital-metrics">
                            {liveVital.turn > 0 ? `第 ${liveVital.turn} 轮 · ` : ""}
                            {liveVital.toolCount} 次工具
                          </span>
                        </div>
                      )}
                    {runId && selected?.state === "completed" && (
                      <section className="wb-stone-milestone" aria-label="运行完成里程碑">
                        <div className="wb-stone-milestone-head">
                          <div className="wb-stone-milestone-title">
                            <span className="wb-stone-mark" aria-hidden="true">
                              ◆
                            </span>
                            <strong>运行已完成收尾</strong>
                            <span className="wb-hash-pill">{runId}</span>
                          </div>
                          {evidence ? (
                            <span className={"wb-badge " + (evidence.trusted ? "is-ok" : "is-warn")}>
                              {evidence.trusted
                                ? "✓ Verifier 可信"
                                : "未确认验证 · 完成 ≠ 已验证"}
                            </span>
                          ) : (
                            <span className="wb-badge">验证证据待核查</span>
                          )}
                        </div>
                        <p className="wb-stone-milestone-note">
                          {evidence
                            ? evidence.trusted
                              ? `已绑定 Verifier 证据${
                                  evidence.verification?.verification_seq
                                    ? `（#seq ${evidence.verification.verification_seq}）`
                                    : ""
                                }，共执行 ${evidence.metrics?.turns ?? 0} 轮、${
                                  evidence.metrics?.tool_calls ?? 0
                                } 次工具调用。`
                              : "运行循环已结束，但当前没有通过的 Verifier 验证证据，不能将运行完成等同于修改正确。"
                            : "运行循环已结束；可打开验证证据面板核查 Verifier 退出码与运行指标。"}
                          {changes?.entries?.length
                            ? ` 当前工作区检测到 ${changes.entries.length} 项文件变更。`
                            : ""}
                        </p>
                        <div className="wb-stone-milestone-actions">
                          <button
                            type="button"
                            className="wb-ghost"
                            onClick={() => void activateBuiltinTab("changes")}
                          >
                            打开变更审查
                          </button>
                          <button
                            type="button"
                            className="wb-ghost"
                            onClick={() => void activateBuiltinTab("evidence")}
                          >
                            核查验证证据 (⌘E)
                          </button>
                          <button
                            type="button"
                            className="wb-ghost"
                            onClick={() => forkRun(runId)}
                          >
                            派生分支 (⌘⇧S)
                          </button>
                        </div>
                      </section>
                    )}
                    <div ref={bottomRef} />
                  </div>
                </div>
                {showScrollBottom && (
                  <button
                    type="button"
                    className="wb-scroll-bottom-pill"
                    onClick={scrollToSessionBottom}
                  >
                    ↓ 回到底部
                  </button>
                )}
                <div className="wb-session-composer">{compose}</div>
              </div>
            ))}
          {page === "workflow" && (
            <section className="wb-content-page">
              <div className="wb-page-head">
                <span>WORKFLOWS</span>
                <h1>工作流</h1>
                <p>通过对话创建、调试与演进工作流；此处用于只读检视流程定义、运行历史与直接触发。</p>
              </div>
              {workspace ? (
                <WorkflowsView
                  key={workspace}
                  workspace={workspace}
                  notify={onQueueError}
                  runs={runs}
                  bindings={workflowBindings}
                  focusedWorkflowId={focusedWorkflowId}
                  onConverseCreate={startConversationalWorkflowCreate}
                  onConverseEdit={startConversationalWorkflowEdit}
                  onLaunchWorkflow={launchWorkflowInSession}
                  onOpenRun={(targetRunId) => {
                    autoFollowWorkflowRunRef.current = null;
                    selectRun(targetRunId);
                    setPage("home");
                  }}
                />
              ) : (
                <p>请先选择工作区。</p>
              )}
              <button type="button" className="wb-secondary" onClick={closeToSession}>
                返回工作区
              </button>
            </section>
          )}
        </main>
      {page === "home" && Boolean(workspace) && panel !== "closed" && (
        <SidecarWorkbench
          workspace={workspace}
          runId={runId}
          selectedRun={selected}
          runStatusText={statusName(selected)}
          tabs={sidecarTabs}
          activeTabId={activeSidecarTabId}
          expanded={expandedDiff}
          omniboxFocusRevision={omniboxFocusRev}
          changes={changes}
          selectedFile={selectedFile}
          diff={diff}
          evidence={evidence}
          compactions={compactions}
          timelineKind={timelineKind}
          filteredTimeline={filteredTimeline}
          recentTimeline={timelineItems}
          eventHashBySeq={eventHashBySeq}
          subagents={subagents}
          sessionTree={sessionTree}
          pendingQueueCount={pendingQueueCount}
          queueRevision={queueRevision}
          openLocations={openLocations}
          onActivateTab={activateSidecarTab}
          onCloseTab={closeSidecarTab}
          onOpenNewLauncherTab={openNewLauncherTab}
          onSelectBuiltinPanel={(kind) => void activateBuiltinTab(kind)}
          onOpenFileTab={openFileTab}
          onInspectChangedFile={(rel) => void inspectChangedFile(rel)}
          onChooseDiffFile={(rel) => void chooseFile(rel)}
          onSaveDiffComment={saveDiffComment}
          onRefreshChanges={() => loadSidecarDataForKind("changes")}
          onChangeTimelineKind={setTimelineKind}
          onOpenChildRun={openChild}
          onForkCurrentRun={() => {
            if (runId) forkRun(runId);
          }}
          onOpenWorkspaceInLocation={(locId) => void handleOpenWorkspaceInEditor(locId)}
          onRunFromQueue={onRunFromQueue}
          onToggleExpanded={() => setExpandedDiff((open) => !open)}
          onCloseSidecar={() => {
            setPanel("closed");
            setExpandedDiff(false);
          }}
          notify={onQueueError}
        />
      )}
      {page === "home" && Boolean(workspace) && (
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
          {runId && (
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
          )}
          {runId && (
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
          )}
          <button
            type="button"
            className={
              "wb-dock-btn" + (panel === "files" || panel === "file" ? " current" : "")
            }
            data-tooltip="文件"
            aria-label="文件"
            aria-pressed={panel === "files" || panel === "file"}
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
        </nav>
      )}
      </div>
        </>
      )}
      <CommandPalette
        open={commandPaletteOpen}
        workspace={workspace}
        runId={runId}
        runs={runs}
        providerPresets={providerPresets}
        changesCount={changes?.entries?.length ?? 0}
        pendingQueueCount={pendingQueueCount}
        readOnly={readOnly}
        onClose={() => setCommandPaletteOpen(false)}
        onNewTask={() => {
          setNeedsAttentionOnly(false);
          setAttachedFiles([]);
          selectRun(null);
          setPage("home");
        }}
        onSelectRun={(id) => {
          setPage("home");
          selectRun(id);
        }}
        onToggleSidecar={() => {
          setPage("home");
          toggleSidecar();
        }}
        onOpenSidecarTab={(kind) => {
          setPage("home");
          void activateBuiltinTab(kind);
        }}
        onOpenFileTab={(rel) => {
          setPage("home");
          openFileTab(rel);
        }}
        onForkCurrentRun={() => {
          if (runId) {
            setPage("home");
            forkRun(runId);
          }
        }}
        onToggleReadOnly={() => setReadOnly((prev) => !prev)}
        onToggleTheme={toggleQuickColorScheme}
        onOpenWorkflow={openWorkflow}
        onConverseWorkflow={() => startConversationalWorkflowCreate()}
        onOpenSettingsTab={(tab) => {
          setSettingsTab(tab);
          openSettings();
        }}
        onRunDoctor={() => {
          setSettingsTab("about");
          openSettings();
          void runWorkspaceDoctor();
        }}
        onActivatePreset={(preset) => void handleActivatePresetFromHero(preset)}
      />
    </div>
  );
}
