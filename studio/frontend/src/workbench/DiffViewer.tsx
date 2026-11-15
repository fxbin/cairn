/**
 * 代码差异阅读器（DiffViewer）：对齐现代 IDE / Codex 变更审查体验。
 * 自动剥离原始 Git 标头噪音，支持内联统一（Unified）与左右并排（Split）两种视图、
 * 块间未修改上下文按需展开、词级差异高亮（Word Diff）、轻量语法着色及行级审查备注。
 * 所有操作均为只读视图与本地审查元数据，不直接修改工作区。
 *
 * Author: fxbin
 */
import { Fragment, useEffect, useMemo, useState, type ReactNode } from "react";
import { desktop } from "../desktop/client";

/**
 * DiffViewMode 定义代码差异展示模式：内联统一流（unified）或左右双栏对比（split）。
 */
export type DiffViewMode = "unified" | "split";

/**
 * DiffLineStats 描述单个文件解析出的新增与删除行数统计。
 */
export interface DiffLineStats {
  additions: number;
  deletions: number;
}

interface ParsedDiffLine {
  key: number;
  kind: "add" | "delete" | "context";
  number: number;
  oldNumber: number | null;
  newNumber: number | null;
  code: string;
  wordSpan?: [number, number];
}

interface ParsedHunk {
  index: number;
  oldStart: number;
  oldCount: number;
  newStart: number;
  newCount: number;
  hiddenBeforeCount: number;
  hiddenNewStart: number;
  hiddenNewEnd: number;
  lines: ParsedDiffLine[];
}

type SplitRow = {
  key: number;
  left: ParsedDiffLine | null;
  right: ParsedDiffLine | null;
};

type SavedReviewNote = {
  id: string;
  relative: string;
  line?: string;
  body: string;
};

const KEYWORDS = new Set([
  "package",
  "import",
  "export",
  "from",
  "default",
  "func",
  "function",
  "type",
  "struct",
  "interface",
  "class",
  "extends",
  "implements",
  "const",
  "let",
  "var",
  "return",
  "if",
  "else",
  "switch",
  "case",
  "for",
  "range",
  "while",
  "break",
  "continue",
  "defer",
  "go",
  "select",
  "async",
  "await",
  "try",
  "catch",
  "finally",
  "throw",
  "new",
  "true",
  "false",
  "nil",
  "null",
  "undefined",
  "void",
  "boolean",
  "string",
  "number",
  "int",
  "int64",
  "error",
  "bool",
  "byte",
  "map",
  "chan",
]);

