/**
 * Cairn Desktop：对话优先 Agent 客户端。
 * 对照 DeepSeek Harness / Codex / MiMo Desktop 的交互闭环：
 * 可发送、可观察、可审批、可取消、可恢复、可设置，且切换会话不串流。
 */
import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import { desktop, DesktopError } from "./desktop/client";
import { buildChatBlocks, type ChatBlock } from "./desktop/chatBlocks";
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
import { SettingsDrawer, ThreadContextMenu } from "./components/SettingsDrawer";

interface AppState {
  health: HealthResult | null;
  workspace: WorkspaceStatus | null;
  recent: RecentProject[];
  runs: RunHistoryView[];
  selectedRunId: string | null;
  runDetail: RunDetail | null;
  streamEvents: StreamEvent[];
  optimistic: ChatBlock[];
  loading: boolean;
  booting: boolean;
  error: ErrorBody | null;
  railOpen: boolean;
  railTab: RailTab;
  settingsOpen: boolean;
  contextRun: { id: string; title: string } | null;
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
  | { type: "optimistic"; blocks: ChatBlock[] }
  | { type: "railOpen"; value: boolean }
  | { type: "railTab"; tab: RailTab }
  | { type: "settings"; open: boolean }
  | { type: "context"; run: { id: string; title: string } | null };

const initialState: AppState = {
  health: null,
  workspace: null,
  recent: [],
  runs: [],
  selectedRunId: null,
  runDetail: null,
  streamEvents: [],
  optimistic: [],
  loading: true,
  booting: true,
  error: null,
  railOpen: true,
  railTab: "details",
  settingsOpen: false,
  contextRun: null,
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
      return {
        ...state,
        selectedRunId: action.runId,
        streamEvents: [],
        optimistic: [],
      };
    case "autoSelectRun":
      if (!action.runId || state.selectedRunId) {
        return state;
      }
      return { ...state, selectedRunId: action.runId };
    case "stream": {
      // 按 run 归属过滤，旧会话回调不得污染当前视图。
      const runId = action.event.run_id;
      if (state.selectedRunId && runId && runId !== state.selectedRunId) {
        return state;
      }
      const next = state.streamEvents.concat(action.event);
      const bounded = next.length > 800 ? next.slice(next.length - 800) : next;
      return { ...state, streamEvents: bounded };
    }
    case "clearStream":
      return { ...state, streamEvents: [], optimistic: [] };
    case "optimistic":
      return { ...state, optimistic: action.blocks };
    case "railOpen":
      return { ...state, railOpen: action.value };
    case "railTab":
      return { ...state, railTab: action.tab, railOpen: true };
    case "settings":
      return { ...state, settingsOpen: action.open };
    case "context":
      return { ...state, contextRun: action.run };
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

function readTheme(): string {
  return localStorage.getItem("cairn.theme") || "dark";
}

function readReadOnly(): boolean {
  return localStorage.getItem("cairn.readonly") === "1";
}

export default function App() {
  const [state, dispatch] = useReducer(reducer, initialState);
  const [workspacePath, setWorkspacePath] = useState("");
  const [threadQuery, setThreadQuery] = useState("");
  const [draft, setDraft] = useState("");
  const [model, setModel] = useState("");
  const [running, setRunning] = useState(false);
  const [theme, setTheme] = useState(readTheme);
  const [readOnly, setReadOnly] = useState(readReadOnly);
  const runningRef = useRef(false);
  const selectedRunIdRef = useRef<string | null>(null);

  const onError = useCallback((error: ErrorBody) => {
    dispatch({ type: "error", error });
  }, []);

  // 主题
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem("cairn.theme", theme);
  }, [theme]);

