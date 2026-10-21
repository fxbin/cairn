/**
 * Cairn Desktop：对话优先 Agent 客户端布局。
 * 交互范式对齐 Codex / DeepSeek Harness / MiMo Desktop：
 * 会话流为主、工具与审批内嵌、底部 composer、可选检查栏。
 */
import { useCallback, useEffect, useMemo, useReducer, useState } from "react";
import { desktop, DesktopError } from "./desktop/client";
import { buildChatBlocks } from "./desktop/chatBlocks";
import type {
  ErrorBody,
  HealthResult,
  RecentProject,
  RunDetail,
  RunHistoryView,
  StreamEvent,
  WorkspaceStatus,
} from "./desktop/types";
import { ChatTranscript } from "./components/ChatTranscript";
import { Composer, ThreadSidebar } from "./components/ChatChrome";
import { InspectorRail, type RailTab } from "./components/InspectorRail";

interface AppState {
  health: HealthResult | null;
  workspace: WorkspaceStatus | null;
  recent: RecentProject[];
  runs: RunHistoryView[];
  selectedRunId: string | null;
  runDetail: RunDetail | null;
  streamEvents: StreamEvent[];
  loading: boolean;
  booting: boolean;
  error: ErrorBody | null;
  railOpen: boolean;
  railTab: RailTab;
}

type Action =
  | { type: "booting"; value: boolean }
  | { type: "loading"; value: boolean }
  | { type: "error"; error: ErrorBody | null }
  | { type: "health"; health: HealthResult }
  | { type: "workspace"; workspace: WorkspaceStatus }
  | { type: "recent"; recent: RecentProject[] }
  | { type: "runs"; runs: RunHistoryView[] }
  | { type: "runDetail"; detail: RunDetail | null }
  | { type: "selectRun"; runId: string | null }
  | { type: "autoSelectRun"; runId?: string }
  | { type: "stream"; event: StreamEvent }
  | { type: "clearStream" }
  | { type: "railOpen"; value: boolean }
  | { type: "railTab"; tab: RailTab };

const initialState: AppState = {
  health: null,
  workspace: null,
  recent: [],
  runs: [],
  selectedRunId: null,
  runDetail: null,
  streamEvents: [],
  loading: true,
  booting: true,
  error: null,
  railOpen: true,
  railTab: "details",
};

function reducer(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "booting":
      return { ...state, booting: action.value };
    case "loading":
      return { ...state, loading: action.value };
    case "error":
      return { ...state, error: action.error };
    case "health":
      return { ...state, health: action.health };
    case "workspace":
      return { ...state, workspace: action.workspace };
    case "recent":
      return { ...state, recent: action.recent };
    case "runs":
      return { ...state, runs: action.runs };
    case "runDetail":
      return { ...state, runDetail: action.detail };
    case "selectRun":
      return { ...state, selectedRunId: action.runId, streamEvents: [] };
    case "autoSelectRun":
      if (!action.runId || state.selectedRunId) {
        return state;
      }
      return { ...state, selectedRunId: action.runId };
    case "stream": {
      const next = state.streamEvents.concat(action.event);
      const bounded = next.length > 800 ? next.slice(next.length - 800) : next;
      return { ...state, streamEvents: bounded };
    }
    case "clearStream":
      return { ...state, streamEvents: [] };
    case "railOpen":
      return { ...state, railOpen: action.value };
    case "railTab":
      return { ...state, railTab: action.tab, railOpen: true };
    default:
      return state;
  }
}

function errorBodyOf(error: unknown): ErrorBody {
  if (error instanceof DesktopError) {
    return error.body;
  }
  return { code: "internal", message: String(error) };
}

