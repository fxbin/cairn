/**
 * INPUT: 草稿文本与光标位置、工作区文件树节点、可选 FilePreview 读取回调。
 * OUTPUT: @文件引用探测/补全/受控展开、/快捷指令目录与用户消息引用胶囊解析纯函数集。
 * POS: studio/frontend/src/workbench 指令舱上下文与命令解析辅助模块，只读复用工作区预览边界。
 *
 * Author: fxbin
 */
import type { FileNode, FilePreview } from "../desktop/types";

const MAX_REFERENCE_PREVIEW_CHARS = 8192;
const REFERENCE_BLOCK_PREFIX = "\n\n[引用文件 @";

/**
 * AttachedFileChip 描述指令舱已挂载的单个工作区文件上下文胶囊。
 */
export interface AttachedFileChip {
  relative: string;
  name: string;
  dir: string;
  size?: number;
}

/**
 * ComposerSlashCommandId 定义指令舱内置 / 快捷指令的唯一标识。
 */
export type ComposerSlashCommandId =
  | "review"
  | "plan"
  | "changes"
  | "evidence"
  | "files"
  | "readonly"
  | "fork"
  | "workflow"
  | "doctor"
  | "clear";

/**
 * ComposerSlashCommand 描述指令舱中一条可搜索与可执行的 / 快捷指令。
 */
export interface ComposerSlashCommand {
  id: ComposerSlashCommandId;
  name: string;
  label: string;
  description: string;
  hint?: string;
}

/**
 * COMPOSER_SLASH_COMMANDS 登记桌面指令舱支持的全部 / 快捷指令。
 */
export const COMPOSER_SLASH_COMMANDS: readonly ComposerSlashCommand[] = [
  {
    id: "review",
    name: "/review",
    label: "代码审查",
    description: "填入当前 Git 变更与风险审查任务提示词",
    hint: "填入草稿",
  },
  {
    id: "plan",
    name: "/plan",
    label: "只读规划",
    description: "切换至只读探索模式并填入实现计划提示词",
    hint: "只读 · 填稿",
  },
  {
    id: "changes",
    name: "/changes",
    label: "检查变更",
    description: "打开右侧工作台「变更」审查面板",
    hint: "⌃⇧G",
  },
  {
    id: "evidence",
    name: "/evidence",
    label: "验证与证据",
    description: "打开当前运行的 Verifier 证据与 Journal 时间线",
    hint: "⌘E",
  },
  {
    id: "files",
    name: "/files",
    label: "项目文件",
    description: "在右侧工作台浏览工作区目录树与只读源码",
    hint: "⌘P",
  },
  {
    id: "readonly",
    name: "/readonly",
    label: "切换只读",
    description: "切换「只读探索」与「默认审批 ask」运行权限",
    hint: "权限切换",
  },
  {
    id: "fork",
    name: "/fork",
    label: "分支对话",
    description: "基于当前选中 Run 创建独立分支追问",
    hint: "⌘⇧S",
  },
  {
    id: "workflow",
    name: "/workflow",
    label: "工作流编排",
    description: "跳转到线性受控工作流编辑与预演页面",
    hint: "页面",
  },
  {
    id: "doctor",
    name: "/doctor",
    label: "环境诊断",
    description: "打开设置并执行当前工作区只读 Doctor 自检",
    hint: "只读自检",
  },
  {
    id: "clear",
    name: "/clear",
    label: "新建空白任务",
    description: "清空当前草稿并回到空白新建任务态",
    hint: "重置",
  },
];

/**
 * ComposerActiveTrigger 描述当前光标位置命中的 / 或 @ 浮层触发区间。
 */
export type ComposerActiveTrigger =
  | { kind: "slash"; query: string; start: number; end: number }
  | { kind: "mention"; query: string; start: number; end: number };

/**
 * ParsedUserAttachment 表示用户消息正文中被受控展开的单个引用文件块。
 */
export interface ParsedUserAttachment {
  path: string;
  snippet: string;
}

