import { useState } from "react";
import { desktop } from "../desktop/client";
import type { ErrorBody, RunHistoryView } from "../desktop/types";

/** 任务工作区：多行输入、提交与取消。 */
export function TaskWorkspace({
  workspacePath,
  selectedRun,
  onStart,
  onError,
}: {
  workspacePath: string;
  selectedRun: RunHistoryView | null;
  onStart: () => void;
  onError: (error: ErrorBody) => void;
}) {
  const [task, setTask] = useState("");
  const [model, setModel] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    if (!workspacePath || !task.trim()) {
      return;
    }
    setBusy(true);
    try {
      await desktop.startRun({
        workspace: workspacePath,
        task,
        model: model.trim() || undefined,
        session_id: selectedRun?.session_id || undefined,
      });
      setTask("");
      onStart();
    } catch (error) {
      const body =
        error && typeof error === "object" && "body" in error
          ? (error as { body: ErrorBody }).body
          : { code: "internal", message: String(error) };
      onError(body);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="panel flex flex-col gap-2 p-3">
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">任务</h2>
        <div className="flex items-center gap-2">
          <label className="muted text-xs" htmlFor="model-input">
            模型
          </label>
          <input
            id="model-input"
            className="input w-48"
            value={model}
            onChange={(event) => setModel(event.target.value)}
            placeholder="使用配置默认"
            spellCheck={false}
          />
        </div>
      </div>
      <textarea
        className="input min-h-[96px] resize-y"
        value={task}
        onChange={(event) => setTask(event.target.value)}
        placeholder="描述任务。支持多行与粘贴；中文输入法选词时不会误发送。"
        aria-label="任务输入"
        data-testid="task-input"
      />
      <div className="flex items-center justify-between">
        <div className="muted text-xs">
          {selectedRun ? `当前会话 ${selectedRun.session_id || selectedRun.run_id}` : "新建任务"}
        </div>
        <button
          type="button"
          className="btn btn-primary"
          onClick={() => void submit()}
          disabled={busy || !workspacePath || !task.trim()}
          data-testid="task-submit"
        >
          {busy ? "提交中…" : "运行任务"}
        </button>
      </div>
    </section>
  );
}