export default function App() {
  const [state, dispatch] = useReducer(reducer, initialState);
  const [workspacePath, setWorkspacePath] = useState("");
  const [threadQuery, setThreadQuery] = useState("");
  const [draft, setDraft] = useState("");
  const [model, setModel] = useState("");
  const [running, setRunning] = useState(false);

  const onError = useCallback((error: ErrorBody) => {
    dispatch({ type: "error", error });
  }, []);

  const refreshThreads = useCallback(async (path: string) => {
    if (!path) {
      return;
    }
    try {
      const runs = await desktop.listRuns({ workspace: path, query: threadQuery });
      dispatch({ type: "runs", runs });
    } catch (error) {
      dispatch({ type: "error", error: errorBodyOf(error) });
    }
    // threadQuery 单独依赖会触发频繁刷新；搜索在输入后由用户回车/防抖可再优化
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [threadQuery]);

  const refreshAll = useCallback(
    async (path: string) => {
      if (!path) {
        dispatch({ type: "loading", value: false });
        return;
      }
      dispatch({ type: "loading", value: true });
      try {
        const [health, workspace, recent] = await Promise.all([
          desktop.health(),
          desktop.observeWorkspace(path),
          desktop.listRecentProjects(false),
        ]);
        dispatch({ type: "health", health });
        dispatch({ type: "workspace", workspace });
        dispatch({ type: "recent", recent });
        await desktop.rememberProject(path);
        await refreshThreads(path);
      } catch (error) {
        dispatch({ type: "error", error: errorBodyOf(error) });
      } finally {
        dispatch({ type: "loading", value: false });
        dispatch({ type: "booting", value: false });
      }
    },
    [refreshThreads],
  );

  // 启动：恢复最近项目
  useEffect(() => {
    void desktop
      .listRecentProjects(false)
      .then((recent) => {
        dispatch({ type: "recent", recent });
        if (recent.length > 0) {
          setWorkspacePath(recent[0].path);
        }
      })
      .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }))
      .finally(() => dispatch({ type: "booting", value: false }));
  }, []);

  useEffect(() => {
    if (workspacePath) {
      void refreshAll(workspacePath);
    }
  }, [workspacePath, refreshAll]);

  // 选中运行 → 从 Journal 重建
  useEffect(() => {
    if (!workspacePath || !state.selectedRunId) {
      dispatch({ type: "runDetail", detail: null });
      return;
    }
    let cancelled = false;
    void desktop
      .inspectRun({
        workspace: workspacePath,
        run_id: state.selectedRunId,
        include_events: true,
      })
      .then((detail) => {
        if (!cancelled) {
          dispatch({ type: "runDetail", detail });
        }
      })
      .catch((error) => {
        if (!cancelled) {
          dispatch({ type: "error", error: errorBodyOf(error) });
        }
      });
    return () => {
      cancelled = true;
    };
  }, [workspacePath, state.selectedRunId]);

  // 事件流
  useEffect(() => {
    return desktop.onStream((event) => {
      dispatch({ type: "stream", event });
      dispatch({ type: "autoSelectRun", runId: event.run_id });
      if (event.kind === "run.finished") {
        setRunning(false);
        if (workspacePath) {
          void refreshThreads(workspacePath);
        }
      }
      if (event.kind === "run.waiting_approval") {
        setRunning(false);
      }
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workspacePath]);

  const blocks = useMemo(
    () =>
      buildChatBlocks(
        state.runDetail?.events ?? [],
        state.streamEvents.filter(
          (item) => !state.selectedRunId || !item.run_id || item.run_id === state.selectedRunId,
        ),
      ),
    [state.runDetail?.events, state.streamEvents, state.selectedRunId],
  );

  const selectedRun = useMemo(
    () => state.runs.find((run) => run.run_id === state.selectedRunId) ?? null,
    [state.runs, state.selectedRunId],
  );

  const statusLabel = useMemo(() => {
    if (running) {
      return "执行中";
    }
    if (selectedRun?.waiting_approval) {
      return "待审批";
    }
    if (selectedRun) {
      return selectedRun.phase || selectedRun.state;
    }
    return "就绪";
  }, [running, selectedRun]);

  const submitTask = async () => {
    if (!workspacePath || !draft.trim()) {
      return;
    }
    const text = draft.trim();
    setDraft("");
    setRunning(true);
    dispatch({ type: "railTab", tab: "details" });
    try {
      const ack = await desktop.startRun({
        workspace: workspacePath,
        task: text,
        model: model.trim() || undefined,
      });
      if (ack.run_id) {
        dispatch({ type: "selectRun", runId: ack.run_id });
      }
    } catch (error) {
      setRunning(false);
      dispatch({ type: "error", error: errorBodyOf(error) });
    }
  };

  const decide = async (approve: boolean, fingerprint: string, runId: string) => {
    if (!workspacePath) {
      return;
    }
    setRunning(true);
    try {
      await desktop.resumeRun({
        workspace: workspacePath,
        run_id: runId,
        approve,
        fingerprint,
        reason: approve ? "approved via desktop UI" : "denied via desktop UI",
      });
    } catch (error) {
      setRunning(false);
      dispatch({ type: "error", error: errorBodyOf(error) });
    }
  };

  const pickWorkspace = async () => {
    try {
      const path = await desktop.pickDirectory("选择工作区", workspacePath);
      if (path) {
        setWorkspacePath(path);
      }
    } catch (error) {
      dispatch({ type: "error", error: errorBodyOf(error) });
    }
  };

  const filteredThreads = useMemo(() => {
    const q = threadQuery.trim().toLowerCase();
    if (!q) {
      return state.runs;
    }
    return state.runs.filter((run) =>
      `${run.title}\n${run.task}\n${run.run_id}`.toLowerCase().includes(q),
    );
  }, [state.runs, threadQuery]);

  return (
    <div className="app-shell" data-rail={state.railOpen ? "open" : "closed"} data-testid="cairn-shell">
      <ThreadSidebar
        threads={filteredThreads.map((run) => ({
          run_id: run.run_id,
          title: run.title,
          phase: run.phase,
          state: run.state,
          updated_at: run.updated_at,
        }))}
        activeId={state.selectedRunId}
        query={threadQuery}
        onQuery={setThreadQuery}
        onSelect={(runId) => {
          dispatch({ type: "selectRun", runId });
          setRunning(false);
        }}
        onNew={() => {
          dispatch({ type: "selectRun", runId: null });
          dispatch({ type: "clearStream" });
          setDraft("");
          setRunning(false);
        }}
        workspacePath={workspacePath}
        onPickWorkspace={() => void pickWorkspace()}
        workspaceLabel={state.workspace?.path ? workspacePath.split("/").pop() || workspacePath : workspacePath}
      />

      <main className="main-column">
        {state.error && (
          <div className="error-banner" role="alert" data-testid="error-banner">
            <span>
              {state.error.message}
              <span className="muted"> · {state.error.code}</span>
            </span>
            <button type="button" className="btn-sm" onClick={() => dispatch({ type: "error", error: null })}>
              关闭
            </button>
          </div>
        )}

        <header className="chat-header">
          <h1>{selectedRun?.title || "新任务"}</h1>
          <div className="header-actions">
            <span className="status-pill" data-state={running ? "running" : selectedRun ? selectedRun.state : "idle"}>
              <span className="dot" />
              {statusLabel}
            </span>
            <button
              type="button"
              className="icon-btn"
              onClick={() => dispatch({ type: "railOpen", value: !state.railOpen })}
              data-testid="toggle-rail"
            >
              检查器
            </button>
            <button type="button" className="icon-btn" onClick={() => workspacePath && refreshAll(workspacePath)}>
              刷新
            </button>
          </div>
        </header>

        <ChatTranscript
          blocks={blocks}
          loading={state.loading || state.booting}
          onDecide={(approve, fingerprint, runId) => void decide(approve, fingerprint, runId)}
          onSuggestion={(text) => setDraft(text)}
        />

        <Composer
          value={draft}
          onChange={setDraft}
          onSubmit={() => void submitTask()}
          onStop={() => {
            if (state.selectedRunId) {
              void desktop
                .cancelRun({ run_id: state.selectedRunId, reason: "user stop" })
                .then(() => setRunning(false))
                .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
            }
          }}
          running={running}
          model={model}
          onModelChange={setModel}
          disabled={!workspacePath}
        />
      </main>

      <InspectorRail
        tab={state.railTab}
        onTab={(tab) => dispatch({ type: "railTab", tab })}
        workspace={state.workspace}
        runId={state.selectedRunId}
        statusLabel={statusLabel}
        onError={onError}
      />
    </div>
  );
}
