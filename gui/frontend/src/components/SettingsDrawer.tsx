import { BudgetContextForm } from "./BudgetContextForm";
import { useEffect, useRef, useState } from "react";
import { desktop } from "../desktop/client";
import type { ErrorBody, ProviderProbeView, WorkspaceStatus } from "../desktop/types";

/**
 * 设置抽屉：主题、只读模式说明、Provider/诊断、MCP、Skills。
 * 与 DeepSeek Harness 设置入口类似——模态抽屉，不打断会话上下文。
 */
export function SettingsDrawer({
  open,
  onClose,
  workspace,
  workspacePath,
  theme,
  onTheme,
  fontSize,
  onFontSize,
  railWidth,
  onRailWidth,
  readOnly,
  onReadOnly,
  onError,
  onConfigChanged,
}: {
  open: boolean;
  onClose: () => void;
  workspace: WorkspaceStatus | null;
  workspacePath: string;
  theme: string;
  onTheme: (value: string) => void;
  fontSize: number;
  onFontSize: (value: number) => void;
  railWidth: number;
  onRailWidth: (value: number) => void;
  readOnly: boolean;
  onReadOnly: (value: boolean) => void;
  onError: (error: ErrorBody) => void;
  onConfigChanged: () => void;
}) {
  const [revision, setRevision] = useState(0);
  const [doctor, setDoctor] = useState<Record<string, unknown> | null>(null);
  const [mcp, setMcp] = useState<unknown>(null);
  const [skills, setSkills] = useState<unknown>(null);
  const [projectDocs, setProjectDocs] = useState<Array<{ source?: string; path: string; bytes?: number; excerpt?: string }>>([]);
  const [presets, setPresets] = useState<Array<{ name: string; protocol: string; model: string; active: boolean }>>([]);
  const [setupBaseURL, setSetupBaseURL] = useState("");
  const [setupModel, setSetupModel] = useState("");
  const [setupKeyEnv, setSetupKeyEnv] = useState("AGENT_API_KEY");
  const [setupBusy, setSetupBusy] = useState(false);
  const [providerNotice, setProviderNotice] = useState("");
  const [probeResult, setProbeResult] = useState<ProviderProbeView | null>(null);
  const [probeBusy, setProbeBusy] = useState(false);
  const [admissionBusy, setAdmissionBusy] = useState(false);
  const [admissionReason, setAdmissionReason] = useState("");
  const providerRequest = useRef(0);
  const currentView = useRef({ workspacePath, open });
  currentView.current = { workspacePath, open };

  useEffect(() => {
    providerRequest.current += 1;
    setProviderNotice("");
    setProbeResult(null);
    setSetupBusy(false);
    setProbeBusy(false);
    setAdmissionBusy(false);
    setAdmissionReason("");
  }, [workspacePath, open]);

  // 操作回执只更新发起时的设置页，防止切换工作区后混入旧结果。
  const providerActionCurrent = (request: number, workspace: string) =>
    request === providerRequest.current && currentView.current.open && currentView.current.workspacePath === workspace;
  const [tab, setTab] = useState<"general" | "provider" | "resources">("general");

  useEffect(() => {
    if (!open || !workspacePath) {
      return;
    }
    let cancelled = false;
    setDoctor(null);
    setPresets([]);
    setProjectDocs([]);
    void (async () => {
      try {
        const [doc, mcpResult, skillResult, docsResult, presetsResult] = await Promise.all([
          desktop.doctor({ workspace: workspacePath }),
          desktop.mcpStatus({ workspace: workspacePath }).catch(() => null),
          desktop.listSkills({ workspace: workspacePath }).catch(() => null),
          desktop.listProjectInstructions(workspacePath).catch(() => ({ documents: [] as Array<{ source?: string; path: string; bytes?: number; excerpt?: string }> })),
          desktop.listProviderPresets({ workspace: workspacePath }).catch(() => []),
        ]);
        if (!cancelled) {
          setDoctor(doc as Record<string, unknown>);
          setMcp(mcpResult);
          setSkills(skillResult);
          setProjectDocs(Array.isArray(docsResult?.documents) ? docsResult.documents : []);
          setPresets(Array.isArray(presetsResult) ? presetsResult : []);
        }
      } catch (error) {
        if (!cancelled) {
          setDoctor(null);
          onError(
            error && typeof error === "object" && "body" in error
              ? (error as { body: ErrorBody }).body
              : { code: "internal", message: String(error) },
          );
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [open, workspacePath, onError, revision]);

  if (!open) {
    return null;
  }

  return (
    <div className="drawer-overlay" data-testid="settings-drawer" role="dialog" aria-label="设置">
      <div className="drawer">
        <div className="drawer-header">
          <h2>设置</h2>
          <button type="button" className="icon-btn" onClick={onClose} aria-label="关闭设置">
            ✕
          </button>
        </div>
        <div className="rail-tabs" style={{ borderBottom: "1px solid var(--c-border)" }}>
          {(
            [
              ["general", "通用"],
              ["provider", "Provider"],
              ["resources", "Skills / MCP"],
            ] as const
          ).map(([id, label]) => (
            <button key={id} type="button" className={`rail-tab ${tab === id ? "active" : ""}`} onClick={() => setTab(id)}>
              {label}
            </button>
          ))}
        </div>
        <div className="drawer-body">
          {tab === "general" && (
            <>
              <label className="field">
                <span>主题</span>
                <select className="chip-select" value={theme} onChange={(event) => onTheme(event.target.value)}>
                  <option value="dark">深色</option>
                  <option value="light">浅色</option>
                  <option value="system">跟随系统</option>
                </select>
              </label>
              <label className="field">
                <span>正文字号 · {fontSize}px</span>
                <input
                  type="range"
                  min={12}
                  max={18}
                  value={fontSize}
                  data-testid="font-size"
                  onChange={(event) => onFontSize(Number(event.target.value))}
                />
              </label>
              <label className="field">
                <span>检查器宽度 · {railWidth}px</span>
                <input
                  type="range"
                  min={260}
                  max={480}
                  value={railWidth}
                  data-testid="rail-width"
                  onChange={(event) => onRailWidth(Number(event.target.value))}
                />
              </label>
              <label className="field">
                <span>只读模式（下次任务生效）</span>
                <input
                  type="checkbox"
                  checked={readOnly}
                  onChange={(event) => onReadOnly(event.target.checked)}
                  data-testid="readonly-toggle"
                />
                <span className="muted">禁用写入与命令执行，仍可浏览与分析</span>
              </label>
              <BudgetContextForm
                initial={{
                  maxTurns: 30,
                  maxInputTokens: 50000,
                  maxOutputTokens: 8000,
                  autoCompact: true,
                  maxPromptTokens: 50000,
                  maxSummaryBytes: 16384,
                }}
                onSubmit={() => undefined}
              />
              <div className="field">
                <span>当前工作区</span>
                <div className="mono-block">{workspace?.path || workspacePath || "未选择"}</div>
              </div>
              <div className="field" data-testid="config-source">
                <span>配置来源</span>
                <div className="muted">
                  {workspace?.config_loaded ? "已加载" : "未加载"}
                  {workspace?.branch ? ` · ${workspace.branch}` : ""}
                  {workspace?.config_error ? ` · ${workspace.config_error}` : ""}
                </div>
                <div className="mono-block" style={{ marginTop: 6 }}>
                  {workspace?.config_path || "未检测到 .agent/config.yaml"}
                </div>
                <div className="muted" style={{ fontSize: 11 }}>
                  解析顺序：安全默认 → Profile → YAML → 环境 → CLI；界面不绕过指纹与准入。
                </div>
              </div>
              <div className="field" data-testid="project-instructions">
                <span>生效规约（AGENTS.md）</span>
                {projectDocs.length === 0 ? (
                  <div className="muted">未发现用户全局或项目规约</div>
                ) : (
                  projectDocs.map((doc) => (
                    <div className="list-card" key={doc.path}>
                      <div className="title">{doc.path}</div>
                      <div className="meta">
                        {doc.source === "user" ? "用户全局" : "项目"} · {doc.bytes ?? 0} bytes
                      </div>
                      <div className="muted" style={{ fontSize: 11 }}>{doc.excerpt}</div>
                    </div>
                  ))
                )}
                <div className="muted" style={{ fontSize: 11 }}>正文是用户级指导，不能授予权限或自动执行命令。</div>
                <button
                  type="button"
                  className="btn-sm"
                  data-testid="write-instructions"
                  onClick={() => {
                    const content = window.prompt("写入工作区 AGENTS.md 内容");
                    if (content == null || !workspacePath) return;
                    void desktop
                      .writeProjectInstructions(workspacePath, content)
                      .then(() => desktop.listProjectInstructions(workspacePath))
                      .then((docs) => setProjectDocs(docs.documents || []))
                      .catch((error) =>
                        onError(
                          error && typeof error === "object" && "body" in error
                            ? (error as { body: ErrorBody }).body
                            : { code: "internal", message: String(error) },
                        ),
                      );
                  }}
                >
                  写入项目规约
                </button>
              </div>
              <div className="field" data-testid="lifecycle-policy">
                <span>启动与关闭</span>
                <div className="muted">
                  退出时有界取消运行并等待 Journal 对齐；不留下失控任务。关窗即终止（macOS 最后窗口关闭后退出）。
                </div>
              </div>
              <div className="field" data-testid="version-info">
                <span>版本与依赖</span>
                <div className="muted">
                  {doctor && typeof doctor === "object"
                    ? `应用 ${String((doctor as Record<string, unknown>).app_name || "Cairn")} · 构建 ${
                        String((doctor as Record<string, unknown>).build_valid ?? "—")
                      }`
                    : "加载中"}
                </div>
                <div className="muted" style={{ fontSize: 11 }}>
                  运行时版本与模型目录以 Provider/诊断为准；此处不猜测未知版本。
                </div>
              </div>
              <div className="field" data-testid="shortcut-help">
                <span>快捷键</span>
                <div className="muted">
                  Enter 发送 · Shift+Enter 换行 · ⌘/Ctrl+Enter 入队 · ⌘/Ctrl+N 新任务 · ⌘/Ctrl+, 设置 · Esc 关闭 · ? 帮助
                </div>
              </div>
            </>
          )}
          {tab === "provider" && (
            <div className="field">
              <span>配置向导</span>
              <div className="list-card" data-testid="config-wizard">
                <div className="muted" style={{ fontSize: 11 }}>
                  生成 v2 配置；密钥值保存在工作区 .agent/keys.env 或用户 ~/.cairn/keys.env（权限 600），此处只填变量名。覆盖既有配置需 CLI 显式 --overwrite。
                </div>
                <input
                  className="input-sm"
                  placeholder="Base URL"
                  value={setupBaseURL}
                  data-testid="setup-base-url"
                  onChange={(e) => setSetupBaseURL(e.target.value)}
                />
                <input
                  className="input-sm"
                  placeholder="模型名"
                  value={setupModel}
                  data-testid="setup-model"
                  onChange={(e) => setSetupModel(e.target.value)}
                />
                <input
                  className="input-sm"
                  placeholder="API Key 环境变量名，如 MINIMAX_API_KEY（请勿粘贴密钥）"
                  aria-label="API Key 环境变量名"
                  autoComplete="off"
                  spellCheck={false}
                  value={setupKeyEnv}
                  data-testid="setup-key-env"
                  onChange={(e) => setSetupKeyEnv(e.target.value)}
                />
                <button
                  type="button"
                  className="btn-sm primary"
                  data-testid="run-setup"
                  disabled={setupBusy || probeBusy || admissionBusy || !setupBaseURL || !setupModel || !setupKeyEnv || !workspacePath}
                  onClick={() => {
                    if (!workspacePath) return;
                    if (!/^[A-Z_][A-Z0-9_]*$/.test(setupKeyEnv)) {
                      onError({ code: "configuration", message: "此处只填写大写环境变量名，如 MINIMAX_API_KEY；请在本机凭据文件中保存密钥值。" });
                      return;
                    }
                    const request = ++providerRequest.current;
                    const workspace = workspacePath;
                    setProviderNotice("");
                    setProbeResult(null);
                    setSetupBusy(true);
                    void desktop
                      .runProviderSetup({
                        base_url: setupBaseURL,
                        model: setupModel,
                        api_key_env: setupKeyEnv,
                        workspace: workspacePath,
                      })
                      .then((result) => {
                        if (!providerActionCurrent(request, workspace)) return;
                        setRevision((value) => value + 1);
                        onConfigChanged();
                        setProviderNotice(`配置已生成 ${result.config_path}。${result.probe_ran ? "两轮能力探测通过，仍需显式确认准入。" : "尚未实测，请配置本机凭据后显式探测。"}${result.notes?.length ? ` ${result.notes.join("；")}` : ""}`);
                      })
                      .catch((error) => {
                        if (!providerActionCurrent(request, workspace)) return;
                        onError(error && typeof error === "object" && "body" in error
                          ? (error as { body: ErrorBody }).body
                          : { code: "internal", message: String(error) });
                      })
                      .finally(() => { if (providerActionCurrent(request, workspace)) setSetupBusy(false); });
                  }}
                >
                  {setupBusy ? "生成中…" : "生成配置"}
                </button>
              </div>
              {providerNotice && <div role="status" data-testid="provider-notice" className="muted">{providerNotice}</div>}
              <span>诊断 / 预算 / 准入</span>
              <DoctorCards
          report={doctor}
          presets={presets}
          onUsePreset={(name) => {
            if (!workspacePath) return;
            void desktop
              .useProviderPreset(workspacePath, name)
              .then(() => {
                setProbeResult(null);
                setProviderNotice("");
                setRevision((value) => value + 1);
                onConfigChanged();
              })
              .catch((error) =>
                onError(
                  error && typeof error === "object" && "body" in error
                    ? (error as { body: ErrorBody }).body
                    : { code: "internal", message: String(error) },
                ),
              );
          }}
        />
              <button
                type="button"
                className="btn-sm primary"
                data-testid="probe-provider"
                disabled={!workspacePath || setupBusy || probeBusy || admissionBusy}
                onClick={() => {
                  const request = ++providerRequest.current;
                  const workspace = workspacePath;
                  setProbeBusy(true);
                  setProbeResult(null);
                  setProviderNotice("");
                  void desktop.probeProvider({ workspace })
                    .then((result) => {
                      if (!providerActionCurrent(request, workspace)) return;
                      setProbeResult(result);
                      setRevision((value) => value + 1);
                      setProviderNotice(`探测：${result.result} · 工具回路 ${result.tool_roundtrip || "未观测"} · 流式用量 ${result.usage_streamed ? "已确认" : "未确认"}${result.error ? ` · ${result.error}` : ""}`);
                    })
                    .catch((error) => {
                      if (!providerActionCurrent(request, workspace)) return;
                      onError(error && typeof error === "object" && "body" in error
                        ? (error as { body: ErrorBody }).body : { code: "internal", message: String(error) });
                    })
                    .finally(() => { if (providerActionCurrent(request, workspace)) setProbeBusy(false); });
                }}
              >
                {probeBusy ? "探测中…" : "显式探测 Provider"}
              </button>
              {probeResult?.result === "pass" && probeResult.tool_roundtrip === "ok" && probeResult.usage_streamed && probeResult.evidence_fingerprint && probeResult.config_fingerprint && probeResult.request_profile_fingerprint && (
                <div className="list-card" data-testid="provider-admission">
                  <span>确认准入：{probeResult.model}</span>
                  <div className="mono-block">证据 {probeResult.evidence_fingerprint}</div>
                  <div className="muted">确认后允许后续任务使用此 Provider；写入与执行仍各自经过审批。</div>
                  <label className="field">
                    <span>准入理由</span>
                    <input className="input-sm" value={admissionReason} maxLength={256} onChange={(event) => setAdmissionReason(event.target.value)} data-testid="admission-reason" />
                  </label>
                  <button type="button" className="btn-sm primary" data-testid="confirm-provider-admission" disabled={admissionBusy || !admissionReason.trim()} onClick={() => {
                    const request = ++providerRequest.current;
                    const workspace = workspacePath;
                    setAdmissionBusy(true);
                    void desktop.confirmProviderAdmission({
                      workspace, evidence_fingerprint: probeResult.evidence_fingerprint!,
                      config_fingerprint: probeResult.config_fingerprint!, request_profile_fingerprint: probeResult.request_profile_fingerprint!,
                      reason: admissionReason.trim(),
                    }).then(() => {
                      if (!providerActionCurrent(request, workspace)) return;
                      setProbeResult(null);
                      setProviderNotice("已记录明确准入决定。写入、执行与验证继续遵循当前策略。");
                      setRevision((value) => value + 1);
                      onConfigChanged();
                    }).catch((error) => {
                      if (!providerActionCurrent(request, workspace)) return;
                      setProbeResult(null);
                      onError(error && typeof error === "object" && "body" in error
                        ? (error as { body: ErrorBody }).body : { code: "internal", message: String(error) });
                    }).finally(() => { if (providerActionCurrent(request, workspace)) setAdmissionBusy(false); });
                  }}>{admissionBusy ? "记录中…" : "确认准入此 Provider"}</button>
                </div>
              )}
              <div className="muted" style={{ fontSize: 11, marginTop: 6 }}>
                打开页面不会自动探测或消耗额度；探测结论与准入分开。
              </div>
            </div>
          )}
          {tab === "resources" && (
            <>
              <div className="field">
                <span>Skills（本地内容包，不授予权限）</span>
                <SkillCards data={skills} />
              </div>
              <div className="field">
                <span>MCP（声明/状态，不等于已授权）</span>
                <MCPCards data={mcp} />
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

/** 会话行更多操作：重命名/置顶/归档。 */
export function ThreadContextMenu({
  open,
  onClose,
  title,
  onRename,
  onPin,
  onArchive,
  onFork,
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  onRename: (title: string) => void;
  onPin: () => void;
  onArchive: () => void;
  onFork: () => void;
}) {
  const [value, setValue] = useState(title);
  useEffect(() => {
    setValue(title);
  }, [title]);
  if (!open) {
    return null;
  }
  return (
    <div className="drawer-overlay" data-testid="thread-menu" role="dialog">
      <div className="drawer" style={{ width: 360 }}>
        <div className="drawer-header">
          <h2>会话操作</h2>
          <button type="button" className="icon-btn" onClick={onClose}>
            ✕
          </button>
        </div>
        <div className="drawer-body">
          <label className="field">
            <span>重命名</span>
            <input className="input-sm" value={value} onChange={(event) => setValue(event.target.value)} data-testid="rename-input" />
          </label>
          <div className="btn-row">
            <button
              type="button"
              className="btn-sm primary"
              onClick={() => {
                onRename(value);
                onClose();
              }}
            >
              保存名称
            </button>
            <button type="button" className="btn-sm" onClick={() => { onPin(); onClose(); }}>
              置顶 / 取消置顶
            </button>
            <button type="button" className="btn-sm danger" onClick={() => { onArchive(); onClose(); }}>
              归档
            </button>
            <button type="button" className="btn-sm" data-testid="fork-session" onClick={() => { onFork(); onClose(); }}>
              从该会话分支
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}


function asRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" ? (value as Record<string, unknown>) : {};
}

/** DoctorCards 结构化展示诊断事实，不把未知伪装为零。 */
function DoctorCards({ report, presets, onUsePreset }: { report: Record<string, unknown> | null; presets: Array<{ name: string; protocol: string; model: string; active: boolean }>; onUsePreset: (name: string) => void }) {
  if (!report) {
    return <div className="empty-sm">加载中或不可用</div>;
  }
  const provider = asRecord(report.provider);
  const admission = asRecord(report.admission);
  const budget = asRecord(report.budget);
  const resolution = Array.isArray(report.resolution) ? report.resolution.map(asRecord) : [];
  const resolved = new Map(resolution.map((entry) => [String(entry.field), String(entry.value)]));
  const active = presets.find((preset) => preset.active);
  const presetName = active?.name || resolved.get("provider.use") || provider.use || "内联连接";
  const protocol = active?.protocol || resolved.get("provider.protocol") || provider.protocol || "未知";
  return (
    <div data-testid="doctor-cards" style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      <div className="list-card">
        <div className="title">就绪 · {String(report.ready ?? "unknown")}</div>
        <div className="meta">
          配置 {String(report.config_valid ?? "—")} · 构建 {String(report.build_valid ?? "—")} · Journal{" "}
          {String(report.journal_state ?? "—")}
        </div>
      </div>
      <div className="list-card" data-testid="model-profile">
        <div className="title">模型与能力画像</div>
        <div className="meta">
          {String(provider.model || "—")} · 声明能力以冻结配置/能力卡为准
        </div>
        <div className="meta" data-testid="active-binding">
          激活预设 {String(presetName)} · 协议 {String(protocol)}
        </div>
        {report.provider_card && typeof report.provider_card === "object" ? (
          <div className="meta">
            能力卡已解析 · 工具调用{" "}
            {String((report.provider_card as Record<string, unknown>).tool_calls ?? "—")}
          </div>
        ) : (
          <div className="meta">能力卡：{report.provider_card_error ? String(report.provider_card_error) : "未声明"}</div>
        )}
        <div className="muted" style={{ fontSize: 11 }}>
          目录建议 ≠ 能力验证；准入与实测证据分开。
        </div>
      </div>
      <div className="list-card">
        <div className="title">Provider</div>
        <div className="meta">
          {String(provider.base_url || "—")} · {String(provider.model || "—")}
        </div>
        <div className="meta" data-testid="credential-status">
          密钥环境变量 {String(provider.api_key_env || "—")} ·{" "}
          {provider.api_key_present ? "可用" : "未配置（不显示密钥）"}
        </div>
        <div className="muted" style={{ fontSize: 11 }}>
          密钥只经环境变量引用；前端、日志与导出不返回密钥内容。
        </div>
        {report.provider_card_error ? (
          <div className="meta">能力卡错误：{String(report.provider_card_error)}</div>
        ) : null}
      </div>
      <div className="list-card">
        <div className="title">准入</div>
        <div className="meta">
          模式 {String(admission.mode || "—")} · 已准入 {String(admission.admitted ?? "—")}
        </div>
      </div>
      <div className="list-card" data-testid="preset-list">
        <div className="title">命名连接预设</div>
        {presets.length === 0 ? (
          <div className="muted">未配置命名预设</div>
        ) : (
          presets.map((item) => (
            <div className="meta" key={item.name} style={{ display: "flex", gap: 8, alignItems: "center" }}>
              <span>
                {item.name}
                {item.active ? " · 激活" : ""} · {item.protocol} · {item.model}
              </span>
              {!item.active && (
                <button
                  type="button"
                  className="btn-sm"
                  data-testid={`use-preset-${item.name}`}
                  onClick={() => onUsePreset(item.name)}
                >
                  切换
                </button>
              )}
            </div>
          ))
        )}
        <div className="muted" style={{ fontSize: 11 }}>
          不显示密钥；切换预设对下一新 run 生效。
        </div>
      </div>
      <div className="list-card" data-testid="preset-relation">
        <div className="title">预设与生效关系</div>
        <div className="meta">
          模型覆盖/只读等对“下次任务”生效；既有运行按冻结配置处理，不被悄悄改配。
        </div>
        <div className="meta">
          激活预设 {String(presetName)} · 协议 {String(protocol)}
        </div>
      </div>
      <div className="list-card" data-testid="model-params">
        <div className="title">模型参数与预算</div>
        <div className="meta">
          仅展示后端真实支持的参数；运行快照与恢复指纹一致，不支持的值拒绝。
        </div>
      </div>
      <div className="list-card" data-testid="budget-feedback">
        <div className="title">预算与限额</div>
        <div className="meta">
          turns {String(budget.max_turns ?? resolved.get("budget.max_turns") ?? "—")} · tools {String(budget.max_tool_calls ?? resolved.get("budget.max_tool_calls") ?? "—")} · wall{" "}
          {String(budget.max_wall_time ?? resolved.get("budget.max_wall_time") ?? "—")}
        </div>
        <div className="muted" style={{ fontSize: 11 }}>
          提高预算必须显式并进入指纹；运行超限失败关闭，不静默放宽。
        </div>
      </div>
      <div className="list-card" data-testid="policy-health">
        <div className="title">权限策略（只读检查器）</div>
        <div className="meta">
          写入 {String(asRecord(report.policy).write || "—")} · 执行 {String(asRecord(report.policy).exec || "—")}
        </div>
        <div className="muted" style={{ fontSize: 11 }}>
          界面不代批、不扩大权限；保护路径与审批仍由核心 Policy 强制。
        </div>
      </div>
      <div className="list-card" data-testid="sandbox-health">
        <div className="title">执行环境</div>
        <div className="meta">
          后端 {String(asRecord(report.sandbox).backend || "—")}
          {asRecord(report.sandbox).docker_image ? ` · 镜像 ${String(asRecord(report.sandbox).docker_image)}` : ""}
        </div>
        <div className="meta">
          镜像可用性{" "}
          {String(asRecord(report.sandbox).image_available ?? "unknown")}
          <span className="muted">（unknown 不伪装为可用）</span>
        </div>
        <div className="muted" style={{ fontSize: 11 }}>
          交互终端（PTY）与 argv-only 执行合同冲突，当前 blocked；不提供绕过授权的宿主 shell。
        </div>
      </div>
    </div>
  );
}

/** SkillCards 列出技能来源与状态；声明不等于授权。 */
function SkillCards({ data }: { data: unknown }) {
  const items = Array.isArray(data) ? data : [];
  if (items.length === 0) {
    return <div className="empty-sm">未配置技能或不可用</div>;
  }
  return (
    <div data-testid="skill-cards" style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      {items.map((raw, index) => {
        const item = asRecord(raw);
        return (
          <div className="list-card" key={String(item.name || index)}>
            <div className="title">{String(item.name || "—")}</div>
            <div className="meta">{String(item.description || "")}</div>
            <div className="meta">
              来源 {String(item.scope || "—")} · {item.disabled ? "已禁用" : "启用"} ·{" "}
              {item.manual_only ? "仅手动" : "可自动"}
            </div>
          </div>
        );
      })}
    </div>
  );
}

/** MCPCards 列出服务器声明与事实状态；未连接不得显示在线。 */
function MCPCards({ data }: { data: unknown }) {
  const items = Array.isArray(data) ? data : [];
  if (items.length === 0) {
    return <div className="empty-sm">未配置 MCP 服务器或不可用</div>;
  }
  return (
    <div data-testid="mcp-cards" style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      {items.map((raw, index) => {
        const item = asRecord(raw);
        return (
          <div className="list-card" key={String(item.name || index)} data-testid={`mcp-${String(item.name || index)}`}>
            <div className="title">{String(item.name || "—")}</div>
            <div className="meta">
              {item.disabled ? "已禁用" : "已声明"}
              {item.credentials_present ? " · 凭据存在" : " · 无凭据"}
            </div>
            <div className="meta">
              传输 {String(item.transport || "—")} · 连接策略 {String(item.connect_policy || "—")} · 调用策略{" "}
              {String(item.call_policy || "—")}
            </div>
            <div className="muted" style={{ fontSize: 11 }}>
              未连接探测不会显示“在线”；列表不启动外部进程。工具目录仅在成功 Journal 回执后展示；schema 漂移失败关闭。
              resources/prompts 仅对协议适配器实际支持的能力提供浏览，不造条目。
              导入前审查 argv/权限/身份；导出不含凭据，导入不自动启动。
              连接诊断需显式请求；超时/取消/失败可观测，不后台静默启动 stdio 服务。
            </div>
          </div>
        );
      })}
    </div>
  );
}
