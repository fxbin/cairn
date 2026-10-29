/**
 * Cairn Desktop：对话优先 Agent 客户端。
 * 对照 DeepSeek Harness / Codex / MiMo Desktop 的交互闭环：
 * 可发送、可观察、可审批、可取消、可恢复、可设置，且切换会话不串流。
 */
import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import { desktop, DesktopError } from "./desktop/client";
import { bridgeMode, demoBridgeActive } from "./desktop/wails-api";
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
  contextRun: { id: string; title: string; sessionId: string; pinned: boolean } | null;
}

type Action =
  | { type: "booting"; value: boolean }
  | { type: "loading"; value: boolean }
  | { type: "error"; error: ErrorBody | null }
  | { type: "health"; health: HealthResult }
  | { type: "workspace"; workspace: WorkspaceStatus }
  | { type: "switchWorkspace" }
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
  | { type: "context"; run: { id: string; title: string; sessionId: string; pinned: boolean } | null };

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
    case "switchWorkspace":
      return { ...state, workspace: null, runs: [], selectedRunId: null, runDetail: null, streamEvents: [], optimistic: [], contextRun: null, error: null, loading: true };
    case "recent":
      return { ...state, recent: action.recent };
    case "runs":
      return { ...state, runs: action.runs };
    case "runDetail":
      return { ...state, runDetail: action.detail };
    case "selectRun":
      if (state.selectedRunId === action.runId) return state;
      return {
        ...state,
        selectedRunId: action.runId,
        runDetail: null,
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
  const [forkFromRunId, setForkFromRunId] = useState<string | null>(null);
  const [queueVersion, setQueueVersion] = useState(0);
  const [draft, setDraft] = useState("");
  const [model, setModel] = useState("");
  const [running, setRunning] = useState(false);
  const [theme, setTheme] = useState(readTheme);
  const [readOnly, setReadOnly] = useState(readReadOnly);
  const [railWidth, setRailWidth] = useState(() => Number(localStorage.getItem("cairn.railWidth") || "320"));
  const [fontSize, setFontSize] = useState(() => Number(localStorage.getItem("cairn.fontSize") || "14"));
  const [skillItems, setSkillItems] = useState<Array<{ name: string; description?: string; scope?: string; disabled?: boolean; manual_only?: boolean }>>([]);
  const [skillQuery, setSkillQuery] = useState("");
  const [sessionTree, setSessionTree] = useState<{ nodes: Record<string, { run_id: string; session_id?: string; task?: string; phase?: string; children?: string[] }>; roots: string[] } | null>(null);
  const [commands, setCommands] = useState<Array<{ name: string; description: string; scope: string }>>([]);
  const [errorLog, setErrorLog] = useState<Array<{ code: string; message: string; at: string }>>([]);
  const [memories, setMemories] = useState<Array<{ id: string; kind: string; outcome: string; status: string; trusted_verification: boolean; created_at?: string }>>([]);
  const [workflows, setWorkflows] = useState<Array<{ id: string; name: string; steps: Array<{ task: string }> }>>([]);
  const [hooks, setHooks] = useState<Array<{ id: string; event: string; enabled: boolean; actions: Array<{ kind: string; content?: string }> }>>([]);
  const [navView, setNavView] = useState<"workbench" | "resources" | "automation" | "stats">(() => {
    const saved = localStorage.getItem("cairn.navView");
    return saved === "resources" || saved === "automation" || saved === "stats" ? saved : "workbench";
  });
  const runningRef = useRef(false);
  const selectedRunIdRef = useRef<string | null>(null);
  const workspaceIdentityRef = useRef({ path: "", generation: 0 });
  const threadRequestRef = useRef(0);
  const refreshRequestRef = useRef(0);
  const threadQueryRef = useRef("");
  const knownRunWorkspaceRef = useRef(new Map<string, string>());
  const activeRunsRef = useRef(new Set<string>());
  const settledRunsRef = useRef(new Set<string>());
  const queueBusyRef = useRef(new Set<number>());

  // 切换时立即推进世代；路径相同的 A→B→A 也不能接受第一次 A 的迟到响应。
  const changeWorkspace = useCallback((path: string) => {
    if (workspaceIdentityRef.current.path === path) return;
    workspaceIdentityRef.current = { path, generation: workspaceIdentityRef.current.generation + 1 };
    selectedRunIdRef.current = null;
    runningRef.current = false;
    setRunning(false);
    setDraft("");
    setModel("");
    setForkFromRunId(null);
    setSkillItems([]);
    setSessionTree(null);
    setMemories([]);
    setWorkflows([]);
    setHooks([]);
    dispatch({ type: "switchWorkspace" });
    setRunReturnStack([]);
    setWorkspacePath(path);
  }, []);

  const [runReturnStack, setRunReturnStack] = useState<Array<{ child: string; parent: string }>>([]);

  const selectCurrentRun = useCallback((runId: string | null) => {
    selectedRunIdRef.current = runId;
    dispatch({ type: "selectRun", runId });
    const active = Boolean(runId && activeRunsRef.current.has(runId));
    runningRef.current = active;
    setRunning(active);
  }, []);

  const selectSubagentRun = useCallback((childRunId: string) => {
    const parent = selectedRunIdRef.current;
    if (parent && parent !== childRunId) {
      setRunReturnStack((stack) => [...stack.filter((item) => item.child !== childRunId), { child: childRunId, parent }]);
    }
    selectCurrentRun(childRunId);
  }, [selectCurrentRun]);

  const backToParentRun = useCallback(() => {
    setRunReturnStack((stack) => {
      if (stack.length === 0) return stack;
      const top = stack[stack.length - 1];
      selectCurrentRun(top.parent);
      return stack.slice(0, -1);
    });
  }, [selectCurrentRun]);

  const acceptWorkspaceResult = <T,>(apply: (value: T) => void) => {
    const identity = workspaceIdentityRef.current;
    return (value: T) => { if (workspaceIdentityRef.current === identity) apply(value); };
  };


  const onError = useCallback((error: ErrorBody) => {
    dispatch({ type: "error", error });
    setErrorLog((prev) =>
      [{ code: error.code, message: error.message, at: new Date().toISOString() }, ...prev].slice(0, 20),
    );
  }, []);

  // 主题
  useEffect(() => {
    const root = document.documentElement;
    const apply = () => {
      if (theme === "system" && typeof window.matchMedia === "function") {
        const dark = window.matchMedia("(prefers-color-scheme: dark)").matches;
        root.dataset.theme = dark ? "dark" : "light";
      } else {
        root.dataset.theme = theme === "system" ? "dark" : theme;
      }
    };
    apply();
    localStorage.setItem("cairn.theme", theme);
    if (typeof window.matchMedia !== "function") {
      return;
    }
    const media = window.matchMedia("(prefers-color-scheme: dark)");
    media.addEventListener("change", apply);
    return () => media.removeEventListener("change", apply);
  }, [theme]);

  const refreshThreads = useCallback(async (path: string, query?: string) => {
    const identity = workspaceIdentityRef.current;
    if (!path || identity.path !== path) return;
    const request = ++threadRequestRef.current;
    try {
      const runs = await desktop.listRuns({ workspace: path, query: query ?? threadQueryRef.current });
      if (workspaceIdentityRef.current !== identity || threadRequestRef.current !== request) return;
      for (const run of runs) knownRunWorkspaceRef.current.set(run.run_id, path);
      dispatch({ type: "runs", runs });
    } catch (error) {
      if (workspaceIdentityRef.current === identity && threadRequestRef.current === request) {
        dispatch({ type: "error", error: errorBodyOf(error) });
      }
    }
  }, []);

  const refreshAll = useCallback(async (path: string) => {
    const identity = workspaceIdentityRef.current;
    if (!path || identity.path !== path) return;
    const request = ++refreshRequestRef.current;
    const current = () => workspaceIdentityRef.current === identity && refreshRequestRef.current === request;
    dispatch({ type: "loading", value: true });
    try {
      const [health, workspace, recent] = await Promise.all([
        desktop.health(), desktop.observeWorkspace(path), desktop.listRecentProjects(false),
      ]);
      if (!current()) return;
      dispatch({ type: "health", health });
      dispatch({ type: "workspace", workspace });
      dispatch({ type: "recent", recent });
      await desktop.rememberProject(path);
      if (current()) await refreshThreads(path);
    } catch (error) {
      if (current()) dispatch({ type: "error", error: errorBodyOf(error) });
    } finally {
      if (current()) {
        dispatch({ type: "loading", value: false });
        dispatch({ type: "booting", value: false });
      }
    }
  }, [refreshThreads]);

  useEffect(() => {
    void desktop
      .listRecentProjects(false)
      .then((recent) => {
        dispatch({ type: "recent", recent });
        if (recent.length > 0 && !workspaceIdentityRef.current.path) {
          changeWorkspace(recent[0].path);
        }
      })
      .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }))
      .finally(() => dispatch({ type: "booting", value: false }));
  }, []);

  useEffect(() => {
    if (workspacePath) void refreshAll(workspacePath);
  }, [workspacePath, refreshAll]);

  // 受理 ACK 只分配身份；历史列表或已持久化事件确认存在后，才能查询投影。
  const selectedRunPersisted = state.runs.some((run) => run.run_id === state.selectedRunId) ||
    state.streamEvents.some((event) => event.run_id === state.selectedRunId && event.kind === "run.event" && Boolean(event.event));

  // 会话切换及运行启停时从 Journal 重建，审批停点与最终完成证据不能沿用旧投影。
  useEffect(() => {
    if (!workspacePath || !state.selectedRunId) {
      dispatch({ type: "runDetail", detail: null });
      return;
    }
    if (!selectedRunPersisted) return;
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
  }, [workspacePath, state.selectedRunId, running, selectedRunPersisted]);

  // 流必须同时匹配工作区身份与当前 run，后台项目完成不能改变当前任务状态。
  useEffect(() => {
    if (!workspacePath) return;
    const identity = workspaceIdentityRef.current;
    return desktop.onStream((event) => {
      if (workspaceIdentityRef.current !== identity) return;
      const source = event.workspace || (event.run_id ? knownRunWorkspaceRef.current.get(event.run_id) : undefined);
      if (source !== workspacePath) return;
      if (event.run_id) knownRunWorkspaceRef.current.set(event.run_id, workspacePath);
      const stopped = event.kind === "run.finished" || event.kind === "run.waiting_approval" || event.kind === "run.waiting_reconcile";
      if (event.run_id && stopped) {
        activeRunsRef.current.delete(event.run_id);
        settledRunsRef.current.add(event.run_id);
      }
      if (stopped) void refreshThreads(workspacePath);
      if (selectedRunIdRef.current && event.run_id && selectedRunIdRef.current !== event.run_id) return;
      if (event.run_id && !selectedRunIdRef.current) {
        selectedRunIdRef.current = event.run_id;
        dispatch({ type: "autoSelectRun", runId: event.run_id });
      }
      dispatch({ type: "stream", event });
      if (stopped) {
        runningRef.current = false;
        setRunning(false);
      } else if (event.run_id && event.kind === "run.event") {
        activeRunsRef.current.add(event.run_id);
        settledRunsRef.current.delete(event.run_id);
        runningRef.current = true;
        setRunning(true);
      }
    });
  }, [workspacePath, refreshThreads]);

  useEffect(() => {
    selectedRunIdRef.current = state.selectedRunId;
  }, [state.selectedRunId]);

  useEffect(() => {
    document.documentElement.style.setProperty("--font-size-base", `${fontSize}px`);
    localStorage.setItem("cairn.fontSize", String(fontSize));
  }, [fontSize]);

  useEffect(() => {
    document.documentElement.style.setProperty("--rail-width", `${railWidth}px`);
    localStorage.setItem("cairn.railWidth", String(railWidth));
  }, [railWidth]);

  useEffect(() => {
    localStorage.setItem("cairn.navView", navView);
  }, [navView]);

  useEffect(() => {
    if (!workspacePath) return;
    let cancelled = false;
    const update = <T,>(setter: (value: T) => void) => (value: T) => { if (!cancelled) setter(value); };
    void Promise.resolve(desktop.listHooks(workspacePath)).then(update((items) => setHooks(items || []))).catch(() => update(setHooks)([]));
    void Promise.resolve(desktop.listWorkflows(workspacePath)).then(update((items) => setWorkflows(items || []))).catch(() => update(setWorkflows)([]));
    void Promise.resolve(desktop.listMemories({ workspace: workspacePath })).then(update((items) => setMemories(items || []))).catch(() => update(setMemories)([]));
    void Promise.resolve(desktop.listCommands()).then(update((items) => setCommands(items || []))).catch(() => update(setCommands)([]));
    void Promise.resolve(desktop.sessionTree({ workspace: workspacePath })).then(update((tree) => setSessionTree(tree || null))).catch(() => update(setSessionTree)(null));
    void Promise.resolve(desktop.listSkills({ workspace: workspacePath })).then(update((data: unknown) => setSkillItems(Array.isArray(data) ? data : []))).catch(() => update(setSkillItems)([]));
    return () => { cancelled = true; };
  }, [workspacePath]);

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

  const pendingItems = useMemo(() => {
    const items: Array<{ kind: string; label: string; runId: string }> = [];
    for (const run of state.runs) {
      if (run.waiting_approval) {
        items.push({ kind: "approval", label: run.title || run.run_id, runId: run.run_id });
      }
      if (run.waiting_reconcile) {
        items.push({ kind: "reconcile", label: run.title || run.run_id, runId: run.run_id });
      }
      if (run.state === "failed" || run.phase === "failed") {
        items.push({ kind: "failed", label: run.title || run.run_id, runId: run.run_id });
      }
    }
    return items;
  }, [state.runs]);

  // 运行状态一律渲染桥接下发的展示词；前端不自行把机器状态映射成中文。
  const statusLabel = useMemo(() => {
    const approvalPending =
      Boolean(selectedRun?.waiting_approval) || blocks.some((b) => b.kind === "approval" && !b.resolved);
    const approvalEntry = state.runs.find((item) => item.waiting_approval);
    const runningEntry = state.runs.find((item) => item.phase === "running");
    const detail = state.runDetail;
    const label =
      selectedRun?.phase_label ||
      (approvalPending ? approvalEntry?.phase_label : "") ||
      runningEntry?.phase_label ||
      (detail?.phase === "running" ? detail.phase_label || detail.phase : "") ||
      selectedRun?.phase ||
      selectedRun?.state ||
      "";
    if (running || approvalPending || selectedRun) {
      return label;
    }
    // 未选中运行时的空态提示不是运行事实，因此不来自运行词表。
    return "就绪";
  }, [running, selectedRun, blocks, state.runs, state.runDetail]);

  const submitTask = async () => {
    if (!workspacePath || !draft.trim() || runningRef.current) return;
    const identity = workspaceIdentityRef.current;
    const text = draft.trim();
    selectCurrentRun(null);
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
      // 历史续接：显式绑定 session 链尾；新任务不带链关系。
      const continueFrom =
        !forkFromRunId && selectedRun && selectedRun.session_id && selectedRun.state !== "running" && !selectedRun.waiting_approval
          ? selectedRun
          : null;
      // 续接与分支显式区分：forkFromRunId 存在时是新会话分支。
      const ack = await desktop.startRun({
        workspace: workspacePath,
        task: text,
        model: model.trim() || undefined,
        mode: readOnly ? "read-only" : "default",
        session_id: continueFrom?.session_id,
        prev_run_id: continueFrom?.run_id,
        require_session_tail: Boolean(continueFrom),
        fork_source_run_id: forkFromRunId || undefined,
      });
      knownRunWorkspaceRef.current.set(ack.run_id, identity.path);
      if (workspaceIdentityRef.current !== identity) return;
      setForkFromRunId(null);
      if (ack.run_id) {
        if (!settledRunsRef.current.has(ack.run_id)) activeRunsRef.current.add(ack.run_id);
        selectCurrentRun(ack.run_id);
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
      if (workspaceIdentityRef.current !== identity) return;
      // 失败时保全已发送文本与等待期间的新草稿。
      setDraft((current) => current ? text + "\n" + current : text);
      setRunning(false);
      runningRef.current = false;
      dispatch({ type: "error", error: errorBodyOf(error) });
    }
  };

  const queueTask = async () => {
    const identity = workspaceIdentityRef.current;
    if (!workspacePath || !draft.trim() || queueBusyRef.current.has(identity.generation)) return;
    queueBusyRef.current.add(identity.generation);
    const text = draft.trim();
    setDraft("");
    try {
      const item = await desktop.submitQueue({ workspace: workspacePath, content: text });
      if (workspaceIdentityRef.current !== identity) return;
      setQueueVersion((version) => version + 1);
      dispatch({ type: "railTab", tab: "queue" });
      dispatch({ type: "error", error: { code: "queued", message: `已入队 ${item.input_id}（待确认；确认后启动为新 run）` } });
    } catch (error) {
      if (workspaceIdentityRef.current !== identity) return;
      setDraft((current) => current ? text + "\n" + current : text);
      dispatch({ type: "error", error: errorBodyOf(error) });
    } finally {
      queueBusyRef.current.delete(identity.generation);
    }
  };

  const decide = async (approve: boolean, fingerprint: string, runId: string): Promise<boolean> => {
    if (!workspacePath) return false;
    const identity = workspaceIdentityRef.current;
    settledRunsRef.current.delete(runId);
    activeRunsRef.current.add(runId);
    setRunning(true);
    runningRef.current = true;
    try {
      await desktop.resumeRun({
        workspace: workspacePath, run_id: runId, approve, fingerprint,
        reason: approve ? "approved via desktop UI" : "denied via desktop UI",
      });
      return true;
    } catch (error) {
      activeRunsRef.current.delete(runId);
      if (workspaceIdentityRef.current === identity && selectedRunIdRef.current === runId) {
        setRunning(false);
        runningRef.current = false;
        dispatch({ type: "error", error: errorBodyOf(error) });
      }
      return false;
    }
  };

  const stopRun = () => {
    if (!state.selectedRunId) {
      return;
    }
    void desktop
      .cancelRun({ run_id: state.selectedRunId, reason: "user stop" })
      .then(() => undefined)
      .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
  };

  const pickWorkspace = async () => {
    try {
      const path = await desktop.pickDirectory("选择工作区", workspacePath);
      if (path) {
        changeWorkspace(path);
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
        setForkFromRunId(null);
        selectCurrentRun(null);
        setDraft("");
      }
      if (meta && event.key === ",") {
        event.preventDefault();
        dispatch({ type: "settings", open: true });
      }
      if (event.key === "Escape") {
        dispatch({ type: "settings", open: false });
        dispatch({ type: "context", run: null });
      }
      if (event.key === "?" && !meta && !(event.target instanceof HTMLInputElement) && !(event.target instanceof HTMLTextAreaElement)) {
        event.preventDefault();
        dispatch({ type: "settings", open: true });
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [selectCurrentRun]);

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
    <div
      className="app-shell"
      data-rail={state.railOpen ? "open" : "closed"}
      data-testid="cairn-shell"
      style={{ ["--rail-width" as string]: `${railWidth}px` }}
    >
      <nav className="global-nav" aria-label="主导航">
        {(
          [
            ["workbench", "工作台"],
            ["resources", "资源"],
            ["automation", "自动化"],
            ["stats", "统计"],
          ] as const
        ).map(([id, label]) => (
          <button
            key={id}
            type="button"
            className={`nav-item ${navView === id ? "active" : ""}`}
            data-testid={`nav-${id}`}
            aria-current={navView === id ? "page" : undefined}
            onClick={() => setNavView(id)}
          >
            {label}
          </button>
        ))}
        <button
          type="button"
          className="nav-item"
          data-testid="nav-settings"
          onClick={() => dispatch({ type: "settings", open: true })}
        >
          设置
        </button>
      </nav>
      <ThreadSidebar
        threads={filteredThreads.map((run) => ({
          run_id: run.run_id,
          session_id: run.session_id,
          pinned: Boolean(run.pinned),
          title: run.title,
          phase: run.phase,
          phase_label: run.phase_label,
          state: run.state,
          updated_at: run.updated_at,
          waiting_approval: run.waiting_approval,
        }))}
        activeId={state.selectedRunId}
        query={threadQuery}
        onQuery={(value) => {
          setThreadQuery(value);
          threadQueryRef.current = value;
          if (workspacePath) {
            void refreshThreads(workspacePath, value);
          }
        }}
        onSelect={(runId) => {
          setForkFromRunId(null);
          selectCurrentRun(runId);
        }}
        onNew={() => {
          setForkFromRunId(null);
          selectCurrentRun(null);
          setDraft("");
        }}
        onContext={(runId, title, sessionId, pinned) =>
          dispatch({ type: "context", run: { id: runId, title, sessionId, pinned } })
        }
        workspacePath={workspacePath}
        onPickWorkspace={() => void pickWorkspace()}
        workspaceLabel={state.workspace?.path ? workspacePath.split("/").pop() || workspacePath : workspacePath}
        recent={state.recent}
        onSwitchWorkspace={changeWorkspace}
      />

      {navView !== "workbench" && (
        <main className="main-column" data-testid={`view-${navView}`}>
          <header className="chat-header">
            <h1>
              {navView === "resources" ? "资源中心" : navView === "automation" ? "自动化中心" : "使用统计"}
            </h1>
            <div className="header-actions">
              <button type="button" className="icon-btn" onClick={() => setNavView("workbench")}>
                返回工作台
              </button>
              <button type="button" className="icon-btn" onClick={() => dispatch({ type: "settings", open: true })}>
                设置
              </button>
            </div>
          </header>
          {navView === "resources" && (
            <div className="rail-section" data-testid="skill-catalog">
              <h3>技能目录</h3>
              <p className="muted">
                本地 SKILL.md 内容包；选中不等于已激活，声明工具/钩子不授予权限。
                编辑须在合法原生路径；旧运行仍使用历史正文快照。
              </p>
              <input
                className="input-sm"
                placeholder="搜索技能…"
                value={skillQuery}
                data-testid="skill-search"
                onChange={(event) => setSkillQuery(event.target.value)}
              />
              <div style={{ marginTop: 10, display: "flex", flexDirection: "column", gap: 8 }}>
                {skillItems
                  .filter((item) => {
                    const q = skillQuery.trim().toLowerCase();
                    if (!q) return true;
                    return (
                      item.name.toLowerCase().includes(q) ||
                      String(item.description || "").toLowerCase().includes(q)
                    );
                  })
                  .map((item) => (
                    <div className="list-card" key={item.name} data-testid={`skill-${item.name}`}>
                      <div className="title">{item.name}</div>
                      <div className="meta">{item.description || ""}</div>
                      <div className="meta">
                        来源 {item.scope || "—"} · {item.disabled ? "已禁用" : "启用"} ·{" "}
                        {item.manual_only ? "仅手动" : "可自动"}
                      </div>
                      <div className="btn-row">
                        <button
                          type="button"
                          className="btn-sm"
                          data-testid={`preview-skill-${item.name}`}
                          onClick={() => {
                            dispatch({
                              type: "error",
                              error: {
                                code: "skill_source",
                                message: `${item.name} 来源 ${item.scope || "—"}；正文在技能目录 SKILL.md，经工具边界读取。选中不等于已激活。`,
                              },
                            });
                          }}
                        >
                          来源说明
                        </button>
                        {!item.disabled && (
                          <button
                            type="button"
                            className="btn-sm"
                            data-testid={`use-skill-${item.name}`}
                            onClick={() => {
                              setDraft((d) => (d ? d + "\n" : "") + `使用技能 ${item.name}：`);
                              setNavView("workbench");
                            }}
                          >
                            用于任务
                          </button>
                        )}
                        <button
                          type="button"
                          className="btn-sm"
                          data-testid={`toggle-skill-${item.name}`}
                          onClick={() => {
                            void desktop
                              .setSkillEnabled(workspacePath, item.scope || "user", item.name, Boolean(item.disabled))
                              .then(() => desktop.listSkills({ workspace: workspacePath }))
                              .then((data) => setSkillItems(Array.isArray(data) ? data : []))
                              .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
                          }}
                        >
                          {item.disabled ? "启用" : "停用"}
                        </button>
                      </div>
                    </div>
                  ))}
                {skillItems.length === 0 && <div className="empty-sm">未发现技能或未启用</div>}
              </div>
              <h3 style={{ marginTop: 16 }}>内置命令</h3>
              <div data-testid="command-catalog">
                {commands.map((cmd) => (
                  <div className="list-card" key={cmd.name}>
                    <div className="title">{cmd.name}</div>
                    <div className="meta">{cmd.description}</div>
                    <div className="muted" style={{ fontSize: "var(--fs-body)" }}>作用域 {cmd.scope}</div>
                  </div>
                ))}
              </div>
              <button
                type="button"
                className="btn-sm primary"
                data-testid="open-resource-settings"
                onClick={() => dispatch({ type: "settings", open: true })}
              >
                打开 Provider / MCP 配置
              </button>
            </div>
          )}
          {navView === "automation" && (
            <div className="rail-section" data-testid="automation-hooks">
              <h3>钩子（有限动作）</h3>
              <p className="muted">
                支持事件：run_finished（入队后续）。连续失败自动熔断。不执行任意命令，不提升权限；
                Skill/规约正文不得自动注册钩子。
              </p>
              <button
                type="button"
                className="btn-sm primary"
                data-testid="add-hook"
                onClick={() => {
                  if (!workspacePath) return;
                  const content = window.prompt("run 完成后自动入队的内容");
                  if (!content) return;
                  void desktop
                    .upsertHook({
                      workspace: workspacePath,
                      event: "run_finished",
                      enabled: true,
                      actions: [{ kind: "queue_followup", content }],
                    })
                    .then(() => desktop.listHooks(workspacePath))
                    .then(acceptWorkspaceResult(setHooks))
                    .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
                }}
              >
                添加完成入队钩子
              </button>
              <div style={{ marginTop: 12, display: "flex", flexDirection: "column", gap: 8 }}>
                {hooks.length === 0 ? (
                  <div className="empty-sm">尚未配置钩子</div>
                ) : (
                  hooks.map((hook) => (
                    <div className="list-card" key={hook.id} data-testid={`hook-${hook.id}`}>
                      <div className="title">{hook.event}</div>
                      <div className="meta">
                        {hook.enabled ? "启用" : "已停用"} ·{" "}
                        {hook.actions.map((a) => a.content || a.kind).join("；")}
                      </div>
                      {hook.enabled && (
                        <button
                          type="button"
                          className="btn-sm"
                          onClick={() => {
                            void desktop
                              .disableHook(hook.id)
                              .then(() => desktop.listHooks(workspacePath))
                              .then(acceptWorkspaceResult(setHooks))
                              .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
                          }}
                        >
                          停用
                        </button>
                      )}
                    </div>
                  ))
                )}
              </div>
              <h3 style={{ marginTop: 16 }}>工作流（有限步骤）</h3>
              <p className="muted">
                仅支持顺序 agent 步骤；预演只校验不执行。步骤上限 8，不建设任意脚本引擎。
                模板编辑不改变已运行实例；运行冻结步骤快照。
              </p>
              <button
                type="button"
                className="btn-sm primary"
                data-testid="add-workflow"
                onClick={() => {
                  if (!workspacePath) return;
                  const name = window.prompt("工作流名称");
                  if (!name) return;
                  const task = window.prompt("步骤 1 任务");
                  if (!task) return;
                  void desktop
                    .saveWorkflow({ workspace: workspacePath, name, steps: [{ task }] })
                    .then(() => desktop.listWorkflows(workspacePath))
                    .then(acceptWorkspaceResult(setWorkflows))
                    .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
                }}
              >
                新建单步工作流
              </button>
              <div style={{ marginTop: 10, display: "flex", flexDirection: "column", gap: 8 }}>
                {workflows.map((wf) => (
                  <div className="list-card" key={wf.id} data-testid={`workflow-${wf.id}`}>
                    <div className="title">{wf.name}</div>
                    <div className="meta">{wf.steps.length} 步</div>
                    <div className="btn-row">
                      <button
                        type="button"
                        className="btn-sm"
                        onClick={() => {
                          void desktop
                            .runWorkflow(workspacePath, wf.id, true)
                            .then((ack) => dispatch({ type: "error", error: { code: "dry_run", message: `预演：${ack.state}` } }))
                            .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
                        }}
                      >
                        预演
                      </button>
                      <button
                        type="button"
                        className="btn-sm primary"
                        data-testid={`run-workflow-${wf.id}`}
                        disabled={readOnly}
                        title={readOnly ? "工作流使用各步骤配置；当前只读模式下仅可预演" : undefined}
                        onClick={() => {
                          void desktop
                            .runWorkflow(workspacePath, wf.id, false)
                            .then(() => dispatch({ type: "railTab", tab: "details" }))
                            .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
                        }}
                      >
                        运行
                      </button>
                    </div>
                  </div>
                ))}
              </div>
              <p className="muted">预演只校验计划结构。运行遇到审批、对账或失败即停止；恢复当前运行不会自动启动剩余步骤。</p>
              <h3 style={{ marginTop: 16 }}>子智能体 / 浏览器</h3>
              <p className="muted">
                合同草案见 .vidt/gui/contracts（AGT-01/BRW-01）。
                父子身份、权限收窄、预算扣减与取消传播落地前，不提供虚假“可用子代理”入口。
                浏览器连接默认仅 loopback；本地应用预览与写操作均需显式确认。
              </p>
            </div>
          )}
          {navView === "stats" && (
            <div className="rail-section" data-testid="stats-empty">
              <h3>用量与费用</h3>
              <p className="muted">
                单次运行的 turns / tools / tokens 在检查器展示。完整统计面板待 STA 工作包接入；未知用量不显示为零。
              </p>
              <div className="muted" style={{ fontSize: "var(--fs-body)", marginBottom: 8 }} data-testid="export-safety">
                导出仅含展示摘要；不含密钥、环境变量值或未脱敏正文。
              </div>
              <div className="list-card" data-testid="usage-overview">
                <div className="title">用量概览</div>
                <div className="meta">
                  近期运行 {state.runs.length} · 待处理 {pendingItems.length}
                </div>
                <div className="muted" style={{ fontSize: "var(--fs-body)" }}>
                  跨运行 token/费用聚合依赖 Journal 证据；未知不显示为零。
                  费用估算需价格来源；无来源时不编造金额。
                </div>
              </div>
              <button
                type="button"
                className="btn-sm"
                data-testid="export-stats"
                onClick={() => {
                  const payload = {
                    runs: state.runs.map((r) => ({
                      run_id: r.run_id,
                      title: r.title,
                      state: r.state,
                      phase: r.phase,
                      updated_at: r.updated_at,
                    })),
                    exported_at: new Date().toISOString(),
                    note: "仅导出展示层摘要；token/费用以 Journal 证据为准，未知不填零。",
                  };
                  void navigator.clipboard?.writeText(JSON.stringify(payload, null, 2));
                  dispatch({ type: "error", error: { code: "export", message: "统计摘要已复制到剪贴板（不含密钥）" } });
                }}
              >
                导出摘要到剪贴板
              </button>
              <h3 style={{ marginTop: 16 }}>会话树</h3>
              <div data-testid="session-tree" style={{ maxHeight: 220, overflow: "auto" }}>
                {!sessionTree || sessionTree.roots.length === 0 ? (
                  <div className="empty-sm">暂无会话树</div>
                ) : (
                  sessionTree.roots.map((rootId) => {
                    const node = sessionTree.nodes[rootId];
                    return (
                      <div key={rootId} className="list-card">
                        <div className="title">{node?.task?.slice(0, 40) || rootId}</div>
                        <div className="meta">
                          {node?.session_id || "—"} · {node?.phase || "—"} · 子节点 {node?.children?.length || 0}
                        </div>
                      </div>
                    );
                  })
                )}
              </div>
              <h3 style={{ marginTop: 16 }}>数据与缓存</h3>
              <div className="list-card" data-testid="commit-review-note">
                <div className="title">本地提交审查</div>
                <div className="meta">
                  暂存与本地提交必须经授权链；GUI 不绕过 Policy 自动 git commit。
                </div>
              </div>
              <div className="list-card" data-testid="storage-note">
                <div className="meta">
                  偏好：~/.cairn/gui-preferences.json · 运行事实：Journal（不在此删除）
                </div>
                <div className="muted" style={{ fontSize: "var(--fs-body)" }}>
                  清理缓存不会删除 Journal；删除历史必须走明确授权的对账/管理命令。
                </div>
              </div>
              <h3 style={{ marginTop: 16 }}>错误与恢复</h3>
              <div data-testid="error-log">
                {errorLog.length === 0 ? (
                  <div className="empty-sm">本次会话暂无错误</div>
                ) : (
                  errorLog.map((item, index) => (
                    <div className="list-card" key={`${item.at}-${index}`}>
                      <div className="title">{item.code}</div>
                      <div className="meta">{item.message}</div>
                      <div className="muted" style={{ fontSize: "var(--fs-label)" }}>{item.at}</div>
                    </div>
                  ))
                )}
              </div>
              <button
                type="button"
                className="btn-sm"
                data-testid="consolidate-memory"
                onClick={() => {
                  void desktop
                    .consolidateMemories({ workspace: workspacePath })
                    .then((count) =>
                      dispatch({ type: "error", error: { code: "consolidate", message: `整理完成，派生 ${count} 项` } }),
                    )
                    .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
                }}
              >
                整理记忆
              </button>
              <h3 style={{ marginTop: 16 }}>记忆候选</h3>
              <p className="muted">
                来源与有效性来自 Journal 投影；记忆不能授予权限或决定运行终态。
                召回解释区分候选与本轮真实注入。
              </p>
              <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
                {memories.length === 0 ? (
                  <div className="empty-sm">暂无记忆候选</div>
                ) : (
                  memories.map((item) => (
                    <div className="list-card" key={item.id} data-testid={`memory-${item.id}`}>
                      <div className="title">{item.kind}</div>
                      <div className="meta">
                        {item.outcome} · {item.status}
                        {item.trusted_verification ? " · 可信验证" : " · 非可信"}
                      </div>
                      {item.created_at ? <div className="meta">{item.created_at}</div> : null}
                      <button
                        type="button"
                        className="btn-sm"
                        data-testid={`inspect-memory-${item.id}`}
                        onClick={() => {
                          void desktop
                            .inspectMemory(workspacePath, item.id)
                            .then((detail) =>
                              dispatch({
                                type: "error",
                                error: {
                                  code: "memory_detail",
                                  message: `来源 run ${detail.candidate.run_id || "—"} · 审计 ${(detail.audit || []).join("、") || "无"}`,
                                },
                              }),
                            )
                            .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
                        }}
                      >
                        详情
                      </button>
                      <button
                        type="button"
                        className="btn-sm"
                        data-testid={`revoke-memory-${item.id}`}
                        onClick={() => {
                          void desktop
                            .revokeMemory({
                              workspace: workspacePath,
                              candidate_id: item.id,
                              reason: "superseded",
                            })
                            .then(() => desktop.listMemories({ workspace: workspacePath }))
                            .then(acceptWorkspaceResult(setMemories))
                            .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
                        }}
                      >
                        撤销
                      </button>
                    </div>
                  ))
                )}
              </div>
            </div>
          )}
        </main>
      )}
      {navView === "workbench" && (
      <main className="main-column">
        {demoBridgeActive() && (
          <div className="error-banner" role="status" data-testid="demo-banner" data-mode="demo">
            <span>
              演示后端：当前显示模拟会话与固定用量，不代表真实 Journal 运行。
              <span className="muted"> · bridge=demo</span>
            </span>
          </div>
        )}
        {!demoBridgeActive() && bridgeMode() === "unavailable" && (
          <div className="error-banner" role="alert" data-testid="bridge-unavailable-banner">
            <span>
              桌面后端不可用：需要 Wails 运行时或显式演示入口（?cairn-demo=1）。
              <span className="muted"> · bridge=unavailable</span>
            </span>
          </div>
        )}
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
          <h1>
            {selectedRun?.title || "新任务"}
            {forkFromRunId ? <span className="muted" style={{ fontSize: "var(--fs-body)", marginLeft: 8 }}>· 将从所选会话分支</span> : null}
          </h1>
          <div className="header-actions">
            <span className="status-pill" data-state={running ? "running" : selectedRun ? selectedRun.state : "idle"}>
              <span className="dot" />
              {statusLabel}
            </span>
            {pendingItems.length > 0 && (
              <button
                type="button"
                className="icon-btn"
                data-testid="inbox-button"
                title={pendingItems.map((i) => `${i.kind}:${i.label}`).join("\n")}
                onClick={() => {
                  const first = pendingItems[0];
                  if (first) {
                    selectCurrentRun(first.runId);
                  }
                }}
              >
                待处理 {pendingItems.length}
              </button>
            )}
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
            {running && state.selectedRunId && (
              <button
                type="button"
                className="icon-btn"
                title="立即补充输入（steering）：在安全点消费；与后续队列语义不同"
                data-testid="steer-run"
                onClick={() => {
                  const content = window.prompt("立即补充输入（steering，非队列）");
                  if (!content || !state.selectedRunId) return;
                  void desktop
                    .submitSteering({ run_id: state.selectedRunId, content })
                    .then(() => dispatch({ type: "railTab", tab: "details" }))
                    .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
                }}
              >
                补充输入
              </button>
            )}
            <button type="button" className="icon-btn" onClick={() => workspacePath && refreshAll(workspacePath)}>
              刷新
            </button>
          </div>
        </header>

        <ChatTranscript
          blocks={blocks}
          loading={state.loading || state.booting}
          capability={{
            hasWorkspace: Boolean(workspacePath),
            hasConfig: Boolean(state.workspace?.has_config),
            onPickWorkspace: () => void pickWorkspace(),
            onOpenSettings: () => dispatch({ type: "settings", open: true }),
          }}
          onDecide={decide}
          onSuggestion={(text) => setDraft(text)}
        />

        <Composer
          value={draft}
          onChange={setDraft}
          onSubmit={() => void submitTask()}
          onStop={stopRun}
          onQueue={() => void queueTask()}
          onInsertFile={() => {
            const rel = window.prompt("输入工作区相对路径（敏感文件会被拒绝）");
            if (!rel) return;
            void desktop
              .previewFile(workspacePath, rel)
              .then(acceptWorkspaceResult(() => setDraft((d) => (d ? d + "\n" : "") + `@${rel}`)))
              .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
          }}
          running={running}
          model={model}
          onModelChange={setModel}
          readOnly={readOnly}
          disabled={!workspacePath}
        />
      </main>
      )}
      <InspectorRail
        key={`inspector:${workspacePath}`}
        tab={state.railTab}
        onTab={(tab) => dispatch({ type: "railTab", tab })}
        workspace={state.workspace}
        runId={selectedRunPersisted ? state.selectedRunId : null}
        statusLabel={statusLabel}
        onError={onError}
        onRefreshThreads={() => workspacePath && refreshAll(workspacePath)}
        queueVersion={queueVersion}
        runRevision={state.runDetail?.events?.length ?? 0}
        streamEvents={state.streamEvents}
        onSelectRun={selectSubagentRun}
        runReturnDepth={runReturnStack.length}
        onBackToParent={backToParentRun}
        onQueueAction={(action, inputId) => {
          if (!workspacePath) {
            return;
          }
          const identity = workspaceIdentityRef.current;
          const done = () => {
            if (workspaceIdentityRef.current !== identity) return;
            void refreshAll(workspacePath);
            setQueueVersion((v) => v + 1);
          };
          if (action === "confirm") {
            void desktop
              .confirmQueue({ workspace: workspacePath, input_id: inputId, mode: readOnly ? "read-only" : "default", model: model.trim() || undefined })
              .then((ack) => {
                knownRunWorkspaceRef.current.set(ack.run_id, identity.path);
                if (workspaceIdentityRef.current !== identity) return;
                if (ack.run_id) {
                  if (["queued_starting", "starting", "running", "resuming"].includes(ack.state) && !settledRunsRef.current.has(ack.run_id)) activeRunsRef.current.add(ack.run_id);
                  selectCurrentRun(ack.run_id);
                }
                done();
              })
              .catch((error) => {
                dispatch({ type: "error", error: errorBodyOf(error) });
                done();
              });
            return;
          }
          void desktop
            .cancelQueue({ workspace: workspacePath, input_id: inputId, reason: "user cancel" })
            .then(done)
            .catch((error) => {
              dispatch({ type: "error", error: errorBodyOf(error) });
              done();
            });
        }}
      />

      <SettingsDrawer
        key={`settings:${workspacePath}`}
        open={state.settingsOpen}
        onClose={() => dispatch({ type: "settings", open: false })}
        workspace={state.workspace}
        workspacePath={workspacePath}
        theme={theme}
        onTheme={setTheme}
        fontSize={fontSize}
        onFontSize={setFontSize}
        railWidth={railWidth}
        onRailWidth={setRailWidth}
        readOnly={readOnly}
        onReadOnly={(value) => {
          setReadOnly(value);
          localStorage.setItem("cairn.readonly", value ? "1" : "0");
        }}
        onError={onError}
        onConfigChanged={() => { dispatch({ type: "error", error: null }); void refreshAll(workspacePath); }}
      />

      <ThreadContextMenu
        open={Boolean(state.contextRun)}
        onClose={() => dispatch({ type: "context", run: null })}
        title={state.contextRun?.title || ""}
        onRename={(title) => {
          if (workspacePath && state.contextRun) {
            void desktop
              .renameSession(state.contextRun.sessionId, title)
              .then(() => refreshAll(workspacePath))
              .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
          }
        }}
        onPin={() => {
          if (workspacePath && state.contextRun) {
            // 置顶是切换语义；归档状态保持不变。
            const nextPinned = !state.contextRun.pinned;
            void desktop
              .setSessionFlags(state.contextRun.sessionId, nextPinned, false)
              .then(() => refreshAll(workspacePath))
              .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
          }
        }}
        onArchive={() => {
          if (workspacePath && state.contextRun) {
            void desktop
              .setSessionFlags(state.contextRun.sessionId, state.contextRun.pinned, true)
              .then(() => refreshAll(workspacePath))
              .catch((error) => dispatch({ type: "error", error: errorBodyOf(error) }));
          }
        }}
        onFork={() => {
          if (!workspacePath || !state.contextRun) {
            return;
          }
          // 分支：清空输入区，下一次发送以来源 run 为 fork_source_run_id。
          setForkFromRunId(state.contextRun.id);
          selectCurrentRun(null);
          setDraft("");
        }}
      />
    </div>
  );
}
