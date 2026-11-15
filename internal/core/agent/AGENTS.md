# internal/core/agent/
> L2 | Parent: ../../../AGENTS.md

单 Agent 执行语义。模型、工具和能力只经端口接入。
Journal 是唯一运行事实源。恢复不自动重试未确认操作。

## 成员

AGENTS.md: 模块职责地图与文件成员入口。
artifact_store.go: ArtifactStore 限定内容寻址制品的读写端口。由组合根注入实现。
authorization.go: Authorizer 绑定策略判定、补丁计划与工作区观察。Core 不依赖 Policy 实现。
batch_plan.go: BatchPlan 按只读分类投影工具分组。执行事件仍由 Loop 单写入。
batch_plan_test.go: 回归 BatchPlan 按只读分类投影工具分组。执行事件仍由 Loop 单写入。
context.go: Context 压缩消息并写入可恢复摘要。预算与正文投影共用冻结输入。
context_budget_test.go: 验证实际请求开销、极小预算与持久正文的压缩边界。
context_inventory.go: ExplorationInventory 从成功配对回执提取读取范围。摘要不复制源码。
context_inventory_test.go: 回归 ExplorationInventory 从成功配对回执提取读取范围。摘要不复制源码。
context_journal_test.go: 验证重复压缩的平坦回放和验证失败前的文本保留。
context_provider.go: ContextProvider 在安全点原子冻结文本块。预算与压缩消费同一投影。
context_provider_test.go: 验证上下文顺序、冻结预算与错误短路。
context_tool_evidence.go: CheckpointEvidence 从脱敏回执提取路径和位置。定位信息受容量边界约束。
context_tool_evidence_test.go: 回归 CheckpointEvidence 从脱敏回执提取路径和位置。定位信息受容量边界约束。
hook_contract.go: HookRule 表达受限钩子动作。权限收紧仍由宿主策略执行。
hook_contract_test.go: 回归 HookRule 表达受限钩子动作。权限收紧仍由宿主策略执行。
loop.go: Loop 串行推进模型、工具和 Journal。终态与可信验证证据分别记录。
loop_async_delegate.go: AsyncChildSet 跟踪进程内子运行信号。父 Loop 在安全点吸收持久化终态。
loop_async_delegate_test.go: 回归 AsyncChildSet 跟踪进程内子运行信号。父 Loop 在安全点吸收持久化终态。
loop_budget.go: BudgetUsage 从持久化事件重建用量。预算检查覆盖恢复路径。
loop_budget_recovery_test.go: 验证恢复入口重建用量并拒绝超预算执行。
loop_checkpoint.go: DecisionCheckpoint 在只读探索后补充说明提示。检查点不强制写入。
loop_checkpoint_runtime_test.go: 通过真实工具验证检查点、路径证据与写入边界。
loop_checkpoint_test.go: 回归 DecisionCheckpoint 在只读探索后补充说明提示。检查点不强制写入。
loop_delegate.go: Delegator 受控启动子运行。回调返回后重载父 Journal 投影。
loop_delegate_test.go: 回归 Delegator 受控启动子运行。回调返回后重载父 Journal 投影。
loop_events.go: EventSink 只广播成功追加的事件。消息重建配对历史提议和回执。
loop_exec_feedback_test.go: 验证失败进程的事件、反馈和摘要一致脱敏。
loop_guard.go: LoopGuard 从事件提取重复失败与进展。内部清理不构成业务进展。
loop_guard_test.go: 回归 LoopGuard 从事件提取重复失败与进展。内部清理不构成业务进展。
loop_model.go: Provider 请求冻结工具规格并收集完整响应。连续性制品与用量独立校验。
loop_parallel.go: ReadonlyGroup 并发执行已授权调用。事件与结果按固定顺序落盘。
loop_parallel_test.go: 回归 ReadonlyGroup 并发执行已授权调用。事件与结果按固定顺序落盘。
loop_patch_feedback_test.go: 验证过期补丁反馈与重新读取后的唯一写入。
loop_resume.go: Resume 重放历史并恢复审批。悬挂副作用先对账，只读中断补写结果。
loop_resume_cancel.go: CancellationRepair 补齐取消请求和终态。恢复不猜测副作用结果。
loop_resume_index.go: ToolEventIndex 配对历史调用与模型响应。索引只消费 Journal。
loop_resume_repair.go: OperationRepair 补齐未启动终态和缺失回执。已启动副作用不自动重试。
loop_resume_terminal.go: TerminalRepair 根据持久化证据补齐运行终态。缺少事实时拒绝收尾。
loop_resume_test.go: 回归 Resume 重放历史并恢复审批。悬挂副作用先对账，只读中断补写结果。
loop_scripted.go: ScriptedPlan 不调用模型。固定工具计划复用授权、恢复和验证链。
loop_scripted_test.go: 回归 ScriptedPlan 不调用模型。固定工具计划复用授权、恢复和验证链。
loop_secret_compaction_test.go: 验证压缩与重放不把旧凭据写入模型上下文。
loop_secret_regression_test.go: 验证工具提议在持久化前拒绝凭据材料。
loop_snapshot_test.go: 验证补丁 before 快照完整且可供回退。
loop_terminal.go: RunTerminal 记录取消、预算耗尽和失败。外部清理先于终态。
loop_test.go: 回归 Loop 串行推进模型、工具和 Journal。终态与可信验证证据分别记录。
loop_tools.go: ToolOperation 保持提议、策略、审批与 started 顺序。结果双落盘后通知观察器。
loop_verification.go: Verification 执行冻结命令并校验工作区观察。成功执行不自动等于可信验收。
loop_verification_recovery_test.go: 验证验证命令各中断点的恢复与工作区漂移拒绝。
mcp_lifecycle.go: MCP 清理复用受审计操作链。释放未确认时保留待对账操作。
mcp_lifecycle_test.go: 回归 MCP 清理复用受审计操作链。释放未确认时保留待对账操作。
mcp_test.go: 验证 MCP 声明、审批与不确定结果的恢复边界。
message_inbox.go: MessageInbox 在安全点交付跨运行消息。消息不扩大当前权限。
message_inbox_test.go: 回归 MessageInbox 在安全点交付跨运行消息。消息不扩大当前权限。
minimal_registry_test.go: 验证最小工具端口可运行且缺失能力失败关闭。
project_instructions.go: ProjectInstructions 只恢复创建时的历史快照。正文不能授予工具权限。
project_instructions_test.go: 回归 ProjectInstructions 只恢复创建时的历史快照。正文不能授予工具权限。
request_scope.go: RequestScope 绑定同一授权实例的请求快照。缺失必需作用域时失败关闭。
request_scope_test.go: 回归 RequestScope 绑定同一授权实例的请求快照。缺失必需作用域时失败关闭。
result_observer.go: ToolResultObserver 在双落盘后更新能力。逐事件回放保留模型声明顺序。
result_observer_test.go: 回归 ToolResultObserver 在双落盘后更新能力。逐事件回放保留模型声明顺序。
sensitive.go: CredentialFilter 在工具提议和摘要边界识别凭据。拒绝值不进入事件。
skill_context_test.go: 验证历史正文、冷恢复、压缩及观察器前置门禁。
steering_test.go: 验证安全点消费、输入幂等与冷恢复去重。
telemetry.go: SemanticTelemetry 只消费已持久化事件。导出失败不改变运行事实。
telemetry_test.go: 回归 SemanticTelemetry 只消费已持久化事件。导出失败不改变运行事实。
tool_capabilities.go: MCP 过渡端口传递授权身份并查询会话占用。不承担目录与技能投影。
tool_registry.go: ToolRegistry 提供 Loop 必需的工具端口。能力状态由显式观察器接纳。
ui_prompt_telemetry.go: UIPhaseTracker 统计执行和等待区间。不参与策略或终态判定。
ui_prompt_telemetry_test.go: 回归 UIPhaseTracker 统计执行和等待区间。不参与策略或终态判定。
verification_summary.go: VerificationFeedback 生成有界失败摘要。模型反馈不包含私有验收正文。
verification_summary_test.go: 回归 VerificationFeedback 生成有界失败摘要。模型反馈不包含私有验收正文。

[PROTOCOL]: Update this header on change, then check AGENTS.md
