/**
 * INPUT: 工作区状态、运行列表、模型预设列表与各导航/面板/动作回调。
 * OUTPUT: 全局 ⌘K / Ctrl+K 命令面板（聚合快捷动作、最近会话、模型预设与懒加载工作区文件搜索）。
 * POS: studio/frontend/src/workbench 全局键盘神经中枢，只读消费桥接数据并分发既有安全动作。
 *
 * Author: fxbin
 */
import { useEffect, useMemo, useRef, useState } from "react";
import { desktop } from "../desktop/client";
import type { ProviderPresetView, RunHistoryView } from "../desktop/types";
import type { SidecarTabKind } from "./SidecarWorkbench";
import {
  flattenWorkspaceFiles,
  matchWorkspaceFiles,
  type AttachedFileChip,
} from "./composerContext";

type PaletteGroupKey = "actions" | "sessions" | "presets" | "files";

interface PaletteItem {
  id: string;
  group: PaletteGroupKey;
  title: string;
  subtitle?: string;
  badge?: string;
  badgeTone?: "is-ok" | "is-warn" | "is-accent" | "is-muted";
  shortcut?: string;
  dotColor?: string;
  keywords: string;
  onSelect: () => void;
}

const GROUP_LABELS: ReadonlyArray<{ key: PaletteGroupKey; label: string }> = [
  { key: "actions", label: "快捷动作" },
  { key: "sessions", label: "最近会话" },
  { key: "presets", label: "模型预设" },
  { key: "files", label: "工作区文件" },
];

const TERMINAL_STATES = new Set(["completed", "failed", "cancelled"]);

/**
 * 解析运行状态对应的语义圆点颜色与简短徽标文案。
 */
function resolvePaletteRunMeta(run: RunHistoryView): {
  dotColor: string;
  badge?: string;
  badgeTone?: "is-ok" | "is-warn" | "is-accent" | "is-muted";
} {
  if (run.waiting_reconcile) {
    return { dotColor: "var(--wb-amber)", badge: "待对账", badgeTone: "is-warn" };
  }
  if (run.waiting_approval) {
    return { dotColor: "var(--wb-amber)", badge: "待授权", badgeTone: "is-warn" };
  }
  if (!TERMINAL_STATES.has(run.state)) {
    return {
      dotColor: "var(--wb-action)",
      badge: run.phase_label || "运行中",
      badgeTone: "is-accent",
    };
  }
  if (run.state === "failed") {
    return { dotColor: "var(--wb-danger)", badge: "失败", badgeTone: "is-warn" };
  }
  if (run.state === "completed") {
    return { dotColor: "var(--wb-moss)", badge: "完成", badgeTone: "is-ok" };
  }
  return { dotColor: "var(--wb-subtle)", badge: "已取消", badgeTone: "is-muted" };
}

/**
 * CommandPaletteProps 描述全局命令面板所需的态势投影与调度回调。
 */
export interface CommandPaletteProps {
  open: boolean;
  workspace: string;
  runId: string | null;
  runs: RunHistoryView[];
  providerPresets: ProviderPresetView[];
  changesCount: number;
  pendingQueueCount: number;
  readOnly: boolean;
  onClose: () => void;
  onNewTask: () => void;
  onSelectRun: (id: string) => void;
  onToggleSidecar: () => void;
  onOpenSidecarTab: (kind: Exclude<SidecarTabKind, "file">) => void;
  onOpenFileTab: (relativePath: string) => void;
  onForkCurrentRun: () => void;
  onToggleReadOnly: () => void;
  onToggleTheme: () => void;
  onOpenWorkflow: () => void;
  onConverseWorkflow?: () => void;
  onOpenSettingsTab: (tab: "models" | "about" | "general") => void;
  onRunDoctor: () => void;
  onActivatePreset: (preset: ProviderPresetView) => void;
}

/**
 * 渲染全局 ⌘K 命令面板模态框，支持中英文关键词筛选、IME 防误触与全键盘导航。
 */
