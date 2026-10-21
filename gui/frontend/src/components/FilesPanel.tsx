import type { WorkspaceStatus } from "../desktop/types";

/** 文件/改动区：F01 先展示工作区边界与占位，F07/F08 填充真实树与 Diff。 */
export function FilesPanel({ workspace }: { workspace: WorkspaceStatus | null }) {
  return (
    <section className="panel flex min-h-0 flex-col p-3">
      <h2 className="mb-2 text-sm font-semibold">文件 / 改动</h2>
      {!workspace?.exists && (
        <div className="muted text-xs" data-testid="files-empty">
          选择有效工作区后显示文件树与改动。
        </div>
      )}
      {workspace?.exists && (
        <div className="muted space-y-1 text-xs" data-testid="files-placeholder">
          <div>工作区：{workspace.path}</div>
          <div>Git：{workspace.git_root || "非 git 仓库"}</div>
          <div>符号链接、敏感文件与超大文件将在文件查看中强制边界。</div>
        </div>
      )}
    </section>
  );
}