const TOKEN_REGEX =
  /(\/\/.*$|\/\*[\s\S]*?\*\/|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|`(?:\\.|[^`\\])*`|\b\d+(?:\.\d+)?\b|\b[A-Za-z_][A-Za-z0-9_]*\b)/g;

/**
 * 判断字符是否属于标识符组成部分，用于将词级差异边界对齐到完整单词而非半截字母。
 */
function isWordChar(ch: string | undefined): boolean {
  if (!ch) return false;
  const code = ch.charCodeAt(0);
  return (
    (code >= 48 && code <= 57) ||
    (code >= 65 && code <= 90) ||
    (code >= 97 && code <= 122) ||
    code === 95
  );
}

/**
 * 计算配对删除行与新增行内部的词级变更区间；若整行完全重写或无公共前后缀则返回 undefined。
 */
function computeWordDiffSpans(
  oldCode: string,
  newCode: string,
): { oldSpan?: [number, number]; newSpan?: [number, number] } {
  if (!oldCode || !newCode || oldCode === newCode) return {};
  if (oldCode.length > 600 || newCode.length > 600) return {};

  let prefix = 0;
  const minLen = Math.min(oldCode.length, newCode.length);
  while (prefix < minLen && oldCode[prefix] === newCode[prefix]) {
    prefix += 1;
  }

  let suffix = 0;
  while (
    suffix < minLen - prefix &&
    oldCode[oldCode.length - 1 - suffix] === newCode[newCode.length - 1 - suffix]
  ) {
    suffix += 1;
  }

  // 对齐到词边界，避免高亮切碎标识符首尾字母。
  while (
    prefix > 0 &&
    isWordChar(oldCode[prefix - 1]) &&
    (isWordChar(oldCode[prefix]) || isWordChar(newCode[prefix]))
  ) {
    prefix -= 1;
  }
  while (
    suffix > 0 &&
    isWordChar(oldCode[oldCode.length - suffix]) &&
    (isWordChar(oldCode[oldCode.length - 1 - suffix]) ||
      isWordChar(newCode[newCode.length - 1 - suffix]))
  ) {
    suffix -= 1;
  }

  const shared = prefix + suffix;
  if (shared === 0) return {};

  const oldEnd = oldCode.length - suffix;
  const newEnd = newCode.length - suffix;
  return {
    oldSpan: oldEnd > prefix ? [prefix, oldEnd] : undefined,
    newSpan: newEnd > prefix ? [prefix, newEnd] : undefined,
  };
}

/**
 * 为连续删除块与新增块配对标注行内词级高亮区间。
 */
function annotateWordDiffs(lines: ParsedDiffLine[]): void {
  let idx = 0;
  while (idx < lines.length) {
    if (lines[idx].kind !== "delete") {
      idx += 1;
      continue;
    }
    const delStart = idx;
    while (idx < lines.length && lines[idx].kind === "delete") {
      idx += 1;
    }
    const delEnd = idx;
    const addStart = idx;
    while (idx < lines.length && lines[idx].kind === "add") {
      idx += 1;
    }
    const addEnd = idx;
    const pairCount = Math.min(delEnd - delStart, addEnd - addStart);
    for (let k = 0; k < pairCount; k += 1) {
      const delLine = lines[delStart + k];
      const addLine = lines[addStart + k];
      const spans = computeWordDiffSpans(delLine.code, addLine.code);
      if (spans.oldSpan) delLine.wordSpan = spans.oldSpan;
      if (spans.newSpan) addLine.wordSpan = spans.newSpan;
    }
  }
}

/**
 * 从原始 unified diff 文本快速统计新增与删除行数（排除文件头）。
 */
export function countDiffStatsFromText(diff: string): DiffLineStats {
  if (!diff) return { additions: 0, deletions: 0 };
  let additions = 0;
  let deletions = 0;
  for (const raw of diff.split("\n")) {
    if (raw.startsWith("+++ ") || raw.startsWith("--- ")) continue;
    if (raw.startsWith("+")) additions += 1;
    else if (raw.startsWith("-")) deletions += 1;
  }
  return { additions, deletions };
}

/**
 * 将原始 Git unified diff 解析为结构化 Hunk 列表，过滤底层协议头并计算折叠未修改行区间。
 */
function parseDiffHunks(diff: string): { hunks: ParsedHunk[]; fallbackNotice: string } {
  const trimmed = diff.trim();
  if (!trimmed) {
    return { hunks: [], fallbackNotice: "" };
  }

  const rawLines = diff.replace(/\r\n/g, "\n").split("\n");
  const hunks: ParsedHunk[] = [];
  let currentHunk: ParsedHunk | null = null;
  let oldNo = 0;
  let newNo = 0;
  let prevNewEnd = 0;
  let keySeq = 1;
  const nonDiffLines: string[] = [];

  for (let i = 0; i < rawLines.length; i += 1) {
    const text = rawLines[i];
    if (i === rawLines.length - 1 && text === "") continue;

    const hunkMatch = text.match(/^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/);
    if (hunkMatch) {
      if (currentHunk) {
        annotateWordDiffs(currentHunk.lines);
      }
      const oldStart = Number(hunkMatch[1]);
      const oldCount = hunkMatch[2] !== undefined ? Number(hunkMatch[2]) : 1;
      const newStart = Number(hunkMatch[3]);
      const newCount = hunkMatch[4] !== undefined ? Number(hunkMatch[4]) : 1;
      const hiddenStart = prevNewEnd + 1;
      const hiddenEnd = newStart - 1;
      const hiddenBeforeCount = hiddenEnd >= hiddenStart ? hiddenEnd - hiddenStart + 1 : 0;

      currentHunk = {
        index: hunks.length,
        oldStart,
        oldCount,
        newStart,
        newCount,
        hiddenBeforeCount,
        hiddenNewStart: hiddenStart,
        hiddenNewEnd: hiddenEnd,
        lines: [],
      };
      hunks.push(currentHunk);
      oldNo = oldStart;
      newNo = newStart;
      prevNewEnd = newCount > 0 ? newStart + newCount - 1 : prevNewEnd;
      continue;
    }

    if (
      /^(diff --git |index |--- |\+\+\+ |new file mode |deleted file mode |old mode |new mode |similarity index |rename from |rename to |Binary files |\\ No newline)/.test(
        text,
      )
    ) {
      if (text.startsWith("Binary files ")) {
        nonDiffLines.push("二进制文件差异，不支持文本级对比。");
      }
      continue;
    }

    if (!currentHunk) {
      if (text.trim()) nonDiffLines.push(text);
      continue;
    }

    if (text.startsWith("+")) {
      const n = newNo++;
      currentHunk.lines.push({
        key: keySeq++,
        kind: "add",
        number: n,
        oldNumber: null,
        newNumber: n,
        code: text.slice(1),
      });
    } else if (text.startsWith("-")) {
      const o = oldNo++;
      currentHunk.lines.push({
        key: keySeq++,
        kind: "delete",
        number: o,
        oldNumber: o,
        newNumber: null,
        code: text.slice(1),
      });
    } else {
      const o = oldNo++;
      const n = newNo++;
      currentHunk.lines.push({
        key: keySeq++,
        kind: "context",
        number: n,
        oldNumber: o,
        newNumber: n,
        code: text.startsWith(" ") ? text.slice(1) : text,
      });
      if (n > prevNewEnd) {
        prevNewEnd = n;
      }
    }
  }

  if (currentHunk) {
    annotateWordDiffs(currentHunk.lines);
  }

  return {
    hunks,
    fallbackNotice: hunks.length === 0 ? nonDiffLines.join("\n") : "",
  };
}

/**
 * 将单个 Hunk 内的行序列配对为左右并排（左旧右新）对齐行结构。
 */
function buildHunkSplitRows(lines: ParsedDiffLine[]): SplitRow[] {
  const rows: SplitRow[] = [];
  let delBuf: ParsedDiffLine[] = [];
  let addBuf: ParsedDiffLine[] = [];

  const flush = () => {
    if (delBuf.length === 0 && addBuf.length === 0) return;
    const maxLen = Math.max(delBuf.length, addBuf.length);
    for (let i = 0; i < maxLen; i += 1) {
      const left = delBuf[i] ?? null;
      const right = addBuf[i] ?? null;
      rows.push({
        key: left ? left.key : right!.key,
        left,
        right,
      });
    }
    delBuf = [];
    addBuf = [];
  };

  for (const line of lines) {
    if (line.kind === "context") {
      flush();
      rows.push({ key: line.key, left: line, right: line });
    } else if (line.kind === "delete") {
      if (addBuf.length > 0) flush();
      delBuf.push(line);
    } else {
      addBuf.push(line);
    }
  }
  flush();
  return rows;
}

/**
 * 对单段纯文本代码做轻量零依赖语法着色，提升长段代码的可读性。
 */
function renderSyntaxSegment(segment: string, keyPrefix: string): ReactNode[] {
  if (!segment) return [];
  if (segment.length > 400) return [segment];

  const nodes: ReactNode[] = [];
  let lastIndex = 0;
  TOKEN_REGEX.lastIndex = 0;
  let match: RegExpExecArray | null;
  let seq = 0;

  while ((match = TOKEN_REGEX.exec(segment)) !== null) {
    if (match.index > lastIndex) {
      nodes.push(segment.slice(lastIndex, match.index));
    }
    const token = match[0];
    let cls = "";
    if (token.startsWith("//") || token.startsWith("/*")) {
      cls = "wb-tok-comment";
    } else if (token.startsWith('"') || token.startsWith("'") || token.startsWith("`")) {
      cls = "wb-tok-str";
    } else if (token.charCodeAt(0) >= 48 && token.charCodeAt(0) <= 57) {
      cls = "wb-tok-num";
    } else if (KEYWORDS.has(token)) {
      cls = "wb-tok-kw";
    }

    if (cls) {
      nodes.push(
        <span key={`${keyPrefix}-${seq++}`} className={cls}>
          {token}
        </span>,
      );
    } else {
      nodes.push(token);
    }
    lastIndex = match.index + token.length;
  }

  if (lastIndex < segment.length) {
    nodes.push(segment.slice(lastIndex));
  }
  return nodes;
}

