/**
 * Cairn GUI 根布局：六区壳层 + 全局错误/加载/空态。
 * 运行事实来自 desktop 桥接的 Journal 投影，本地状态只保存视图偏好。
 */
import { useCallback, useEffect, useMemo, useReducer, useState } from "react";
import { desktop, DesktopError } from "./desktop/client";
import type {
  ErrorBody,
  HealthResult,
  RecentProject,
  RunHistoryView,
  StreamEvent,
  WorkspaceStatus,
} from "./desktop/types";
import { Sidebar } from "./components/Sidebar";
import { TaskWorkspace } from "./components/TaskWorkspace";
import { ApprovalPanel } from "./components/ApprovalPanel";
import { FilesPanel } from "./components/FilesPanel";
import { RunDetailsPanel } from "./components/RunDetailsPanel";
import { StatusBar } from "./components/StatusBar";
import { ErrorBanner } from "./components/ErrorBanner";

type PanelId = "files" | "run" | "settings";

interface AppState {
  health: HealthResult | null;
  workspace: WorkspaceStatus | null;
  recent: RecentProject[];
  runs: RunHistoryView[];
  selectedRunId: string | null;
  streamEvents: StreamEvent[];
  loading: boolean;
  error: ErrorBody | null;
  panel: PanelId;
}

type Action =
  | { type: "loading"; loading: boolean }
  | { type: "error"; error: ErrorBody | null }
  | { type: "health"; health: HealthResult }
  | { type: "workspace"; workspace: WorkspaceStatus }
  | { type: "recent"; recent: RecentProject[] }
  | { type: "runs"; runs: RunHistoryView[] }
  | { type: "selectRun"; runId: string | null }
  | { type: "autoSelectRun"; runId?: string }
  | { type: "stream"; event: StreamEvent }
  | { type: "panel"; panel: PanelId }
  | { type: "resetStream" };

const initialState: AppState = {
  health: null,
  workspace: null,
  recent: [],
  runs: [],
  selectedRunId: null,
  streamEvents: [],
  loading: true,
  error: null,
  panel: "run",
};

function reducer(state: AppState, action: Action): AppState {
  switch (action.type) {
    case "loading":
      return { ...state, loading: action.loading };
    case "error":
      return { ...state, error: action.error, loading: false };
    case "health":
      return { ...state, health: action.health };
    case "workspace":
      return { ...state, workspace: action.workspace };
    case "recent":
      return { ...state, recent: action.recent };
    case "runs":
      return { ...state, runs: action.runs };
    case "selectRun":
      return { ...state, selectedRunId: action.runId };
    case "autoSelectRun":
      if (!action.runId || state.selectedRunId) {
        return state;
      }
      return { ...state, selectedRunId: action.runId };
    case "stream": {
      const next = state.streamEvents.concat(action.event);
      // 有界缓冲，避免长日志拖垮渲染；完整事件以 Inspect 快照为准。
      const bounded = next.length > 500 ? next.slice(next.length - 500) : next;
      return { ...state, streamEvents: bounded };
    }
    case "resetStream":
      return { ...state, streamEvents: [] };
    case "panel":
      return { ...state, panel: action.panel };
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

  const refreshAll = useCallback(async (path: string) => {
    if (!path) {
      dispatch({ type: "loading", loading: false });
      return;
    }
    dispatch({ type: "loading", loading: true });
    dispatch({ type: "error", error: null });
    try {
      const [health, workspace, recent, runs] = await Promise.all([
        desktop.health(),
        desktop.observeWorkspace(path),
        desktop.listRecentProjects(false),
        desktop.listRuns({ workspace: path }),
      ]);
      dispatch({ type: "health", health });
      dispatch({ type: "workspace", workspace });
      dispatch({ type: "recent", recent });
      dispatch({ type: "runs", runs });
      await desktop.rememberProject(path);
    } catch (error) {
      dispatch({ type: "error", error: errorBodyOf(error) });
    } finally {
      dispatch({ type: "loading", loading: false });
    }
  }, []);

  useEffect(() => {
    void desktop
      .listRecentProjects(false)
      .then((recent) => {
        dispatch({ type: "recent", recent });
        if (recent.length > 0 && !workspacePath) {
          setWorkspacePath(recent[0].path);
        }
      })
      .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }))
      .finally(() => dispatch({ type: "loading", loading: false }));
  }, [workspacePath]);

  useEffect(() => {
    if (workspacePath) {
      void refreshAll(workspacePath);
    }
  }, [workspacePath, refreshAll]);

  useEffect(() => {
    return desktop.onStream((event) => {
      dispatch({ type: "stream", event });
      dispatch({ type: "autoSelectRun", runId: event.run_id });
      if (
        event.kind === "run.finished" ||
        event.kind === "run.waiting_approval" ||
        event.kind === "run.waiting_reconcile"
      ) {
        if (workspacePath) {
          void desktop
            .listRuns({ workspace: workspacePath })
            .then((runs) => dispatch({ type: "runs", runs }))
            .catch(() => undefined);
        }
      }
    });
  }, [workspacePath]);

  const selectedRun = useMemo(
    () => state.runs.find((run) => run.run_id === state.selectedRunId) ?? null,
    [state.runs, state.selectedRunId],
  );

  return (
    <div className="flex h-full flex-col" data-testid="cairn-shell">
      <ErrorBanner error={state.error} onDismiss={() => dispatch({ type: "error", error: null })} />
      <div className="flex min-h-0 flex-1">
        <Sidebar
          workspace={state.workspace}
          recent={state.recent}
          runs={state.runs}
          selectedRunId={state.selectedRunId}
          loading={state.loading}
          workspacePath={workspacePath}
          onWorkspaceChange={setWorkspacePath}
          onSelectRun={(runId) => {
            dispatch({ type: "selectRun", runId });
            dispatch({ type: "resetStream" });
          }}
          onRefresh={() => void refreshAll(workspacePath)}
          onError={(error) => dispatch({ type: "error", error })}
        />
        <main className="flex min-w-0 flex-1 flex-col gap-3 p-3">
          <TaskWorkspace
            workspacePath={workspacePath}
            selectedRun={selectedRun}
            onStart={() => void refreshAll(workspacePath)}
            onError={(error) => dispatch({ type: "error", error })}
          />
          <div className="grid min-h-0 flex-1 grid-cols-1 gap-3 lg:grid-cols-3">
            <ApprovalPanel
              workspacePath={workspacePath}
              selectedRun={selectedRun}
              streamEvents={state.streamEvents}
              onError={(error) => dispatch({ type: "error", error })}
            />
            <FilesPanel workspace={state.workspace} onError={(error) => dispatch({ type: "error", error })} />
            <div className="flex min-h-0 flex-col gap-3">
              <RunDetailsPanel
                workspacePath={workspacePath}
                selectedRun={selectedRun}
                streamEvents={state.streamEvents}
                panel={state.panel}
                onPanel={(panel) => dispatch({ type: "panel", panel })}
                onError={(error) => dispatch({ type: "error", error })}
              />
            </div>
          </div>
        </main>
      </div>
      <StatusBar health={state.health} workspace={state.workspace} loading={state.loading} />
    </div>
  );
}