  const refreshThreads = useCallback(async (path: string, query?: string) => {
    if (!path) {
      return;
    }
    try {
      const runs = await desktop.listRuns({ workspace: path, query: query ?? threadQuery });
      dispatch({ type: "runs", runs });
    } catch (error) {
      dispatch({ type: "error", error: errorBodyOf(error) });
    }
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

  // 会话切换时从 Journal 重建
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

  // 事件流：只保留当前 run
  useEffect(() => {
    return desktop.onStream((event) => {
      dispatch({ type: "stream", event });
      if (event.run_id && !selectedRunIdRef.current) {
        dispatch({ type: "autoSelectRun", runId: event.run_id });
      }
      if (event.kind === "run.finished" || event.kind === "run.waiting_approval") {
        runningRef.current = false;
        setRunning(false);
        if (workspacePath) {
          void refreshThreads(workspacePath);
        }
      }
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workspacePath]);

  useEffect(() => {
    selectedRunIdRef.current = state.selectedRunId;
  }, [state.selectedRunId]);

  const blocks = useMemo(() => {
    const fromJournal = buildChatBlocks(state.runDetail?.events ?? [], state.streamEvents);
    // 乐观用户消息：若 Journal 已有同文用户消息则丢弃，避免重复。
    const journalUser = new Set(
      fromJournal.filter((b) => b.kind === "user").map((b) => (b.kind === "user" ? b.text : "")),
    );
    const optimistic = state.optimistic.filter((b) => !(b.kind === "user" && journalUser.has(b.text)));
    return [...optimistic, ...fromJournal];
  }, [state.runDetail?.events, state.streamEvents, state.optimistic]);

  const selectedRun = useMemo(
    () => state.runs.find((run) => run.run_id === state.selectedRunId) ?? null,
    [state.runs, state.selectedRunId],
  );

  const statusLabel = useMemo(() => {
    if (running) {
      return "执行中";
    }
    if (selectedRun?.waiting_approval || blocks.some((b) => b.kind === "approval" && !b.resolved)) {
      return "待审批";
    }
    if (selectedRun) {
      return selectedRun.phase || selectedRun.state;
    }
    return "就绪";
  }, [running, selectedRun, blocks]);

  const submitTask = async () => {
    if (!workspacePath || !draft.trim()) {
      return;
    }
    const text = draft.trim();
    setDraft("");
    setRunning(true);
    runningRef.current = true;
    dispatch({ type: "railTab", tab: "details" });
    dispatch({
      type: "optimistic",
      blocks: [
        {
          kind: "user",
          id: `opt-${Date.now()}`,
          text,
          at: new Date().toISOString(),
        },
      ],
    });
    try {
      const ack = await desktop.startRun({
        workspace: workspacePath,
        task: text,
        model: model.trim() || undefined,
        mode: readOnly ? "read-only" : "default",
      });
      if (ack.run_id) {
        dispatch({ type: "selectRun", runId: ack.run_id });
        // selectRun 会清空 optimistic，这里补回用户消息
        dispatch({
          type: "optimistic",
          blocks: [
            {
              kind: "user",
              id: `opt-${Date.now()}`,
              text,
              at: new Date().toISOString(),
            },
          ],
        });
      }
    } catch (error) {
      setRunning(false);
      runningRef.current = false;
      dispatch({ type: "error", error: errorBodyOf(error) });
    }
  };

  const queueTask = async () => {
    if (!workspacePath || !draft.trim()) {
      return;
    }
    const text = draft.trim();
    setDraft("");
    try {
      await desktop.submitQueue({ workspace: workspacePath, content: text });
      dispatch({ type: "railTab", tab: "queue" });
    } catch (error) {
      dispatch({ type: "error", error: errorBodyOf(error) });
    }
  };

  const decide = async (approve: boolean, fingerprint: string, runId: string) => {
    if (!workspacePath) {
      return;
    }
    setRunning(true);
    runningRef.current = true;
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
      runningRef.current = false;
      dispatch({ type: "error", error: errorBodyOf(error) });
    }
  };

  const stopRun = () => {
    if (!state.selectedRunId) {
      return;
    }
    void desktop
      .cancelRun({ run_id: state.selectedRunId, reason: "user stop" })
      .then(() => {
        setRunning(false);
        runningRef.current = false;
      })
      .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
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

  // 快捷键
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      const meta = event.metaKey || event.ctrlKey;
      if (meta && event.key.toLowerCase() === "n") {
        event.preventDefault();
        dispatch({ type: "selectRun", runId: null });
        dispatch({ type: "clearStream" });
        setDraft("");
        setRunning(false);
      }
      if (meta && event.key === ",") {
        event.preventDefault();
        dispatch({ type: "settings", open: true });
      }
      if (event.key === "Escape") {
        dispatch({ type: "settings", open: false });
        dispatch({ type: "context", run: null });
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

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
          waiting_approval: run.waiting_approval,
        }))}
        activeId={state.selectedRunId}
        query={threadQuery}
        onQuery={(value) => {
          setThreadQuery(value);
          if (workspacePath) {
            void refreshThreads(workspacePath, value);
          }
        }}
        onSelect={(runId) => {
          dispatch({ type: "selectRun", runId });
          setRunning(false);
          runningRef.current = false;
        }}
        onNew={() => {
          dispatch({ type: "selectRun", runId: null });
          dispatch({ type: "clearStream" });
          setDraft("");
          setRunning(false);
          runningRef.current = false;
        }}
        onContext={(runId, title) => dispatch({ type: "context", run: { id: runId, title } })}
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
              aria-pressed={state.railOpen}
            >
              {state.railOpen ? "隐藏检查器" : "检查器"}
            </button>
            <button type="button" className="icon-btn" onClick={() => dispatch({ type: "settings", open: true })} data-testid="open-settings">
              设置
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
          onStop={stopRun}
          onQueue={() => void queueTask()}
          running={running}
          model={model}
          onModelChange={setModel}
          readOnly={readOnly}
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
        onRefreshThreads={() => workspacePath && refreshAll(workspacePath)}
      />

      <SettingsDrawer
        open={state.settingsOpen}
        onClose={() => dispatch({ type: "settings", open: false })}
        workspace={state.workspace}
        workspacePath={workspacePath}
        theme={theme}
        onTheme={setTheme}
        readOnly={readOnly}
        onReadOnly={(value) => {
          setReadOnly(value);
          localStorage.setItem("cairn.readonly", value ? "1" : "0");
        }}
        onError={onError}
      />

      <ThreadContextMenu
        open={Boolean(state.contextRun)}
        onClose={() => dispatch({ type: "context", run: null })}
        title={state.contextRun?.title || ""}
        onRename={(title) => {
          if (workspacePath && state.contextRun) {
            void desktop
              .renameSession(state.contextRun.id, title)
              .then(() => refreshAll(workspacePath))
              .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
          }
        }}
        onPin={() => {
          if (workspacePath && state.contextRun) {
            const run = state.runs.find((item) => item.run_id === state.contextRun!.id);
            void desktop
              .setSessionFlags(state.contextRun.id, true, run ? false : false)
              .then(() => refreshAll(workspacePath))
              .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
          }
        }}
        onArchive={() => {
          if (workspacePath && state.contextRun) {
            void desktop
              .setSessionFlags(state.contextRun.id, false, true)
              .then(() => refreshAll(workspacePath))
              .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
          }
        }}
      />
    </div>
  );
}
