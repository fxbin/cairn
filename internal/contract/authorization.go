// authorization.go 定义 Core 与 Runtime 共用的纯授权数据合同。
// 本合同不执行策略、不访问工作区，也不授予权限。
// Author: fxbin
package contract

import "encoding/json"

// AuthorizationEffect 是授权判定的闭合词表；实现不得把未知值视为允许。
type AuthorizationEffect string

const (
	// AuthorizationAllow 表示当前策略允许直接执行。
	AuthorizationAllow AuthorizationEffect = "allow"
	// AuthorizationAsk 表示必须取得人工审批后才可执行。
	AuthorizationAsk AuthorizationEffect = "ask"
	// AuthorizationDeny 表示拒绝执行。
	AuthorizationDeny AuthorizationEffect = "deny"
)

// AuthorizationDecision 携带策略结论、审批绑定与结构化拒绝信息。
// 事件写入方显式选择持久化字段，本合同不改变既有事件载荷。
type AuthorizationDecision struct {
	MCPIdentity          string
	Effect               AuthorizationEffect
	RuleID               string
	Fingerprint          string
	Summary              string
	WorkspaceObservation string
	FailureCode          string
	FailureData          json.RawMessage
}

// PatchPlan 是 Core 记录改写摘要与生成回退快照所需的最小计划；不包含执行能力。
type PatchPlan struct {
	Path       string
	Mode       string
	Before     []byte
	BeforeHash string
	AfterHash  string
	Exists     bool
}