/**
 * 根据草稿文本与当前光标偏移识别 / 快捷指令或 @ 文件引用浮层触发器。
 * 忽略词内字符（如邮箱地址 a@b.com），与 TUI reference.go 匹配规则保持一致。
 */
export function detectComposerTrigger(
  text: string,
  caretIndex: number,
): ComposerActiveTrigger | null {
  const safeCaret = Math.max(0, Math.min(caretIndex, text.length));
  const before = text.slice(0, safeCaret);

  const slashMatch = /^\/([^\s]*)$/.exec(before);
  if (slashMatch) {
    return {
      kind: "slash",
      query: slashMatch[1],
      start: 0,
      end: before.length,
    };
  }

  const mentionMatch = /(?:^|\s)@([^\s@]*)$/.exec(before);
  if (mentionMatch) {
    const atOffset = mentionMatch[0].indexOf("@");
    const start = mentionMatch.index + atOffset;
    return {
      kind: "mention",
      query: mentionMatch[1],
      start,
      end: before.length,
    };
  }

  return null;
}

/**
 * 按关键字过滤指令舱 / 快捷指令列表，支持按命令名、标题或说明模糊匹配。
 */
export function filterSlashCommands(query: string): ComposerSlashCommand[] {
  const normalized = query.trim().toLowerCase().replace(/^\//, "");
  if (!normalized) return [...COMPOSER_SLASH_COMMANDS];
  return COMPOSER_SLASH_COMMANDS.filter((item) => {
    const haystack = `${item.name} ${item.label} ${item.description}`.toLowerCase();
    return haystack.includes(normalized);
  });
}

/**
 * 将层级 FileNode 树扁平化为可直接供指令舱与命令面板检索的文件胶囊条目。
 */
export function flattenWorkspaceFiles(
  node: FileNode | null,
  limit = 1500,
): AttachedFileChip[] {
  if (!node) return [];
  const items: AttachedFileChip[] = [];
  const stack: FileNode[] = [node];
  while (stack.length > 0 && items.length < limit) {
    const current = stack.pop()!;
    if (current.kind === "dir") {
      const children = current.children || [];
      for (let i = children.length - 1; i >= 0; i -= 1) {
        stack.push(children[i]);
      }
      continue;
    }
    if (current.relative && current.relative !== ".") {
      const slashIdx = current.relative.lastIndexOf("/");
      items.push({
        name: current.name,
        relative: current.relative,
        dir: slashIdx > 0 ? current.relative.slice(0, slashIdx) : "",
        size: current.size,
      });
    }
  }
  return items;
}

/**
 * 按文件名精确/前缀/路径包含优先级匹配工作区文件候选项。
 */
export function matchWorkspaceFiles(
  files: readonly AttachedFileChip[],
  query: string,
  limit = 8,
): AttachedFileChip[] {
  const q = query.trim().toLowerCase();
  if (!q) {
    return files.slice(0, limit);
  }
  const scored: Array<{ item: AttachedFileChip; score: number }> = [];
  for (const item of files) {
    const nameLower = item.name.toLowerCase();
    const relLower = item.relative.toLowerCase();
    if (nameLower === q) {
      scored.push({ item, score: 0 });
    } else if (nameLower.startsWith(q)) {
      scored.push({ item, score: 1 });
    } else if (nameLower.includes(q)) {
      scored.push({ item, score: 2 });
    } else if (relLower.includes(q)) {
      scored.push({ item, score: 3 });
    }
  }
  scored.sort(
    (a, b) => a.score - b.score || a.item.relative.length - b.item.relative.length,
  );
  return scored.slice(0, limit).map((entry) => entry.item);
}

/**
 * 将选中的文件相对路径回填至草稿中的当前 @ 触发区间，返回更新文本与新光标位置。
 */
export function applyMentionSelection(
  text: string,
  trigger: ComposerActiveTrigger,
  relativePath: string,
): { text: string; caret: number } {
  const head = text.slice(0, trigger.start);
  const tail = text.slice(trigger.end);
  const insertion = tail.startsWith(" ") ? `@${relativePath}` : `@${relativePath} `;
  const nextText = head + insertion + tail;
  const caret = head.length + `@${relativePath} `.length;
  return { text: nextText, caret };
}

/**
 * 从任务文本中提取去重后的内联 @相对路径 引用列表，忽略词内 @ 字符。
 */
export function extractInlineMentionPaths(text: string): string[] {
  const regex = /(?:^|\s)@([^\s@]+)/g;
  const seen = new Set<string>();
  const out: string[] = [];
  let match: RegExpExecArray | null;
  while ((match = regex.exec(text)) !== null) {
    const rel = match[1];
    if (rel && !seen.has(rel)) {
      seen.add(rel);
      out.push(rel);
    }
  }
  return out;
}

/**
 * 格式化单条受控读取的文件上下文附加块；超限时显式标注字节总量并引导 read_file 读取。
 */
function formatReferenceBlock(relative: string, preview: FilePreview): string {
  if (preview.binary) {
    return `[引用文件 @${relative}]\n（二进制文件，共 ${preview.size} 字节，无法内联文本内容；请使用外部编辑器或允许的工具查看）`;
  }
  const rawText = preview.text ?? "";
  if (rawText.length > MAX_REFERENCE_PREVIEW_CHARS || preview.truncated) {
    const clipped = rawText.slice(0, MAX_REFERENCE_PREVIEW_CHARS);
    const totalBytes = preview.size || rawText.length;
    return `[引用文件 @${relative}]\n${clipped}\n…（已截断，共 ${totalBytes} 字节；完整内容请用 read_file 读取 ${relative}）`;
  }
  return `[引用文件 @${relative}]\n${rawText}`;
}

/**
 * 将已挂载的文件胶囊与草稿中的 @相对路径 经工作区预览边界安全展开并附加至任务正文。
 * 无引用文件或运行时未提供 previewFile 回调时原样返回 rawTask；遇越界或敏感路径报错时失败关闭。
 */
export async function expandComposerTaskWithContext({
  workspace,
  rawTask,
  attachedFiles,
  previewFile,
}: {
  workspace: string;
  rawTask: string;
  attachedFiles: readonly AttachedFileChip[];
  previewFile?: (workspace: string, relative: string) => Promise<FilePreview>;
}): Promise<string> {
  const orderedPaths: string[] = [];
  const seen = new Set<string>();

  for (const item of attachedFiles) {
    if (item.relative && !seen.has(item.relative)) {
      seen.add(item.relative);
      orderedPaths.push(item.relative);
    }
  }
  for (const rel of extractInlineMentionPaths(rawTask)) {
    if (!seen.has(rel)) {
      seen.add(rel);
      orderedPaths.push(rel);
    }
  }

  if (orderedPaths.length === 0 || typeof previewFile !== "function") {
    return rawTask;
  }

  const blocks: string[] = [];
  for (const rel of orderedPaths) {
    const preview = await previewFile(workspace, rel);
    blocks.push(formatReferenceBlock(rel, preview));
  }

  return `${rawTask}\n\n${blocks.join("\n\n")}`;
}

/**
 * 将用户消息文本拆分为主体指令与末尾附加的 [引用文件 @...] 上下文列表，便于对话流美观折叠呈现。
 */
export function parseUserMessageAttachments(text: string): {
  body: string;
  refs: ParsedUserAttachment[];
} {
  const firstIdx = text.indexOf(REFERENCE_BLOCK_PREFIX);
  if (firstIdx < 0) {
    return { body: text, refs: [] };
  }

  const body = text.slice(0, firstIdx).trimEnd();
  const tail = text.slice(firstIdx + 2);
  const segments = tail.split(/\n\n(?=\[引用文件 @)/);
  const refs: ParsedUserAttachment[] = [];

  for (const seg of segments) {
    const match = /^\[引用文件 @([^\]]+)\]\n?([\s\S]*)$/.exec(seg);
    if (match) {
      refs.push({
        path: match[1].trim(),
        snippet: match[2],
      });
    }
  }

  if (refs.length === 0) {
    return { body: text, refs: [] };
  }
  return { body, refs };
}
