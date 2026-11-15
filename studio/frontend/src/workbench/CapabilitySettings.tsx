/**
 * Agent 能力治理面板：统一展示 Skills、MCP、记忆、Hooks、命令与项目规约。
 * 配置源与运行事实仍由 Go 端管理，此界面保持全部显式门禁与确认约束。
 * Author: fxbin
 */
import { useEffect, useMemo, useRef, useState } from "react";
import { desktop } from "../desktop/client";

/**
 * Agent 能力治理的六个子面板标识，供外层设置导航与独立标签页共用。
 */
export type CapabilitySection = "skills" | "mcp" | "memory" | "hooks" | "commands" | "instructions";

/**
 * 六类能力的状态计数快照；MCP 未返回列表时保持 null 以免将未知状态伪装为 0。
 */
export interface CapabilityCounts {
  skills: number;
  mcp: number | null;
  memory: number;
  hooks: number;
  commands: number;
  instructions: number;
}

type Skill = { name: string; scope?: string; description?: string; disabled?: boolean };
type Hook = { id: string; event: string; enabled: boolean; actions: Array<{ kind: string }> };
type Memory = { id: string; kind: string; status: string; trusted_verification: boolean };
type Command = { name: string; description: string; scope: string };

const tabs: Array<[CapabilitySection, string]> = [
  ["skills", "Skills"],
  ["mcp", "MCP"],
  ["memory", "记忆"],
  ["hooks", "Hooks"],
  ["commands", "命令"],
  ["instructions", "项目指令"],
];

/**
 * 计算六个能力标签页对应的状态计数展示文本；MCP 未返回数组时不伪装为 0。
 */
function formatTabCount(id: CapabilitySection, counts: CapabilityCounts): string {
  switch (id) {
    case "skills":
      return String(counts.skills);
    case "mcp":
      return counts.mcp !== null ? String(counts.mcp) : "—";
    case "memory":
      return String(counts.memory);
    case "hooks":
      return String(counts.hooks);
    case "commands":
      return String(counts.commands);
    case "instructions":
      return String(counts.instructions);
  }
}

/**
 * 从记忆检查返回的 JSON 文本中提取可读的审计条目摘要，供来源检查卡片头部展示。
 */
function extractAuditNotes(rawJson: string): string[] {
  if (!rawJson) return [];
  try {
    const parsed: unknown = JSON.parse(rawJson);
    if (parsed && typeof parsed === "object" && "audit" in parsed) {
      const audit = (parsed as { audit?: unknown }).audit;
      if (Array.isArray(audit)) {
        return audit.filter((item): item is string => typeof item === "string");
      }
    }
  } catch {
    return [];
  }
  return [];
}

/**
 * 安全提取 MCP 状态数组中的可读服务条目摘要，不假设未知后端字段存在。
 */
function extractMcpEntries(raw: unknown): Array<{ key: string; name: string; status?: string }> {
  if (!Array.isArray(raw)) return [];
  return raw.map((item, idx) => {
    if (item && typeof item === "object") {
      const record = item as Record<string, unknown>;
      const name =
        typeof record.name === "string" && record.name.trim()
          ? record.name
          : typeof record.id === "string" && record.id.trim()
            ? record.id
            : `MCP 服务 #${idx + 1}`;
      const status =
        typeof record.status === "string" && record.status.trim()
          ? record.status
          : typeof record.state === "string" && record.state.trim()
            ? record.state
            : undefined;
      return { key: `${name}-${idx}`, name, status };
    }
    return { key: `mcp-${idx}`, name: String(item) };
  });
}

/**
 * 渲染带状态计数徽标与操作确认门禁的 Agent 能力治理组件；兼容外层单分区受控与独立标签页两种模式。
 */
