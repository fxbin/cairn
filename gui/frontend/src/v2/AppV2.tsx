/**
 * Cairn Desktop 2.0：独立的新工作入口。
 * 只消费现有 desktop 桥接与 Journal 投影，不复制任何 Agent 运行逻辑。
 * Author: fxbin
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { desktop } from "../desktop/client";
import DiffViewerV2 from "./DiffViewerV2";
import { ReconciliationV2, AbortRunV2 } from "./RecoveryV2";
import { QueuePanelV2 } from "./QueuePanelV2";
import CapabilitySettingsV2 from "./CapabilitySettingsV2";
import ProviderSettingsV2 from "./ProviderSettingsV2";
import UsageV2 from "./UsageV2";
import FilesPanelV2 from "./FilesPanelV2";
import SessionRowV2 from "./SessionRowV2";
import WorkflowsV2 from "./WorkflowsV2";
import { toSubagentViews } from "../desktop/timeline";
import { buildChatBlocks } from "../desktop/chatBlocks";
import type { ChatBlock } from "../desktop/chatBlocks";
import type { ChangeSet, EvidenceView, RecentProject, RunDetail, RunHistoryView, StreamEvent, WorkspaceStatus } from "../desktop/types";

type Panel = "closed" | "workbench" | "changes" | "evidence" | "queue" | "subagents" | "files";
type Page = "home" | "workflow" | "settings";
type SettingsCategory = "general" | "models" | "capabilities" | "usage";
const finished = new Set(["completed", "failed", "cancelled"]);
const icon = "/icons/cairn-icon-128.png";

function textError(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function statusName(run: RunHistoryView | undefined): string {
  if (!run) return "就绪";
  // 运行状态文本优先使用服务端投影，不在 UI 猜测事件终态。
  return run.phase_label || run.phase || run.state;
}

function Transcript({ blocks, onApproval }: {
  blocks: ChatBlock[];
  onApproval: (approve: boolean, fingerprint: string, runId: string) => void;
}) {
  return <div className="v2-transcript" aria-live="polite">
    {blocks.map((block) => {
      if (block.kind === "user") return <div className="v2-message-user" key={block.id}>{block.text}</div>;
      if (block.kind === "agent") return <article className="v2-message-agent" key={block.id}>
        <ReactMarkdown remarkPlugins={[remarkGfm]} skipHtml>{block.text}</ReactMarkdown>
      </article>;
      if (block.kind === "tool") return <details className="v2-tool" key={block.id} open={block.status === "failed"}>
        <summary><span className="v2-tool-mark">⌁</span><strong>{block.name}</strong><span>{block.summary}</span><em>{block.status === "ok" ? "完成" : block.status === "failed" ? "失败" : "执行中"}</em></summary>
        {block.detail && <pre>{block.detail}</pre>}
      </details>;
      if (block.kind === "approval") return <section className="v2-approval" key={block.id}>
        <div className="v2-label">等待你的授权</div>
        <strong>{block.summary}</strong>
        <p>此操作具有副作用。仅批准当前指纹对应的操作，不会扩大后续运行权限。</p>
        <details><summary>查看操作指纹</summary><code>{block.fingerprint}</code></details>
        {block.resolved ? <div className="v2-label">{block.resolved === "approved" ? "已授权" : "已拒绝"}</div> :
          <div className="v2-actions"><button className="v2-primary" onClick={() => onApproval(true, block.fingerprint, block.runId)}>允许并继续</button><button onClick={() => onApproval(false, block.fingerprint, block.runId)}>拒绝</button></div>}
      </section>;
      return <div className="v2-status" key={block.id}><span>●</span>{block.label}{block.detail ? " · " + block.detail : ""}</div>;
    })}
  </div>;
}

export default function AppV2() {
  const [recent, setRecent] = useState<RecentProject[]>([]);
  const [workspace, setWorkspace] = useState("");
  const [workspaceStatus, setWorkspaceStatus] = useState<WorkspaceStatus | null>(null);
  const [bridgeStatus, setBridgeStatus] = useState<"connecting" | "healthy" | "disconnected">("connecting");
  const [runs, setRuns] = useState<RunHistoryView[]>([]);
  const [runId, setRunId] = useState<string | null>(null);
  const [detail, setDetail] = useState<RunDetail | null>(null);
  const [liveEvents, setLiveEvents] = useState<StreamEvent[]>([]);
  const [draft, setDraft] = useState("");
  const [sendingText, setSendingText] = useState("");
  const [sending, setSending] = useState(false);
  const [queueRevision, setQueueRevision] = useState(0);
  const [steeringText, setSteeringText] = useState("");
  const [model, setModel] = useState("");
  const [readOnly, setReadOnly] = useState(false);
  const [fontSize, setFontSize] = useState(() => Math.min(19, Math.max(12, Number(localStorage.getItem("cairn.v2.fontSize") || 14))));
  const [page, setPage] = useState<Page>("home");
  const [panel, setPanel] = useState<Panel>("closed");
  const [expandedDiff, setExpandedDiff] = useState(false);
  const [changes, setChanges] = useState<ChangeSet | null>(null);
  const [selectedFile, setSelectedFile] = useState("");
  const [diff, setDiff] = useState("");
  const [evidence, setEvidence] = useState<EvidenceView | null>(null);
  const [settingsTab, setSettingsTab] = useState<SettingsCategory>("general");
  const [notice, setNotice] = useState("");
  const [filter, setFilter] = useState("");
  const [showArchived, setShowArchived] = useState(false);
  const [forkSource, setForkSource] = useState<string | null>(null);
  const [parentRun, setParentRun] = useState<string | null>(null);
  const [sessionTree, setSessionTree] = useState<import("../desktop/types").SessionTreeView | null>(null);
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const projectRef = useRef("");
  const generationRef = useRef(0);
  const runRef = useRef<string | null>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const diffRequestRef = useRef(0);

  const refreshRuns = useCallback(async (path: string, generation: number) => {
    const data = await desktop.listRuns({ workspace: path });
    if (generation === generationRef.current && projectRef.current === path) setRuns(data);
    return data;
  }, []);

  const switchProject = useCallback(async (path: string) => {
    if (!path) return;
    const generation = ++generationRef.current;
    projectRef.current = path;
    runRef.current = null;
    setWorkspace(path);
    setDraft("");
    setModel("");
    setReadOnly(false);
    setRunId(null);
    setDetail(null);
    setLiveEvents([]);
    setSendingText("");
    setWorkspaceStatus(null);
    setBridgeStatus("connecting");
    setRuns([]);
    setForkSource(null);
    setParentRun(null);
    setSessionTree(null);
    setChanges(null);
    setSelectedFile("");
    setDiff("");
    setEvidence(null);
    diffRequestRef.current += 1;
    setPage("home");
    setPanel("closed");
    setNotice("");
    try {
      const [ws, history, health] = await Promise.all([desktop.observeWorkspace(path), desktop.listRuns({ workspace: path }), desktop.health().catch(() => null)]);
      if (generation !== generationRef.current) return;
      setWorkspaceStatus(ws);
      setBridgeStatus(health?.ok ? "healthy" : "disconnected");
      setRuns(history);
      void desktop.sessionTree({workspace:path}).then(tree=>{ if(generation===generationRef.current) setSessionTree(tree); }).catch(()=>{ if(generation===generationRef.current) setSessionTree(null); });
      void desktop.rememberProject(path);
    } catch (err) {
      if (generation === generationRef.current) setNotice(textError(err));
    }
  }, []);

  useEffect(() => { document.documentElement.style.setProperty("--v2-font-size-base", fontSize + "px"); localStorage.setItem("cairn.v2.fontSize", String(fontSize)); }, [fontSize]);

  useEffect(() => {
    let alive = true;
    void desktop.listRecentProjects(false).then((items) => {
      if (!alive) return;
      setRecent(items);
      if (items.length && !projectRef.current) void switchProject(items[0].path);
    }).catch((err) => { if (alive) setNotice(textError(err)); });
    return () => { alive = false; };
  }, [switchProject]);

  const selectRun = useCallback((id: string | null) => {
    setForkSource(null);
    setParentRun(null);
    runRef.current = id;
    setRunId(id);
    setDetail(null);
    if(id && projectRef.current){ const currentWorkspace=projectRef.current;const generation=generationRef.current;void desktop.inspectRun({workspace:currentWorkspace,run_id:id,include_events:true}).then(result=>{if(generation===generationRef.current && runRef.current===id)setDetail(result);}).catch(error=>{if(generation===generationRef.current && runRef.current===id)setNotice(textError(error));}); }
    setLiveEvents([]);
    setSendingText("");
    setPanel("closed");
    setPage("home");
  }, []);

  // HTTP bridge 也需要可用：按照现有 Journal 事实定期同步，不把 ACK 当成完成。
  useEffect(() => {
    if (!workspace) return;
    let cancelled = false;
    const generation = generationRef.current;
    const poll = async () => {
      try {
        const history = await refreshRuns(workspace, generation);
        const id = runRef.current;
        if (!id || !history.some((item) => item.run_id === id)) return;
        const data = await desktop.inspectRun({ workspace, run_id: id, include_events: true });
        if (!cancelled && generationRef.current === generation && runRef.current === id) setDetail(data);
      } catch (err) {
        // 已持久化运行的读取失败不可静默吞掉；特别是 Journal 损坏必须可见。
        if (!cancelled && generationRef.current === generation) setNotice(textError(err));
      }
    };
    void poll();
    const timer = window.setInterval(() => { void poll(); }, 2000);
    return () => { cancelled = true; window.clearInterval(timer); };
  }, [workspace, refreshRuns]);

  useEffect(() => {
    if (!workspace) return;
    const generation = generationRef.current;
    const unbind = desktop.onStream((evt) => {
      if (generationRef.current !== generation || projectRef.current !== workspace) return;
      if (evt.workspace && evt.workspace !== workspace) return;
      if (evt.run_id && evt.run_id === runRef.current) {
        setLiveEvents((previous) => {
          const key = evt.event?.event_id;
          if (key && previous.some((item) => item.event?.event_id === key)) return previous;
          return previous.concat(evt).slice(-500);
        });
      }
      if (evt.kind === "run.event" || evt.kind === "run.status") void refreshRuns(workspace, generation);
    });
    return unbind;
  }, [workspace, refreshRuns]);

  const selected = runs.find((run) => run.run_id === runId);
  const running = selected ? !finished.has(selected.state) && !selected.waiting_approval && !selected.waiting_reconcile : (sending || Boolean(runId));
  const subagents = useMemo(() => toSubagentViews(detail?.events || []), [detail?.events]);
  const onQueueError = useCallback((message: string) => setNotice(message), []);
  const onRunFromQueue = useCallback((id: string) => {
    if (id) { runRef.current = id; setRunId(id); setDetail(null); setLiveEvents([]); setPanel("closed"); setPage("home"); }
  }, []);
  const blocks = useMemo(() => {
    const projected = buildChatBlocks(detail?.events || [], liveEvents);
    if (sendingText && !projected.some((block) => block.kind === "user" && block.text === sendingText)) {
      projected.unshift({ kind: "user", id: "pending-user", text: sendingText, at: "" });
    }
    return projected;
  }, [detail, liveEvents, sendingText]);

  useEffect(() => { if (typeof bottomRef.current?.scrollIntoView === "function") bottomRef.current.scrollIntoView({ block: "end", behavior: "auto" }); }, [blocks.length, runId]);

  const startTask = async () => {
    const content = draft.trim();
    if (!workspace || !content || sending || running || selected?.waiting_approval || selected?.waiting_reconcile) return;
    const generation = generationRef.current;
    const previous = !forkSource && selected && finished.has(selected.state) ? selected : null;
    setSending(true);
    setSendingText(content);
    setDraft("");
    try {
      const ack = await desktop.startRun({
        workspace, task: content, model: model.trim() || undefined,
        mode: readOnly ? "read-only" : "default",
        session_id: previous?.session_id,
        prev_run_id: previous?.run_id,
        require_session_tail: Boolean(previous),
        fork_source_run_id: forkSource || undefined,
      });
      if (generation !== generationRef.current) return;
      runRef.current = ack.run_id;
      setRunId(ack.run_id);
      setForkSource(null);
      setParentRun(null);
      setDetail(null);
      setLiveEvents([]);
      setPage("home");
      await refreshRuns(workspace, generation);
    } catch (err) {
      if (generation === generationRef.current) {
        setDraft((value) => value ? content + "\n" + value : content);
        setNotice(textError(err));
        setSendingText("");
      }
    } finally { if (generation === generationRef.current) setSending(false); }
  };

  const submitQueued = async () => {
    const text = draft.trim();
    if (!workspace || !text || sending) return;
    const generation = generationRef.current;
    setSending(true);
    setDraft("");
    try {
      const ack = await desktop.submitQueue({ workspace, content: text });
      if (generation !== generationRef.current) return;
      setQueueRevision((n) => n + 1);
      setPanel("queue");
      setNotice(`已提交队列 ${ack.input_id}，仍需人工确认才会启动新运行。`);
    } catch (e) { if (generation === generationRef.current) { setDraft((current)=>current ? text+"\n"+current : text);setNotice(textError(e));} }
    finally { if (generation === generationRef.current) setSending(false); }
  };

  const submitSteering = async () => {
    const text = steeringText.trim();
    if (!text || !runId || !running) return;
    const generation = generationRef.current;
    try {
      await desktop.submitSteering({ run_id: runId, content: text });
      if (generation !== generationRef.current) return;
      setSteeringText("");
      setNotice("补充指令已提交；由 Agent 在安全点消费。");
    } catch (e) { if (generation === generationRef.current) setNotice(textError(e)); }
  };

  const decide = async (approve: boolean, fingerprint: string, requestedRunId: string) => {
    try {
      await desktop.resumeRun({ workspace, run_id: requestedRunId, fingerprint, approve, reason: approve ? "desktop 2.0 user approval" : "desktop 2.0 user rejection" });
      setNotice("审批决定已提交；等待 Journal 更新。");
    } catch (err) { setNotice(textError(err)); }
  };

  const openPanel = async (next: Panel) => {
    setPanel((active) => active === next ? "closed" : next);
    setExpandedDiff(false);
    if (!workspace || next === "closed") return;
    const request = ++diffRequestRef.current;
    try {
      if (next === "changes" || next === "workbench") {
        const result = await desktop.listChanges(workspace);
        if (projectRef.current !== workspace || request !== diffRequestRef.current) return;
        setChanges(result);
        const first = result.entries[0]?.relative || "";
        setSelectedFile(first);
        const resultDiff = first ? await desktop.diffFile(workspace, first) : "";
        if (projectRef.current === workspace && request === diffRequestRef.current) setDiff(resultDiff);
      }
      if (next === "evidence" && runId) {
        const result = await desktop.evidence({ workspace, run_id: runId });
        if (projectRef.current === workspace && runRef.current === runId) setEvidence(result);
      }
    } catch (err) { setNotice(textError(err)); }
  };

  const chooseFile = async (relative: string) => {
    const request = ++diffRequestRef.current;
    setSelectedFile(relative);
    setDiff("");
    try {
      const result = await desktop.diffFile(workspace, relative);
      if (projectRef.current === workspace && request === diffRequestRef.current) setDiff(result);
    } catch (err) { setNotice(textError(err)); }
  };

  const pickProject = async () => {
    try {
      const path = await desktop.pickDirectory("选择 Cairn 工作区", workspace);
      if (path) {
        await switchProject(path);
        const items = await desktop.listRecentProjects(false);
        setRecent(items);
      }
    } catch (err) { setNotice(textError(err)); }
  };

  const openSettings = () => { setPage("settings"); setPanel("closed"); };

  const openWorkflow = () => {setPage("workflow");setPanel("closed");};

  const updateSession = async (run: RunHistoryView, title?: string, flags?: {pinned:boolean;archived:boolean}) => {
    if (!run.session_id) throw new Error("运行缺少会话 ID，不能修改会话元数据。");
    const generation = generationRef.current;
    try {
      if (title!==undefined) await desktop.renameSession(run.session_id,title);
      if (flags) await desktop.setSessionFlags(run.session_id,flags.pinned,flags.archived);
      await refreshRuns(workspace,generation);
    } catch (e) { setNotice(textError(e)); throw e; }
  };
  const forkRun = (id:string) => {selectRun(null);setForkSource(id);setDraft("");setNotice("新会话将从选定 Run 分支创建，而不是续接原会话。");};
  const openChild = (childId:string) => { if (!runId) return;setParentRun(runId);selectRun(childId);setParentRun(runId);};

  const compose = <div className="v2-composer">
    {forkSource && <div className="v2-fork-source">从 Run {forkSource} 创建新分支 <button onClick={()=>setForkSource(null)} aria-label="取消分支">×</button></div>}
    <textarea aria-label="描述任务" value={draft} onChange={(event) => setDraft(event.target.value)} placeholder="描述一个任务、问题或你希望实现的功能…" onKeyDown={(event) => {
      if (event.key === "Enter" && (event.ctrlKey || event.metaKey) && !event.nativeEvent.isComposing) {event.preventDefault();void submitQueued();return;}
      if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) { event.preventDefault(); void startTask(); }
    }} />
    <div className="v2-compose-footer">
      <button title="选择工作区" onClick={() => void pickProject()}><span>⌁</span> {workspace.split("/").pop() || "选择工作区"}</button>
      <label>模型 <input aria-label="模型覆盖" placeholder="默认模型" value={model} onChange={(event) => setModel(event.target.value)} /></label>
      <label className="v2-read-only"><input type="checkbox" checked={readOnly} onChange={(e) => setReadOnly(e.target.checked)}/>只读</label>
      {running && runId && <button className="v2-secondary v2-queue-draft" disabled={!draft.trim() || sending} onClick={() => void submitQueued()}>加入队列</button>}
      {running && runId ? <button className="v2-stop" onClick={() => void desktop.cancelRun({ run_id: runId, reason: "user stop" }).catch((err) => setNotice(textError(err)))}>停止</button> :
        <button className="v2-send" title="提交任务" aria-label="提交任务" disabled={!workspace || !draft.trim() || sending || selected?.waiting_approval || selected?.waiting_reconcile} onClick={() => void startTask()}>↑</button>}
    </div>
  </div>;

  const closeToSession = () => { setPage("home"); };
  return <div className={"v2-shell" + (sidebarOpen ? "" : " v2-sidebar-collapsed") + (expandedDiff ? " v2-review-expanded" : "")} data-testid="cairn-v2-shell">
    <aside className="v2-sidebar" aria-label="项目与会话">
      <div className="v2-traffic"><span/><span/><span/></div>
      <div className="v2-brand"><img src={icon} alt="" /><strong>Cairn</strong><small>2.0</small></div>
      <nav className="v2-nav">
        <button className={page === "home" && !runId ? "active" : ""} onClick={() => { selectRun(null); setPage("home"); }}>＋ <span>新建任务</span></button>
        <button className={page === "workflow" ? "active" : ""} onClick={() => void openWorkflow()}>◇ <span>工作流</span></button>
      </nav>
      <div className="v2-sidebar-title">项目 <button title="添加项目" onClick={() => void pickProject()}>＋</button></div>
      <div className="v2-project-list">
        {recent.map((project) => <button key={project.path} className={workspace === project.path ? "current" : ""} onClick={() => void switchProject(project.path)}><span>▤</span><span>{project.name || project.path.split("/").pop()}</span></button>)}
        {!recent.length && <div className="v2-hint">尚未添加工作区</div>}
      </div>
      <div className="v2-sidebar-title">最近会话 <button aria-label="切换已归档会话" onClick={()=>setShowArchived(v=>!v)}>{showArchived?"显示活跃":"查看归档"}</button></div>
      <div className="v2-session-list">
        <input aria-label="搜索会话" placeholder="搜索会话…" value={filter} onChange={(event) => setFilter(event.target.value)}/>
        {runs.filter((run) => run.archived===showArchived && (run.title + run.task).toLowerCase().includes(filter.toLowerCase())).slice(0,50).map((run)=><SessionRowV2 key={run.run_id} run={run} selected={runId===run.run_id} onSelect={()=>selectRun(run.run_id)} onRename={(title)=>updateSession(run,title)} onFlags={(pinned,archived)=>updateSession(run,undefined,{pinned,archived})} onFork={()=>forkRun(run.run_id)}/>)}
      </div>
      <div className="v2-sidebar-bottom"><button onClick={() => void openSettings()}>⚙ <span>设置</span></button><span className="v2-sidebar-version">Cairn Desktop · {bridgeStatus === "healthy" ? "桥接已连接" : bridgeStatus === "connecting" ? "连接中" : "桥接未连接"}</span></div>
    </aside>
    <main className="v2-stage">
      <header className="v2-toolbar">
        <div><button className="v2-plain" title="显示或隐藏侧栏" onClick={() => setSidebarOpen((open) => !open)}>☷</button><span className="v2-crumb">{workspace.split("/").pop() || "Cairn"}{workspaceStatus?.dirty && <span className="v2-dirty" title="工作区存在未提交修改"> ● 未提交</span>}</span>{runId && <span className="v2-divider">/</span>}<strong>{page === "settings" ? "设置" : page === "workflow" ? "工作流" : runId ? selected?.title || "当前会话" : "新建任务"}</strong></div>
        {page === "home" && runId && <div className="v2-toolbar-actions">
          <span className="v2-run-status">{statusName(selected)}</span>
          <button onClick={() => void openPanel("workbench")} aria-pressed={panel === "workbench"}>工作台</button>
          <button onClick={() => void openPanel("changes")} aria-pressed={panel === "changes"}>检查变更</button>
          <button onClick={() => void openPanel("evidence")} aria-pressed={panel === "evidence"}>验证证据</button>
          <button onClick={() => void openPanel("subagents")} aria-pressed={panel === "subagents"}>子智能体</button>
          <button onClick={() => void openPanel("queue")} aria-pressed={panel === "queue"}>队列</button>
          <button onClick={() => void openPanel("files")} aria-pressed={panel === "files"}>文件</button>
        </div>}
      </header>
      {notice && <div className="v2-notice" role="alert"><span>{notice}</span><button aria-label="关闭提示" onClick={() => setNotice("")}>×</button></div>}
      {page === "home" && (!runId ? <div className="v2-home">
        <div className="v2-welcome"><img src={icon} alt="Cairn 石灵图标"/><h1>今天想让 Cairn 做什么？</h1><p>专注写代码，剩下的交给可靠的执行。</p></div>
        {compose}
        <div className="v2-suggestions"><span>从一个目标开始</span><button onClick={() => setDraft("帮我检查当前项目中的潜在问题，并给出有证据的修复建议。")}>分析项目</button><button onClick={() => setDraft("帮我审查当前 Git 变更，指出潜在风险和测试缺口。")}>审查变更</button><button onClick={() => setDraft("为这个功能制定实现计划，先不要修改文件。")}>制定计划</button></div>
        {workspaceStatus && !workspaceStatus.has_config && <p className="v2-config-warning">当前工作区尚未完成 Agent 配置。请先在设置中检查 Provider 与准入状态。</p>}
      </div> : <div className="v2-session">
        <div className="v2-session-scroll"><div className="v2-session-content">
          {parentRun && <button className="v2-parent-run" onClick={()=>selectRun(parentRun)}>← 返回父运行 {parentRun}</button>}
          <div className="v2-task-heading"><span>当前任务</span><h1>{selected?.title || selected?.task || "Agent 会话"}</h1><p>{workspace.split("/").pop()} · {statusName(selected)}</p></div>
          {selected?.waiting_reconcile && runId && <><ReconciliationV2 key={runId} workspace={workspace} runId={runId} events={detail?.events || []} onDone={() => { setNotice("已提交人工对账，请查看 Journal 中的后续状态。"); void refreshRuns(workspace, generationRef.current); }} onError={onQueueError}/><AbortRunV2 key={"abort-"+runId} workspace={workspace} runId={runId} onDone={() => { setNotice("终止请求已提交，等待运行事实更新。"); void refreshRuns(workspace, generationRef.current); }} onError={onQueueError}/></>}
          {blocks.length ? <Transcript blocks={blocks} onApproval={decide}/> : <p className="v2-hint">正在等待 Journal 中的运行记录…</p>}
          {running && runId && <div className="v2-steering"><label htmlFor="v2-steering">运行中补充指令</label><input id="v2-steering" value={steeringText} placeholder="在下一安全点让 Agent 参考…" onChange={(e) => setSteeringText(e.target.value)}/><button disabled={!steeringText.trim()} onClick={() => void submitSteering()}>提交 Steering</button></div>}
          <div ref={bottomRef}/>
        </div></div><div className="v2-session-composer">{compose}</div>
      </div>)}
      {page === "workflow" && <section className="v2-content-page">
        <div className="v2-page-head"><span>WORKFLOWS</span><h1>工作流</h1><p>创建可重复执行的有序步骤；预演不是成功验证。</p></div>
        {workspace ? <WorkflowsV2 key={workspace} workspace={workspace} notify={onQueueError}/> : <p>请先选择工作区。</p>}
        <button className="v2-secondary" onClick={closeToSession}>返回工作区</button>
      </section>}
      {page === "settings" && <div className="v2-settings">
        <nav aria-label="设置分类">{([["general","通用与外观"],["models","模型与 Provider"],["capabilities","Agent 能力"],["usage","用量与成本"]] as const).map(([key,label]) => <button className={settingsTab === key ? "current" : ""} key={key} onClick={() => setSettingsTab(key)}>{label}</button>)}</nav>
        <div className="v2-settings-body">
          <div className="v2-page-head"><span>SETTINGS</span><h1>{settingsTab === "general" ? "通用与外观" : settingsTab === "models" ? "模型与 Provider" : settingsTab === "capabilities" ? "Agent 能力" : "用量与成本"}</h1></div>
          {settingsTab === "general" && <><div className="v2-setting-row"><div><strong>外观</strong><p>Cairn 的宣纸白主题。深色模式与系统同步仍待适配。</p></div><span>纸白</span></div><div className="v2-setting-row"><div><strong>基础字号</strong><p>字体缩放保存于本机，仅影响桌面界面。</p></div><label><input aria-label="基础字号" type="range" min="12" max="19" step="1" value={fontSize} onChange={(e) => setFontSize(Number(e.target.value))}/><span>{fontSize}px</span></label></div></>}
          {settingsTab === "models" && <ProviderSettingsV2 key={workspace} workspace={workspace} notify={onQueueError} />}
          {settingsTab === "capabilities" && <CapabilitySettingsV2 key={workspace} workspace={workspace} notify={onQueueError} />}
          {settingsTab === "usage" && <UsageV2 key={workspace} workspace={workspace} notify={onQueueError} />}
          <button className="v2-secondary" onClick={closeToSession}>返回工作区</button>
        </div>
      </div>}
    </main>
    {page === "home" && runId && panel !== "closed" && <aside className="v2-inspector" aria-label="会话右侧工作台">
      <div className="v2-inspector-head"><strong>{panel === "workbench" ? "Agent 工作台" : panel === "changes" ? "检查变更" : panel === "queue" ? "任务队列" : panel === "subagents" ? "子智能体" : panel === "files" ? "项目文件" : "验证与证据"}</strong><div>{panel === "changes" && <button title={expandedDiff ? "恢复侧边栏" : "放大审查"} onClick={() => setExpandedDiff((open) => !open)}>{expandedDiff ? "还原" : "⛶"}</button>}<button aria-label="关闭右侧面板" onClick={() => { setPanel("closed"); setExpandedDiff(false); }}>×</button></div></div>
      {panel === "files" && <FilesPanelV2 workspace={workspace} notify={onQueueError}/>}
      {panel === "queue" && <QueuePanelV2 workspace={workspace} revision={queueRevision} onRun={onRunFromQueue} onError={onQueueError}/>}
      {panel === "subagents" && <div className="v2-panel-body"><div className="v2-label">Agent Delegate</div><h2>子智能体</h2><p className="v2-hint">会话树：{sessionTree ? Object.keys(sessionTree.nodes).length + " 个 Run" : "暂无可用投影"}</p>{subagents.length ? subagents.map((child) => <div className="v2-queue-entry" key={child.childRunId}><strong>{child.childRunId}</strong><p>{child.state} · {child.outcome || "尚无完成结论"} · {child.usageReported ? "已报告 Token 用量" : "用量未知"}</p><button className="v2-secondary" onClick={() => openChild(child.childRunId)}>查看子运行</button></div>) : <div className="v2-empty">当前 Run 没有已记录的子智能体委派。</div>}</div>}
      {panel === "workbench" && <div className="v2-panel-body"><div className="v2-label">当前运行</div><h2>{selected?.title || "Agent 任务"}</h2><p>{statusName(selected)}</p><div className="v2-panel-separator"/><div className="v2-label">工作区变更</div><p>{changes ? changes.entries.length + " 个文件（工作区实时状态）" : "读取中…"}</p>{changes && <p data-testid="v2-change-summary">新增 {changes.added} · 修改 {changes.modified} · 删除 {changes.deleted} · 未跟踪 {changes.untracked}</p>}<button className="v2-secondary" onClick={() => void openPanel("changes")}>检查变更 →</button></div>}
      {panel === "changes" && <div className="v2-change-layout"><div className="v2-file-list">{changes?.entries.map((item) => <button key={item.relative} className={selectedFile === item.relative ? "current" : ""} onClick={() => void chooseFile(item.relative)}><span>{item.status}</span>{item.relative}</button>)}{changes?.entries.length === 0 && <div className="v2-hint">当前没有文件变更。</div>}</div><div className="v2-diff"><div className="v2-diff-title">{selectedFile || "请选择文件"}</div>{selectedFile ? <DiffViewerV2 text={diff} relative={selectedFile} workspace={workspace} onComment={async (line, body) => { await desktop.saveReviewNote({ workspace, relative: selectedFile, line, body }); setDraft((old) => old ? old + "\n\n" + "审查意见：" + selectedFile + ":" + line + " — " + body : "请根据这条代码审查意见进一步检查：" + selectedFile + ":" + line + " — " + body); setPanel("closed"); setExpandedDiff(false); setNotice("已保存行级审查备注，意见已带入当前会话输入框；尚未执行修改。"); }} /> : <div className="v2-empty">选择文件后查看变更。</div>}</div></div>}
      {panel === "evidence" && <div className="v2-panel-body">{!evidence ? <p>暂无已读取的证据。</p> : <><div className="v2-label">运行终态</div><h2>{evidence.state_label || evidence.state}</h2><div className="v2-setting-row"><strong>验证可信度</strong><span>{evidence.trust_label || (evidence.trusted ? "可信" : "未确认")}</span></div><div className="v2-setting-row"><strong>输入 Token</strong><span>{evidence.metrics.input_tokens.toLocaleString()}</span></div><div className="v2-setting-row"><strong>输出 Token</strong><span>{evidence.metrics.output_tokens.toLocaleString()}</span></div><p className="v2-hint">{evidence.metrics.usage_estimated ? "以上用量包含估算数据。" : "用量来自已记录的运行证据。"}费用不可用时不做推断。</p></>}</div>}
    </aside>}
  </div>;
}
