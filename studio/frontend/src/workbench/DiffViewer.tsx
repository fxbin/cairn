/**
 * 统一 Diff 阅读器：把原始 Git 文本拆为可浏览的差异行，不改变工作区。
 * 行级评论是本地 review metadata；真正修改仍必须由后续 Agent Run 执行。
 * Author: fxbin
 */
import { Fragment, useEffect, useMemo, useState } from "react";
import { desktop } from "../desktop/client";

type Line = {
  key: number;
  kind: "add" | "delete" | "context" | "header";
  number: number | null;
  text: string;
};

type SavedReviewNote = {
  id: string;
  relative: string;
  line?: string;
  body: string;
};

/**
 * 将原始 unified diff 文本解析为带行号与语义分类的行切片。
 * 新增行与上下文行使用新文件行号，删除行使用旧文件行号，标头行无行号。
 */
function linesOf(diff: string): Line[] {
  let oldNo = 0;
  let newNo = 0;
  return diff.split("\n").map((text, index) => {
    const hunk = text.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
    if (hunk) {
      oldNo = Number(hunk[1]);
      newNo = Number(hunk[2]);
      return { key: index, kind: "header", number: null, text };
    }
    if (/^(diff --git |index |--- |\+\+\+ |new file mode |deleted file mode |\\ No newline)/.test(text)) {
      return { key: index, kind: "header", number: null, text };
    }
    if (text.startsWith("+")) {
      return { key: index, kind: "add", number: newNo++, text };
    }
    if (text.startsWith("-")) {
      return { key: index, kind: "delete", number: oldNo++, text };
    }
    if (oldNo > 0 && newNo > 0) {
      oldNo += 1;
      return { key: index, kind: "context", number: newNo++, text };
    }
    return { key: index, kind: "header", number: null, text };
  });
}

/**
 * 将已保存的行级审查备注映射到对应 Diff 行的唯一锚点索引。
 * 当同一行号同时存在于删除行与新增/上下文行时，优先挂载在新增或上下文行下方以避免重复渲染。
 */
function buildInlineNotesByRowKey(lines: Line[], notes: SavedReviewNote[]): Map<number, SavedReviewNote[]> {
  const anchorByLineNo = new Map<number, number>();
  for (const line of lines) {
    if (line.number === null) continue;
    const previousKey = anchorByLineNo.get(line.number);
    if (previousKey === undefined || line.kind !== "delete") {
      anchorByLineNo.set(line.number, line.key);
    }
  }
  const byRowKey = new Map<number, SavedReviewNote[]>();
  for (const item of notes) {
    if (item.line === undefined || item.line === "") continue;
    const numericLine = Number(item.line);
    if (!Number.isFinite(numericLine)) continue;
    const anchorKey = anchorByLineNo.get(numericLine);
    if (anchorKey === undefined) continue;
    const bucket = byRowKey.get(anchorKey);
    if (bucket) {
      bucket.push(item);
    } else {
      byRowKey.set(anchorKey, [item]);
    }
  }
  return byRowKey;
}

/**
 * 渲染支持行级内联审查卡片与 IME 安全快捷键的只读 Diff 视图。
 */
export default function DiffViewer({
  text,
  relative,
  workspace,
  onComment,
}: {
  text: string;
  relative: string;
  workspace: string;
  onComment: (line: string, body: string) => Promise<void>;
}) {
  const [selected, setSelected] = useState<number | null>(null);
  const [note, setNote] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [existing, setExisting] = useState<SavedReviewNote[]>([]);

  useEffect(() => {
    let active = true;
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

  const lines = useMemo(() => linesOf(text), [text]);
  const inlineNotesByRow = useMemo(() => buildInlineNotesByRowKey(lines, existing), [lines, existing]);

  const submitComment = () => {
    const trimmed = note.trim();
    if (!trimmed || saving || selected === null) return;
    setSaving(true);
    setError("");
    void onComment(String(selected), trimmed)
      .then(() => {
        setNote("");
        setSelected(null);
        void desktop
          .listReviewNotes(workspace, relative)
          .then((data) => setExisting(data))
          .catch(() => {});
      })
      .catch((err) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSaving(false));
  };

  return (
    <div className="wb-diff-viewer">
      <div className="wb-diff-lines">
        {lines.map((line) => {
          const rowNotes = inlineNotesByRow.get(line.key) || [];
          return (
            <Fragment key={line.key}>
              <div className={"wb-diff-row wb-diff-" + line.kind}>
                <button
                  type="button"
                  aria-label={line.number === null ? "差异标头" : "为第 " + line.number + " 行添加意见"}
                  disabled={line.number === null}
                  onClick={() => {
                    setSelected(line.number);
                    setError("");
                  }}
                  className="wb-diff-line-number"
                >
                  {line.number ?? ""}
                </button>
                <code>{line.text || " "}</code>
              </div>
              {rowNotes.map((item) => (
                <div key={item.id} className="wb-inline-note-card">
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap", marginBottom: 4 }}>
                    <span className="wb-badge is-warn">第 {item.line} 行审查备注</span>
                    <span className="wb-hash-pill">{relative}:{item.line}</span>
                  </div>
                  <div>{item.body}</div>
                </div>
              ))}
            </Fragment>
          );
        })}
      </div>
      {existing.length > 0 && (
        <div className="wb-review-notes" data-testid="review-notes" style={{ padding: "12px 16px", borderTop: "1px solid var(--wb-line)" }}>
          <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 6 }}>
            <strong>已保存的审查备注</strong>
            <span className="wb-badge is-muted">{existing.length}</span>
          </div>
          {existing.map((item) => (
            <p key={item.id} style={{ margin: "4px 0", fontSize: 12 }}>
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
            <button type="button" className="wb-primary" disabled={!note.trim() || saving} onClick={submitComment}>
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