export default function CapabilitySettings({
  workspace,
  notify,
  section,
  onCountsChange,
}: {
  workspace: string;
  notify: (message: string) => void;
  section?: CapabilitySection;
  onCountsChange?: (counts: CapabilityCounts) => void;
}) {
  const [tab, setTab] = useState<CapabilitySection>("skills");
  const [skills, setSkills] = useState<Skill[]>([]);
  const [hooks, setHooks] = useState<Hook[]>([]);
  const [memories, setMemories] = useState<Memory[]>([]);
  const [commands, setCommands] = useState<Command[]>([]);
  const [instructions, setInstructions] = useState<Array<{ path: string; excerpt?: string }>>([]);
  const [mcp, setMcp] = useState<unknown>(null);
  const [loaded, setLoaded] = useState(false);
  const [version, setVersion] = useState(0);
  const [busy, setBusy] = useState(false);
  const [source, setSource] = useState("");
  const [scope, setScope] = useState("workspace");
  const [hookText, setHookText] = useState("");
  const [projectText, setProjectText] = useState("");
  const [memoryReason, setMemoryReason] = useState("");
  const [memoryDetail, setMemoryDetail] = useState("");
  const [inspectedMemoryId, setInspectedMemoryId] = useState("");

  const onCountsChangeRef = useRef(onCountsChange);
  onCountsChangeRef.current = onCountsChange;

  useEffect(() => {
    setMemoryDetail("");
    setInspectedMemoryId("");
  }, [workspace]);

  useEffect(() => {
    if (!workspace) {
      setSkills([]);
      setHooks([]);
      setMemories([]);
      setCommands([]);
      setInstructions([]);
      setMcp(null);
      setLoaded(false);
      return;
    }
    let live = true;
    setLoaded(false);
    void Promise.allSettled([
      desktop.listSkills({ workspace }),
      desktop.listHooks(workspace),
      desktop.listMemories({ workspace }),
      desktop.listCommands(),
      desktop.listProjectInstructions(workspace),
      desktop.mcpStatus({ workspace }),
    ]).then((results) => {
      if (!live) return;
      if (results[0].status === "fulfilled") {
        setSkills(Array.isArray(results[0].value) ? (results[0].value as Skill[]) : []);
      }
      if (results[1].status === "fulfilled") {
        setHooks(Array.isArray(results[1].value) ? results[1].value : []);
      }
      if (results[2].status === "fulfilled") {
        setMemories(Array.isArray(results[2].value) ? results[2].value : []);
      }
      if (results[3].status === "fulfilled") {
        setCommands(Array.isArray(results[3].value) ? results[3].value : []);
      }
      if (results[4].status === "fulfilled") {
        setInstructions(
          Array.isArray(results[4].value?.documents) ? results[4].value.documents : []
        );
      }
      if (results[5].status === "fulfilled") {
        setMcp(results[5].value);
      }
      setLoaded(true);
      if (results.some((x) => x.status === "rejected")) {
        notify("部分能力状态读取失败，未将错误伪装成空列表。");
      }
    });
    return () => {
      live = false;
    };
  }, [workspace, version, notify]);

  const mcpCount = Array.isArray(mcp) ? mcp.length : null;

  useEffect(() => {
    if (!loaded) return;
    onCountsChangeRef.current?.({
      skills: skills.length,
      mcp: mcpCount,
      memory: memories.length,
      hooks: hooks.length,
      commands: commands.length,
      instructions: instructions.length,
    });
  }, [
    loaded,
    skills.length,
    mcpCount,
    memories.length,
    hooks.length,
    commands.length,
    instructions.length,
  ]);

  const auditNotes = useMemo(() => extractAuditNotes(memoryDetail), [memoryDetail]);
  const mcpEntries = useMemo(() => extractMcpEntries(mcp), [mcp]);

  const action = async (fn: () => Promise<unknown>) => {
    if (busy) return;
    setBusy(true);
    try {
      await fn();
      setVersion((v) => v + 1);
      notify("更改已提交至本地配置。");
    } catch (e) {
      notify(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const counts: CapabilityCounts = {
    skills: skills.length,
    mcp: mcpCount,
    memory: memories.length,
    hooks: hooks.length,
    commands: commands.length,
    instructions: instructions.length,
  };

  const activeTab: CapabilitySection = section ?? tab;

  return (
    <div className="wb-capabilities" data-testid="capabilities-settings">
      {section === undefined && (
        <div className="wb-capability-tabs">
          {tabs.map(([id, label]) => (
            <button
              key={id}
              type="button"
              aria-label={label}
              className={activeTab === id ? "current" : ""}
              onClick={() => setTab(id)}
              style={{ display: "inline-flex", alignItems: "center", gap: 6 }}
            >
              <span>{label}</span>
              <span
                className={"wb-badge " + (activeTab === id ? "is-accent" : "is-muted")}
                aria-hidden="true"
              >
                {formatTabCount(id, counts)}
              </span>
            </button>
          ))}
        </div>
      )}

      <div className="wb-capability-body">
        {activeTab === "skills" && (
          <>
            <section className="wb-settings-section">
              <h3 className="wb-settings-section-title">安装技能</h3>
              <div className="wb-settings-card wb-settings-card-pad">
                <div className="wb-setting-meta">
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <strong>从明确来源注册 Skill</strong>
                    <span className="wb-badge is-muted">不授予执行权限</span>
                  </div>
                  <p>真实 Skill Store；来源不等于授权，启用不会自动执行脚本。</p>
                </div>
                <div className="wb-inline-form" style={{ margin: "12px 0 0" }}>
                  <input
                    aria-label="技能来源"
                    placeholder="明确的技能来源路径"
                    value={source}
                    onChange={(e) => setSource(e.target.value)}
                  />
                  <select
                    value={scope}
                    aria-label="技能作用域"
                    onChange={(e) => setScope(e.target.value)}
                  >
                    <option value="workspace">当前项目</option>
                    <option value="user">当前用户</option>
                  </select>
                  <button
                    type="button"
                    className="wb-primary"
                    disabled={busy || !source.trim()}
                    onClick={() =>
                      void action(async () => {
                        await desktop.installSkill(workspace, scope, source.trim());
                        setSource("");
                      })
                    }
                  >
                    安装
                  </button>
                </div>
              </div>
            </section>

            <section className="wb-settings-section">
              <h3 className="wb-settings-section-title">已安装技能</h3>
              <div className="wb-settings-card">
                {skills.map((item) => (
                  <div className="wb-setting-row" key={(item.scope || "") + item.name}>
                    <div className="wb-setting-meta">
                      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                        <strong>{item.name}</strong>
                        <span className={"wb-badge " + (item.disabled ? "is-muted" : "is-ok")}>
                          {item.disabled ? "已停用" : "已启用"}
                        </span>
                        <span className="wb-setting-key">{item.scope || "未知作用域"}</span>
                      </div>
                      <p>
                        {item.description || "无描述"} · {item.scope || "未知作用域"}
                      </p>
                    </div>
                    <div className="wb-actions">
                      <button
                        type="button"
                        disabled={busy || !item.scope}
                        onClick={() =>
                          void action(() =>
                            desktop.setSkillEnabled(
                              workspace,
                              item.scope!,
                              item.name,
                              Boolean(item.disabled)
                            )
                          )
                        }
                      >
                        {item.disabled ? "启用" : "停用"}
                      </button>
                      <button
                        type="button"
                        disabled={busy || !item.scope}
                        onClick={() => {
                          if (window.confirm("确定卸载当前作用域的技能？")) {
                            void action(() => desktop.removeSkill(workspace, item.scope!, item.name));
                          }
                        }}
                      >
                        卸载
                      </button>
                    </div>
                  </div>
                ))}
                {!skills.length && (
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>暂无已安装的 Skills。</strong>
                      <p>当前工作区与用户目录下未注册任何 Skill 条目。</p>
                    </div>
                  </div>
                )}
              </div>
              <p className="wb-settings-footnote">
                Skill / MCP / AGENTS.md 不会自动授予执行权限；所有写入与命令执行仍严格经由 Policy 与人工审批门禁控制。
              </p>
            </section>
          </>
        )}

        {activeTab === "mcp" && (
          <section className="wb-settings-section">
            <h3 className="wb-settings-section-title">MCP 运行时状态</h3>
            <div className="wb-settings-card">
              <div className="wb-setting-row">
                <div className="wb-setting-meta">
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <strong>可见服务器</strong>
                    <span className="wb-setting-key">mcp.status</span>
                  </div>
                  <p>MCP 状态来自原生诊断。服务连接不意味着工具已被授权。</p>
                </div>
                <span className={"wb-badge " + (Array.isArray(mcp) ? "is-ok" : "is-muted")}>
                  {Array.isArray(mcp) ? `${mcp.length} 个` : "详细状态需诊断"}
                </span>
              </div>

              {mcpEntries.map((entry) => (
                <div className="wb-setting-row" key={entry.key}>
                  <div className="wb-setting-meta">
                    <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                      <strong>{entry.name}</strong>
                      {entry.status && <span className="wb-badge is-muted">{entry.status}</span>}
                    </div>
                    <p>外部工具调用受 Policy 门禁与 schema 漂移失败关闭策略约束。</p>
                  </div>
                  <span className="wb-badge is-muted">受控接入</span>
                </div>
              ))}

              <div className="wb-setting-row">
                <div className="wb-setting-meta">
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <strong>环境变量与凭据隔离</strong>
                    <span className="wb-badge is-muted">不回显密钥</span>
                  </div>
                  <p>此界面不回显敏感环境变量，服务器安装与权限配置继续由明确的配置入口管理。</p>
                </div>
              </div>
            </div>
            <p className="wb-settings-footnote">
              MCP 声明不能授予任何权限；外部工具调用出现 schema 漂移或结果不确定时立即失败关闭，不自动重试。
            </p>
          </section>
        )}

        {activeTab === "memory" && (
          <>
            <section className="wb-settings-section">
              <h3 className="wb-settings-section-title">记忆治理与门禁</h3>
              <div className="wb-settings-card">
                <div className="wb-setting-row" style={{ flexWrap: "wrap", gap: 12 }}>
                  <div className="wb-setting-meta">
                    <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                      <strong>撤销说明门禁</strong>
                      <span className="wb-badge is-muted">必填理由</span>
                    </div>
                    <p>记忆候选独立于 Journal，不能视作事实或自动学习结论；撤销前须提供明确原因。</p>
                  </div>
                  <div className="wb-inline-form" style={{ margin: 0, flex: "1 1 260px" }}>
                    <input
                      aria-label="记忆撤销说明"
                      value={memoryReason}
                      onChange={(e) => setMemoryReason(e.target.value)}
                      placeholder="说明为何该候选不再适用"
                    />
                  </div>
                </div>

                <div className="wb-setting-row">
                  <div className="wb-setting-meta">
                    <strong>整理工作区候选</strong>
                    <p>按后端巩固规则重整当前工作区的候选记忆条目，不会篡改 Event Journal。</p>
                  </div>
                  <div className="wb-actions">
                    <button
                      type="button"
                      className="wb-secondary"
                      disabled={busy}
                      onClick={() => void action(() => desktop.consolidateMemories({ workspace }))}
                    >
                      整理候选
                    </button>
                  </div>
                </div>
              </div>
            </section>

            <section className="wb-settings-section">
              <h3 className="wb-settings-section-title">候选记忆列表</h3>
              <div className="wb-settings-card">
                {memories.map((x) => (
                  <div className="wb-setting-row" key={x.id}>
                    <div className="wb-setting-meta">
                      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                        <strong>
                          {x.kind} · {x.id}
                        </strong>
                        <span className="wb-badge is-muted">{x.status}</span>
                        <span
                          className={"wb-badge " + (x.trusted_verification ? "is-ok" : "is-warn")}
                        >
                          {x.trusted_verification ? "可信验证" : "尚无可信验证"}
                        </span>
                      </div>
                      <p>
                        {x.status} · {x.trusted_verification ? "可信验证" : "尚无可信验证"}
                      </p>
                    </div>
                    <div className="wb-actions">
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() =>
                          void desktop
                            .inspectMemory(workspace, x.id)
                            .then((v) => {
                              setInspectedMemoryId(x.id);
                              setMemoryDetail(JSON.stringify(v, null, 2));
                            })
                            .catch((e) => notify(String(e)))
                        }
                      >
                        检查来源
                      </button>
                      <button
                        type="button"
                        disabled={busy || !memoryReason.trim()}
                        onClick={() =>
                          void action(() =>
                            desktop.revokeMemory({
                              workspace,
                              candidate_id: x.id,
                              reason: memoryReason.trim(),
                            })
                          )
                        }
                      >
                        撤销候选
                      </button>
                    </div>
                  </div>
                ))}
                {!memories.length && (
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>当前工作区尚无候选记忆。</strong>
                      <p>尚未生成或保留任何记忆候选条目。</p>
                    </div>
                  </div>
                )}
              </div>
              <p className="wb-settings-footnote">
                Event Journal 是唯一事实源；记忆候选仅作可撤销的上下文参考，绝不替代事实或自动授予权限。
              </p>
            </section>

            {memoryDetail && (
              <section className="wb-settings-section">
                <h3 className="wb-settings-section-title">记忆来源审计</h3>
                <div className="wb-settings-card" style={{ overflow: "hidden" }}>
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                        <strong>记忆来源审计 JSON</strong>
                        {inspectedMemoryId && (
                          <span className="wb-setting-key">{inspectedMemoryId}</span>
                        )}
                        {auditNotes.map((note, idx) => (
                          <span key={idx} className="wb-badge is-ok">
                            {note}
                          </span>
                        ))}
                      </div>
                    </div>
                    <div className="wb-actions">
                      <button
                        type="button"
                        aria-label="关闭来源详情"
                        onClick={() => {
                          setMemoryDetail("");
                          setInspectedMemoryId("");
                        }}
                      >
                        收起
                      </button>
                    </div>
                  </div>
                  <pre className="wb-code" style={{ maxHeight: 280, margin: 0, borderRadius: 0 }}>
                    {memoryDetail}
                  </pre>
                </div>
              </section>
            )}
          </>
        )}

        {activeTab === "hooks" && (
          <>
            <section className="wb-settings-section">
              <h3 className="wb-settings-section-title">注册入队钩子</h3>
              <div className="wb-settings-card wb-settings-card-pad">
                <div className="wb-setting-meta">
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <strong>run_finished → queue_followup</strong>
                    <span className="wb-badge is-muted">白名单动作</span>
                  </div>
                  <p>
                    Hooks 受运行时事件与动作白名单约束。这里只支持 run_finished → queue_followup，不执行命令。
                  </p>
                </div>
                <div className="wb-inline-form" style={{ margin: "12px 0 0" }}>
                  <input
                    aria-label="完成后入队内容"
                    value={hookText}
                    placeholder="完成后入队的任务描述"
                    onChange={(e) => setHookText(e.target.value)}
                  />
                  <button
                    type="button"
                    className="wb-primary"
                    disabled={busy || !hookText.trim()}
                    onClick={() =>
                      void action(async () => {
                        await desktop.upsertHook({
                          workspace,
                          event: "run_finished",
                          enabled: true,
                          actions: [{ kind: "queue_followup", content: hookText.trim() }],
                        });
                        setHookText("");
                      })
                    }
                  >
                    添加完成入队钩子
                  </button>
                </div>
              </div>
            </section>

            <section className="wb-settings-section">
              <h3 className="wb-settings-section-title">已注册 Hooks</h3>
              <div className="wb-settings-card">
                {hooks.map((x) => (
                  <div className="wb-setting-row" key={x.id}>
                    <div className="wb-setting-meta">
                      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                        <strong>{x.event}</strong>
                        <span className={"wb-badge " + (x.enabled ? "is-ok" : "is-muted")}>
                          {x.enabled ? "生效中" : "已停用"}
                        </span>
                        <span className="wb-setting-key">{x.id}</span>
                      </div>
                      <p>{x.actions.map((a) => a.kind).join("、")}</p>
                    </div>
                    <div className="wb-actions">
                      <button
                        type="button"
                        disabled={busy || !x.enabled}
                        onClick={() => void action(() => desktop.disableHook(x.id))}
                      >
                        {x.enabled ? "停用" : "已停用"}
                      </button>
                    </div>
                  </div>
                ))}
                {!hooks.length && (
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>没有已注册的 Hook。</strong>
                      <p>当前工作区未配置任何事件钩子。</p>
                    </div>
                  </div>
                )}
              </div>
              <p className="wb-settings-footnote">
                Hooks 仅允许白名单入队动作，且只能收紧运行时边界；入队后的后续任务仍需人工显式确认启动。
              </p>
            </section>
          </>
        )}

        {activeTab === "commands" && (
          <section className="wb-settings-section">
            <h3 className="wb-settings-section-title">内置命令目录</h3>
            <div className="wb-settings-card">
              {commands.map((x) => (
                <div className="wb-setting-row" key={x.name}>
                  <div className="wb-setting-meta">
                    <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                      <strong>{x.name}</strong>
                      <span className="wb-badge is-muted">{x.scope}</span>
                    </div>
                    <p>{x.description}</p>
                  </div>
                  <span className="wb-setting-key">{x.scope}</span>
                </div>
              ))}
              {!commands.length && (
                <div className="wb-setting-row">
                  <div className="wb-setting-meta">
                    <strong>暂无可展示的内置命令。</strong>
                    <p>只展示与 CLI 同源的内置命令，不虚构自定义命令能力。</p>
                  </div>
                </div>
              )}
            </div>
            <p className="wb-settings-footnote">
              只展示与 CLI 同源的内置命令注册表；任何命令触发均遵循相同的 Policy、审批与对账顺序。
            </p>
          </section>
        )}

        {activeTab === "instructions" && (
          <>
            <section className="wb-settings-section">
              <h3 className="wb-settings-section-title">编辑工作区 AGENTS.md</h3>
              <div className="wb-settings-card wb-settings-card-pad">
                <div className="wb-setting-meta" style={{ marginBottom: 10 }}>
                  <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                    <strong>当前工作区项目规约</strong>
                    <span className="wb-badge is-muted">需确认覆盖</span>
                  </div>
                  <p>
                    AGENTS.md 为发现到的项目/用户规约，仅作用户级指导，不会授予权限或自动执行文档内命令。
                  </p>
                </div>
                <textarea
                  aria-label="项目规约正文"
                  value={projectText}
                  onChange={(e) => setProjectText(e.target.value)}
                  placeholder="写入当前工作区的 AGENTS.md；不会授予额外权限。"
                  style={{
                    width: "100%",
                    minHeight: 110,
                    resize: "vertical",
                    padding: "10px 12px",
                    borderRadius: 9,
                    border: "1px solid var(--wb-line)",
                    background: "var(--wb-elev, var(--wb-side))",
                    color: "var(--wb-title)",
                  }}
                />
                <div className="wb-actions" style={{ marginTop: 10, justifyContent: "flex-end" }}>
                  <button
                    type="button"
                    className="wb-primary"
                    disabled={busy || !projectText.trim()}
                    onClick={() => {
                      if (window.confirm("写入当前工作区 AGENTS.md？请确认不会覆盖重要规约。")) {
                        void action(() => desktop.writeProjectInstructions(workspace, projectText));
                      }
                    }}
                  >
                    写入项目规约
                  </button>
                </div>
              </div>
            </section>

            <section className="wb-settings-section">
              <h3 className="wb-settings-section-title">已发现的规约快照</h3>
              <div className="wb-settings-card">
                {instructions.map((x) => (
                  <div
                    className="wb-setting-row"
                    key={x.path}
                    style={{ flexDirection: "column", alignItems: "stretch", gap: 8 }}
                  >
                    <div
                      style={{
                        display: "flex",
                        alignItems: "center",
                        justifyContent: "space-between",
                        gap: 8,
                        flexWrap: "wrap",
                      }}
                    >
                      <div className="wb-setting-meta">
                        <strong>{x.path}</strong>
                      </div>
                      <span className="wb-badge is-muted">只读规约快照 · 不授予权限</span>
                    </div>
                    <pre
                      className="wb-code"
                      style={{
                        padding: "10px 12px",
                        borderRadius: 8,
                        background: "var(--wb-elev, var(--wb-side))",
                        whiteSpace: "pre-wrap",
                        maxHeight: 200,
                        margin: 0,
                      }}
                    >
                      {x.excerpt || "无摘要"}
                    </pre>
                  </div>
                ))}
                {!instructions.length && (
                  <div className="wb-setting-row">
                    <div className="wb-setting-meta">
                      <strong>没有检测到项目规约。</strong>
                      <p>当前工作区与用户目录下未发现 AGENTS.md 文件。</p>
                    </div>
                  </div>
                )}
              </div>
              <p className="wb-settings-footnote">
                Skill / MCP / AGENTS.md 不会自动授予执行权限；规约文档仅用于协作上下文提示，不能绕过 Policy 门禁。
              </p>
            </section>
          </>
        )}
      </div>
    </div>
  );
}