/**
 * 渲染单行代码内容，同时叠加行内词级变更底色与轻量语法高亮。
 */
function renderCodeLine(code: string, wordSpan?: [number, number]): ReactNode {
  if (!code) return " ";
  if (!wordSpan) {
    return renderSyntaxSegment(code, "s");
  }
  const [start, end] = wordSpan;
  const before = code.slice(0, start);
  const hit = code.slice(start, end);
  const after = code.slice(end);
  return (
    <>
      {renderSyntaxSegment(before, "b")}
      <mark className="wb-cr-word-hit">{renderSyntaxSegment(hit, "h")}</mark>
      {renderSyntaxSegment(after, "a")}
    </>
  );
}

/**
 * 将已保存的行级审查备注映射到对应代码行的唯一 key。
 */
function buildInlineNotesByRowKey(
  hunks: ParsedHunk[],
  notes: SavedReviewNote[],
): Map<number, SavedReviewNote[]> {
  const anchorByLineNo = new Map<number, number>();
  for (const hunk of hunks) {
    for (const line of hunk.lines) {
      const prev = anchorByLineNo.get(line.number);
      if (prev === undefined || line.kind !== "delete") {
        anchorByLineNo.set(line.number, line.key);
      }
    }
  }
  const byRowKey = new Map<number, SavedReviewNote[]>();
  for (const item of notes) {
    if (!item.line) continue;
    const numericLine = Number(item.line);
    if (!Number.isFinite(numericLine)) continue;
    const anchorKey = anchorByLineNo.get(numericLine);
    if (anchorKey === undefined) continue;
    const bucket = byRowKey.get(anchorKey);
    if (bucket) bucket.push(item);
    else byRowKey.set(anchorKey, [item]);
  }
  return byRowKey;
}