export default function CommandPalette({
  open,
  workspace,
  runId,
  runs,
  providerPresets,
  changesCount,
  pendingQueueCount,
  readOnly,
  onClose,
  onNewTask,
  onSelectRun,
  onToggleSidecar,
  onOpenSidecarTab,
  onOpenFileTab,
  onForkCurrentRun,
  onToggleReadOnly,
  onToggleTheme,
  onOpenWorkflow,
  onConverseWorkflow,
  onOpenSettingsTab,
  onRunDoctor,
  onActivatePreset,
}: CommandPaletteProps) {
  const [query, setQuery] = useState("");
  const [activeIndex, setActiveIndex] = useState(0);
  const [cachedWorkspace, setCachedWorkspace] = useState("");
  const [flatFiles, setFlatFiles] = useState<AttachedFileChip[] | null>(null);
  const [loadingFiles, setLoadingFiles] = useState(false);

  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (cachedWorkspace !== workspace) {
      setCachedWorkspace(workspace);
      setFlatFiles(null);
      setLoadingFiles(false);
    }
  }, [workspace, cachedWorkspace]);

  useEffect(() => {
    if (!open) {
      setQuery("");
      setActiveIndex(0);
      return;
    }
    const timer = window.setTimeout(() => {
      inputRef.current?.focus();
      inputRef.current?.select();
    }, 0);
    return () => window.clearTimeout(timer);
  }, [open]);

  // 仅在面板打开且用户输入检索关键字时按需读取文件树索引，避免无谓消耗。
  useEffect(() => {
    if (
      !open ||
      !workspace ||
      !query.trim() ||
      flatFiles !== null ||
      loadingFiles ||
      typeof desktop.listFileTree !== "function"
    ) {
      return;
    }
    let alive = true;
    const targetWorkspace = workspace;
    setLoadingFiles(true);
    void Promise.resolve(desktop.listFileTree(targetWorkspace))
      .then((tree) => {
        if (alive && targetWorkspace === workspace) {
          setFlatFiles(flattenWorkspaceFiles(tree));
        }
      })
      .catch(() => {
        if (alive && targetWorkspace === workspace) {
          setFlatFiles([]);
        }
      })
      .finally(() => {
        if (alive && targetWorkspace === workspace) {
          setLoadingFiles(false);
        }
      });
    return () => {
      alive = false;
    };
  }, [open, workspace, query, flatFiles, loadingFiles]);

  const flatItems = useMemo<PaletteItem[]>(() => {
    if (!open) return [];
    const normalizedQuery = query.trim().toLowerCase();
    const tokens = normalizedQuery ? normalizedQuery.split(/\s+/).filter(Boolean) : [];
    const matchesTokens = (text: string) =>
      tokens.length === 0 || tokens.every((tok) => text.toLowerCase().includes(tok));

    const rawActionItems: PaletteItem[] = [
      {
        id: "act:new-task",
        group: "actions",
        title: "新建任务",
        subtitle: "回到空白指令舱开启新会话",
        shortcut: "⌘N",
        keywords: "new task 新建 任务 空白",
        onSelect: onNewTask,
      },
      ...(workspace
        ? [
            {
              id: "act:toggle-sidecar",
              group: "actions" as const,
              title: "切换右侧工作台",
              subtitle: "展开或收起右侧多标签伴随面板",
              shortcut: "⌘B",
              keywords: "sidecar workbench panel 切换 右侧 工作台 面板",
              onSelect: onToggleSidecar,
            },
            {
              id: "act:changes",
              group: "actions" as const,
              title: "检查工作区变更（Diff）",
              subtitle: "打开连续代码变更审查视图",
              badge: changesCount > 0 ? `${changesCount} 项` : undefined,
              badgeTone: changesCount > 0 ? ("is-accent" as const) : undefined,
              shortcut: "⌃⇧G",
              keywords: "diff changes review 变更 审查 代码 差异",
              onSelect: () => onOpenSidecarTab("changes"),
            },
            {
              id: "act:files",
              group: "actions" as const,
              title: "浏览项目文件",
              subtitle: "只读文件树与内建源码阅读器",
              shortcut: "⌘P",
              keywords: "files tree project 文件 目录 源码",
              onSelect: () => onOpenSidecarTab("files"),
            },
            {
              id: "act:queue",
              group: "actions" as const,
              title: "查看候选任务队列",
              subtitle: "入队不等于启动，需人工显式确认",
              badge: pendingQueueCount > 0 ? `${pendingQueueCount} 待确认` : undefined,
              badgeTone: pendingQueueCount > 0 ? ("is-warn" as const) : undefined,
              keywords: "queue 队列 候选 任务",
              onSelect: () => onOpenSidecarTab("queue"),
            },
          ]
        : []),
      ...(workspace && runId
        ? [
            {
              id: "act:evidence",
              group: "actions" as const,
              title: "查看验证与证据",
              subtitle: "核查 Verifier 可信状态与 Event Journal 时间线",
              shortcut: "⌘E",
              keywords: "evidence verifier journal 证据 验证 时间线",
              onSelect: () => onOpenSidecarTab("evidence"),
            },
            {
              id: "act:fork",
              group: "actions" as const,
              title: "从当前 Run 创建分支",
              subtitle: "派生独立分支试探方案，不污染原会话",
              shortcut: "⌘⇧S",
              keywords: "fork branch 分支 对话",
              onSelect: onForkCurrentRun,
            },
          ]
        : []),
      {
        id: "act:readonly",
        group: "actions",
        title: readOnly ? "切换为默认审批 ask 模式" : "切换为只读探索模式",
        subtitle: readOnly
          ? "当前：只读探索（禁止写与命令副作用）"
          : "当前：默认审批 ask（副作用需经授权）",
        badge: readOnly ? "只读中" : "ask",
        badgeTone: readOnly ? "is-warn" : "is-muted",
        keywords: "readonly mode permission 只读 权限 审批",
        onSelect: onToggleReadOnly,
      },
      {
        id: "act:theme",
        group: "actions",
        title: "切换明暗主题",
        subtitle: "在宣纸暖石（浅色）与玄武岩夜色（深色）间切换",
        keywords: "theme dark light appearance 主题 深色 浅色 外观",
        onSelect: onToggleTheme,
      },
      ...(onConverseWorkflow
        ? [
            {
              id: "act:workflow-converse",
              group: "actions" as const,
              title: "通过对话创建工作流",
              subtitle: "向 Agent 描述流程需求或调试已有工作流并保存草案",
              keywords: "workflow create chat converse 工作流 对话 创建 调试 编排",
              onSelect: onConverseWorkflow,
            },
          ]
        : []),
      {
        id: "act:workflow",
        group: "actions",
        title: "查看工作流目录",
        subtitle: "只读检视已保存工作流、运行历史或立即触发",
        keywords: "workflow 工作流 流程 目录",
        onSelect: onOpenWorkflow,
      },
      {
        id: "act:models",
        group: "actions",
        title: "模型与供应商设置",
        subtitle: "管理预设、环境变量引用与两阶段工具准入",
        shortcut: "⌘,",
        keywords: "model provider settings 模型 供应商 设置 准入",
        onSelect: () => onOpenSettingsTab("models"),
      },
      {
        id: "act:doctor",
        group: "actions",
        title: "执行环境诊断（Doctor）",
        subtitle: "跳转到环境与关于页并运行只读健康自检",
        keywords: "doctor health diagnostics 诊断 健康 检查",
        onSelect: onRunDoctor,
      },
    ];
    const actionItems = rawActionItems.filter((item) =>
      matchesTokens(`${item.title} ${item.subtitle ?? ""} ${item.keywords}`),
    );

    const sessionItems: PaletteItem[] = runs
      .filter((r) => !r.archived)
      .filter((r) => matchesTokens(`${r.title} ${r.task} ${r.run_id}`))
      .slice(0, normalizedQuery ? 8 : 5)
      .map((r) => {
        const meta = resolvePaletteRunMeta(r);
        return {
          id: `ses:${r.run_id}`,
          group: "sessions",
          title: r.title || r.task || r.run_id,
          subtitle: r.run_id,
          badge: meta.badge,
          badgeTone: meta.badgeTone,
          dotColor: meta.dotColor,
          keywords: `${r.title} ${r.task} ${r.run_id}`,
          onSelect: () => onSelectRun(r.run_id),
        };
      });

    const presetItems: PaletteItem[] = providerPresets
      .filter((p) => matchesTokens(`${p.name} ${p.model} ${p.base_url}`))
      .map((p) => ({
        id: `pre:${p.name}`,
        group: "presets",
        title: `${p.name}${p.model ? ` · ${p.model}` : ""}`,
        subtitle: p.base_url,
        badge: p.active ? "当前激活" : undefined,
        badgeTone: p.active ? "is-ok" : undefined,
        keywords: `${p.name} ${p.model} ${p.base_url}`,
        onSelect: () => onActivatePreset(p),
      }));

    const fileItems: PaletteItem[] =
      normalizedQuery && flatFiles
        ? matchWorkspaceFiles(flatFiles, query, 8).map((file) => ({
            id: `file:${file.relative}`,
            group: "files",
            title: file.name,
            subtitle: file.dir || "工作区根目录",
            keywords: file.relative,
            onSelect: () => onOpenFileTab(file.relative),
          }))
        : [];

    return [...actionItems, ...sessionItems, ...presetItems, ...fileItems];
  }, [
    open,
    query,
    workspace,
    runId,
    runs,
    providerPresets,
    changesCount,
    pendingQueueCount,
    readOnly,
    flatFiles,
    onNewTask,
    onSelectRun,
    onToggleSidecar,
    onOpenSidecarTab,
    onOpenFileTab,
    onForkCurrentRun,
    onToggleReadOnly,
    onToggleTheme,
    onOpenWorkflow,
    onOpenSettingsTab,
    onRunDoctor,
    onActivatePreset,
  ]);

  useEffect(() => {
    setActiveIndex(0);
  }, [query, open]);

  if (!open) return null;

  const executeItem = (item: PaletteItem | undefined) => {
    if (!item) return;
    onClose();
    item.onSelect();
  };

  return (
    <div
      className="wb-cmdk-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) {
          onClose();
        }
      }}
    >
      <div
        className="wb-cmdk-dialog"
        role="dialog"
        aria-modal="true"
        aria-label="全局命令面板"
        data-testid="command-palette"
      >
        <div className="wb-cmdk-head">
          <svg
            width="15"
            height="15"
            viewBox="0 0 16 16"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            aria-hidden="true"
          >
            <circle cx="7" cy="7" r="4.6" />
            <path d="m10.5 10.5 3.1 3.1" />
          </svg>
          <input
            ref={inputRef}
            type="search"
            aria-label="搜索命令、会话或文件"
            placeholder="输入命令、会话标题、模型预设或工作区文件名…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              const ime = e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229;
              if (ime) return;
              if (e.key === "Escape") {
                e.preventDefault();
                e.stopPropagation();
                onClose();
                return;
              }
              if (e.key === "ArrowDown" && flatItems.length > 0) {
                e.preventDefault();
                setActiveIndex((idx) => (idx + 1) % flatItems.length);
                return;
              }
              if (e.key === "ArrowUp" && flatItems.length > 0) {
                e.preventDefault();
                setActiveIndex((idx) => (idx - 1 + flatItems.length) % flatItems.length);
                return;
              }
              if (e.key === "Enter" && flatItems.length > 0) {
                e.preventDefault();
                executeItem(flatItems[activeIndex] ?? flatItems[0]);
              }
            }}
          />
          <kbd className="wb-launcher-kbd">Esc</kbd>
        </div>

        <div className="wb-cmdk-list" role="listbox" aria-label="命令面板结果">
          {flatItems.length === 0 ? (
            <div className="wb-cmdk-empty">
              {loadingFiles
                ? "正在检索工作区文件索引…"
                : `未找到与「${query.trim()}」匹配的命令、会话或文件`}
            </div>
          ) : (
            GROUP_LABELS.map((group) => {
              const itemsInGroup = flatItems.filter((item) => item.group === group.key);
              if (itemsInGroup.length === 0) return null;
              return (
                <div className="wb-cmdk-group" key={group.key}>
                  <div className="wb-cmdk-group-title">{group.label}</div>
                  {itemsInGroup.map((item) => {
                    const globalIdx = flatItems.indexOf(item);
                    const isActive = globalIdx === activeIndex;
                    return (
                      <button
                        key={item.id}
                        type="button"
                        role="option"
                        aria-selected={isActive}
                        className={"wb-cmdk-item" + (isActive ? " is-active" : "")}
                        onMouseEnter={() => setActiveIndex(globalIdx)}
                        onClick={() => executeItem(item)}
                      >
                        {item.dotColor && (
                          <span
                            className="wb-cmdk-dot"
                            style={{ background: item.dotColor }}
                            aria-hidden="true"
                          />
                        )}
                        <span className="wb-cmdk-item-copy">
                          <strong>{item.title}</strong>
                          {item.subtitle && <small>{item.subtitle}</small>}
                        </span>
                        {item.badge && (
                          <span className={"wb-badge " + (item.badgeTone || "is-muted")}>
                            {item.badge}
                          </span>
                        )}
                        {item.shortcut && <kbd className="wb-launcher-kbd">{item.shortcut}</kbd>}
                      </button>
                    );
                  })}
                </div>
              );
            })
          )}
        </div>

        <div className="wb-cmdk-foot">
          <span>↑↓ 选择 · Enter 执行 · Esc 关闭</span>
          <span>Cairn Command Deck</span>
        </div>
      </div>
    </div>
  );
}
