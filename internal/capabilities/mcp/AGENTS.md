# internal/capabilities/mcp/
> L2 | Parent: ../../../AGENTS.md

MCP 的历史投影、请求声明与操作授权。仅消费共享合同和窄来源。
History 不连接服务器。RequestAuthorizer 不能放宽基础 Policy。

## 成员

AGENTS.md: 模块职责地图与文件成员入口。
history.go: History 核验成功双回执并恢复目录。回放不执行外部调用。
history_test.go: 回归 History 核验成功双回执并恢复目录。回放不执行外部调用。
operation_policy.go: OperationPolicy 读取本地提议并绑定审批。stdio 叠加宿主执行限制。
operation_policy_test.go: 验证审批黄金值、stdio 叠加与错误阻断。
request_authorizer.go: RequestAuthorizer 只收紧模型声明。底层 Policy 保留审批与执行身份。
request_authorizer_test.go: 回归 RequestAuthorizer 只收紧模型声明。底层 Policy 保留审批与执行身份。

[PROTOCOL]: Update this header on change, then check AGENTS.md