/**
 * 合并双栏单行左右单元格关联的行级备注并按 ID 去重。
 */
function collectSplitRowNotes(
  row: SplitRow,
  notesByKey: Map<number, SavedReviewNote[]>,
): SavedReviewNote[] {
  const leftNotes = row.left ? notesByKey.get(row.left.key) || [] : [];
  const rightNotes =
    row.right && (!row.left || row.right.key !== row.left.key)
      ? notesByKey.get(row.right.key) || []
      : [];
  if (leftNotes.length === 0) return rightNotes;
  if (rightNotes.length === 0) return leftNotes;
  const seen = new Set<string>();
  const merged: SavedReviewNote[] = [];
  for (const item of [...leftNotes, ...rightNotes]) {
    if (!seen.has(item.id)) {
      seen.add(item.id);
      merged.push(item);
    }
  }
  return merged;
}

/**
 * DiffViewer 组件渲染单个文件的干净代码块、折叠上下文与行级批注交互。
 */
export default function DiffViewer({
  text,
  relative,
  workspace,
  viewMode = "unified",
  maxInitialLines,
  onComment,
}: {
  text: string;
  relative: string;
  workspace: string;
  viewMode?: DiffViewMode;
  maxInitialLines?: number;
  onComment: (line: string, body: string) => Promise<void>;
}) {
  const [selected, setSelected] = useState<number | null>(null);
  const [note, setNote] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [existing, setExisting] = useState<SavedReviewNote[]>([]);
  const [expandedGaps, setExpandedGaps] = useState<Record<string, boolean>>({});
  const [fullFileLines, setFullFileLines] = useState<string[] | null>(null);
  const [loadingContext, setLoadingContext] = useState(false);
  const [showAllLines, setShowAllLines] = useState(false);

  useEffect(() => {
    setExpandedGaps({});
    setFullFileLines(null);
    setSelected(null);
    setShowAllLines(false);
  }, [workspace, relative]);

  useEffect(() => {
    let active = true;
    if (typeof desktop.listReviewNotes !== "function") return;
    void desktop
      .listReviewNotes(workspace, relative)
      .then((data) => {
        if (active) setExisting(data);
      })
      .catch(() => {
        if (active) setExisting([]);
      });
    return () => {
      active = false;
    };
  }, [workspace, relative]);

  const { hunks, fallbackNotice } = useMemo(() => parseDiffHunks(text), [text]);
  const totalDiffLines = useMemo(
    () => hunks.reduce((sum, hunk) => sum + hunk.lines.length, 0),
    [hunks],
  );
  const { visibleHunks, truncatedLineCount } = useMemo(() => {
    if (
      !maxInitialLines ||
      showAllLines ||
      totalDiffLines <= maxInitialLines + 40
    ) {
      return { visibleHunks: hunks, truncatedLineCount: 0 };
    }
    const sliced: ParsedHunk[] = [];
    let remaining = maxInitialLines;
    for (const hunk of hunks) {
      if (remaining <= 0) break;
      if (hunk.lines.length <= remaining) {
        sliced.push(hunk);
        remaining -= hunk.lines.length;
      } else {
        sliced.push({
          ...hunk,
          lines: hunk.lines.slice(0, remaining),
        });
        remaining = 0;
      }
    }
    const shown = maxInitialLines - remaining;
    return {
      visibleHunks: sliced,
      truncatedLineCount: Math.max(0, totalDiffLines - shown),
    };
  }, [hunks, maxInitialLines, showAllLines, totalDiffLines]);

  const inlineNotesByRow = useMemo(
    () => buildInlineNotesByRowKey(hunks, existing),
    [hunks, existing],
  );

  const toggleGap = async (gapKey: string) => {
    if (expandedGaps[gapKey]) {
      setExpandedGaps((prev) => ({ ...prev, [gapKey]: false }));
      return;
    }
    if (fullFileLines === null && typeof desktop.previewFile === "function" && !loadingContext) {
      setLoadingContext(true);
      try {
        const preview = await desktop.previewFile(workspace, relative);
        if (!preview.binary && preview.text) {
          setFullFileLines(preview.text.replace(/\r\n/g, "\n").split("\n"));
        } else {
          setFullFileLines([]);
        }
      } catch {
        setFullFileLines([]);
      } finally {
        setLoadingContext(false);
      }
    }
    setExpandedGaps((prev) => ({ ...prev, [gapKey]: true }));
  };

  const submitComment = () => {
    const trimmed = note.trim();
    if (!trimmed || saving || selected === null) return;
    setSaving(true);
    setError("");
    void onComment(String(selected), trimmed)
      .then(() => {
        setNote("");
        setSelected(null);
        if (typeof desktop.listReviewNotes === "function") {
          void desktop
            .listReviewNotes(workspace, relative)
            .then((data) => setExisting(data))
            .catch(() => {});
        }
      })
      .catch((err) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSaving(false));
  };

  const renderContextDivider = (
    gapKey: string,
    count: number,
    startLine: number,
    endLine: number,
  ) => {
    if (count <= 0) return null;
    const isExpanded = Boolean(expandedGaps[gapKey]);
    const canExpand = typeof desktop.previewFile === "function";

    if (isExpanded && fullFileLines && fullFileLines.length >= startLine) {
      const slice = fullFileLines.slice(
        Math.max(0, startLine - 1),
        Math.min(fullFileLines.length, endLine),
      );
      return (
        <Fragment key={gapKey}>
          <button
            type="button"
            className="wb-cr-hunk-divider is-expanded"
            onClick={() => void toggleGap(gapKey)}
          >
            <span className="wb-cr-hunk-arrows" aria-hidden="true">
              ⌄
            </span>
            <span>收起 {slice.length} 行未修改代码</span>
          </button>
          {slice.map((ctxLine, offset) => {
            const ln = startLine + offset;
            if (viewMode === "split") {
              return (
                <div key={`${gapKey}-${ln}`} className="wb-cr-split-row">
                  <div className="wb-cr-split-cell wb-diff-context is-expanded-ctx">
                    <span className="wb-diff-line-number is-static">{ln}</span>
                    <code className="wb-cr-code">{renderCodeLine(ctxLine)}</code>
                  </div>
                  <div className="wb-cr-split-cell wb-diff-context is-expanded-ctx">
                    <span className="wb-diff-line-number is-static">{ln}</span>
                    <code className="wb-cr-code">{renderCodeLine(ctxLine)}</code>
                  </div>
                </div>
              );
            }
            return (
              <div key={`${gapKey}-${ln}`} className="wb-cr-row wb-diff-context is-expanded-ctx">
                <span className="wb-diff-line-number is-static">{ln}</span>
                <code className="wb-cr-code">{renderCodeLine(ctxLine)}</code>
              </div>
            );
          })}
        </Fragment>
      );
    }

    return (
      <button
        key={gapKey}
        type="button"
        disabled={!canExpand}
        className="wb-cr-hunk-divider"
        title={canExpand ? "点击展开未修改上下文代码" : undefined}
        onClick={() => void toggleGap(gapKey)}
      >
        <span className="wb-cr-hunk-arrows" aria-hidden="true">
          ⌃⌄
        </span>
        <span>
          {loadingContext ? "正在读取上下文…" : `${count} 行未修改代码`}
        </span>
      </button>
    );
  };

  if (hunks.length === 0) {
    return (
      <div className="wb-cr-empty-diff">
        {fallbackNotice || "当前范围内暂无文本行级差异。"}
      </div>
    );
  }

  return (
    <div className={"wb-diff-viewer wb-cr-diff mode-" + viewMode}>
      <div className="wb-cr-scroll-x">
        {viewMode === "split" ? (
          <div className="wb-cr-split-table">
            {visibleHunks.map((hunk) => {
              const rows = buildHunkSplitRows(hunk.lines);
              const gapKey = `hunk-${hunk.index}`;
              return (
                <Fragment key={gapKey}>
                  {renderContextDivider(
                    gapKey,
                    hunk.hiddenBeforeCount,
                    hunk.hiddenNewStart,
                    hunk.hiddenNewEnd,
                  )}
                  {rows.map((row) => {
                    const rowNotes = collectSplitRowNotes(row, inlineNotesByRow);
                    const leftNo = row.left?.oldNumber ?? row.left?.number ?? null;
                    const rightNo = row.right?.newNumber ?? row.right?.number ?? null;
                    return (
                      <Fragment key={`sp-${row.key}`}>
                        <div className="wb-cr-split-row">
                          {row.left ? (
                            <div className={"wb-cr-split-cell wb-diff-" + row.left.kind}>
                              <button
                                type="button"
                                aria-label={`为第 ${leftNo} 行添加意见`}
                                onClick={() => {
                                  setSelected(leftNo);
                                  setError("");
                                }}
                                className="wb-diff-line-number"
                              >
                                <span className="wb-cr-ln-text">{leftNo}</span>
                                <span className="wb-cr-ln-plus" aria-hidden="true">+</span>
                              </button>
                              <code className="wb-cr-code">
                                {renderCodeLine(row.left.code, row.left.wordSpan)}
                              </code>
                            </div>
                          ) : (
                            <div className="wb-cr-split-cell wb-diff-empty" aria-hidden="true">
                              <span className="wb-diff-line-number is-placeholder" />
                              <code className="wb-cr-code"> </code>
                            </div>
                          )}

                          {row.right ? (
                            <div className={"wb-cr-split-cell wb-diff-" + row.right.kind}>
                              <button
                                type="button"
                                aria-label={`为第 ${rightNo} 行添加意见`}
                                onClick={() => {
                                  setSelected(rightNo);
                                  setError("");
                                }}
                                className="wb-diff-line-number"
                              >
                                <span className="wb-cr-ln-text">{rightNo}</span>
                                <span className="wb-cr-ln-plus" aria-hidden="true">+</span>
                              </button>
                              <code className="wb-cr-code">
                                {renderCodeLine(row.right.code, row.right.wordSpan)}
                              </code>
                            </div>
                          ) : (
                            <div className="wb-cr-split-cell wb-diff-empty" aria-hidden="true">
                              <span className="wb-diff-line-number is-placeholder" />
                              <code className="wb-cr-code"> </code>
                            </div>
                          )}
                        </div>
                        {rowNotes.map((item) => (
                          <div key={item.id} className="wb-inline-note-card is-split">
                            <div className="wb-inline-note-meta">
                              <span className="wb-badge is-warn">第 {item.line} 行审查备注</span>
                              <span className="wb-hash-pill">
                                {relative}:{item.line}
                              </span>
                            </div>
                            <div>{item.body}</div>
                          </div>
                        ))}
                      </Fragment>
                    );
                  })}
                </Fragment>
              );
            })}
          </div>
        ) : (
          <div className="wb-cr-unified-table">
            {visibleHunks.map((hunk) => {
              const gapKey = `hunk-${hunk.index}`;
              return (
                <Fragment key={gapKey}>
                  {renderContextDivider(
                    gapKey,
                    hunk.hiddenBeforeCount,
                    hunk.hiddenNewStart,
                    hunk.hiddenNewEnd,
                  )}
                  {hunk.lines.map((line) => {
                    const rowNotes = inlineNotesByRow.get(line.key) || [];
                    const displayLineNo =
                      line.kind === "delete"
                        ? (line.oldNumber ?? line.number)
                        : (line.newNumber ?? line.number);
                    return (
                      <Fragment key={line.key}>
                        <div className={"wb-cr-row wb-diff-" + line.kind}>
                          <button
                            type="button"
                            aria-label={`为第 ${displayLineNo} 行添加意见`}
                            onClick={() => {
                              setSelected(displayLineNo);
                              setError("");
                            }}
                            className="wb-diff-line-number"
                          >
                            <span className="wb-cr-ln-text">{displayLineNo}</span>
                            <span className="wb-cr-ln-plus" aria-hidden="true">+</span>
                          </button>
                          <code className="wb-cr-code">
                            {renderCodeLine(line.code, line.wordSpan)}
                          </code>
                        </div>
                        {rowNotes.map((item) => (
                          <div key={item.id} className="wb-inline-note-card">
                            <div className="wb-inline-note-meta">
                              <span className="wb-badge is-warn">第 {item.line} 行审查备注</span>
                              <span className="wb-hash-pill">
                                {relative}:{item.line}
                              </span>
                            </div>
                            <div>{item.body}</div>
                          </div>
                        ))}
                      </Fragment>
                    );
                  })}
                </Fragment>
              );
            })}
          </div>
        )}
      </div>

      {truncatedLineCount > 0 && (
        <button
          type="button"
          className="wb-cr-more-lines-btn"
          onClick={() => setShowAllLines(true)}
        >
          <span>
            已预览前 {totalDiffLines - truncatedLineCount} 行差异（共 {totalDiffLines} 行）
          </span>
          <strong>展开剩余 {truncatedLineCount} 行差异 ▾</strong>
        </button>
      )}

      {existing.length > 0 && (
        <div
          className="wb-review-notes"
          data-testid="review-notes"
          style={{ padding: "10px 14px", borderTop: "1px solid var(--wb-line)" }}
        >
          <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 4 }}>
            <strong>已保存的审查备注</strong>
            <span className="wb-badge is-muted">{existing.length}</span>
          </div>
          {existing.map((item) => (
            <p key={item.id} style={{ margin: "3px 0", fontSize: 12 }}>
              第 {item.line || "?"} 行 · {item.body}
            </p>
          ))}
        </div>
      )}

      {selected !== null && (
        <div className="wb-line-comment">
          <div>
            <strong>{relative}</strong>
            <span className="wb-badge is-accent">第 {selected} 行</span>
            <button type="button" aria-label="关闭批注编辑" onClick={() => setSelected(null)}>
              ×
            </button>
          </div>
          <textarea
            aria-label="行级审查意见"
            value={note}
            onChange={(event) => setNote(event.target.value)}
            onKeyDown={(event) => {
              const ime = event.nativeEvent.isComposing || event.nativeEvent.keyCode === 229;
              if (event.key === "Enter" && (event.metaKey || event.ctrlKey) && !ime) {
                event.preventDefault();
                submitComment();
              }
            }}
            placeholder="描述需要进一步检查或修改的地方…（Ctrl/⌘ + Enter 快速保存）"
          />
          {error && <p role="alert">{error}</p>}
          <div className="wb-actions">
            <button
              type="button"
              className="wb-primary"
              disabled={!note.trim() || saving}
              onClick={submitComment}
            >
              {saving ? "保存中…" : "保存并带回会话"}
            </button>
            <button type="button" onClick={() => setSelected(null)}>
              取消
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
