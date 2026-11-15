# internal/runtime/policy/
> L2 | Parent: ../../../AGENTS.md

基础副作用判定与审批绑定。能力规则通过显式端口接入。
固定工具先判定，未命中才交能力规则。最终判定统一接受 hooks 收紧。

## 成员

AGENTS.md: 模块职责地图与成员入口。
authorizer.go: PatchAuthority 暴露只读规划与工作区观察。Core 不访问 Gate 字段。
dangerous_command.go: DangerousCommand 拒绝固定危险 argv。输出重写提示但不执行。
dangerous_command_test.go: 验证危险 argv 的拒绝与允许边界。
gate.go: Gate 先求固定工具或能力规则，再统一应用 hooks。审批绑定仍由各规则生成。
gate_delegation_test.go: 验证委派沿用执行策略与参数边界。
gate_test.go: 验证补丁、命令和验证计划的独立审批身份。
mcp_test.go: 保留 MCP 身份漂移与宿主执行策略回归。提议只消费共享合同。
safety_event.go: SafetyLog 有界记录安全判定。不改变授权结果。
safety_event_test.go: 验证安全日志容量、快照与统计。

tool_rule_test.go: 验证固定工具优先、规则回退及统一 hooks 收紧。

[PROTOCOL]: Update this header on change, then check AGENTS.md
