// authorization.go 定义 Agent Loop 消费的最小授权合同。
// 合同只承载判定结果、改写计划与工作区观察值，不暴露 Policy 的内部结构；
// Deny 优先、不可放宽与 Ask 收紧的语义仍由宿主注入的授权实现持有。
// Author: fxbin
package agent

import (
	"encoding/json"

	"github.com/fxbin/cairn/internal/contract"
)

// AuthorizationEffect 是授权判定结果的闭合词表。
type AuthorizationEffect = contract.AuthorizationEffect

const (
	// AuthorizationAllow 表示当前策略允许直接执行。
	AuthorizationAllow = contract.AuthorizationAllow
	// AuthorizationAsk 表示必须取得人工审批后才可执行。
	AuthorizationAsk = contract.AuthorizationAsk
	// AuthorizationDeny 表示拒绝执行。
	AuthorizationDeny = contract.AuthorizationDeny
)

// AuthorizationDecision 复用共享合同，使宿主实现无需反向依赖 Core。
type AuthorizationDecision = contract.AuthorizationDecision

// PatchPlan 是 Core 记录 operation_started 与生成回退快照所需的最小改写计划。
type PatchPlan = contract.PatchPlan

// Authorizer 是 Core 消费的授权端口：判定提议、为冻结验证命令生成独立判定、
// 读取改写计划与工作区观察值。实现必须保持 Deny 优先且不得放宽既有策略。
type Authorizer interface {
	// Decide 对模型提议的固定工具调用作出授权判定。
	Decide(toolName string, raw json.RawMessage) (AuthorizationDecision, error)
	// DecideVerificationWithObservation 为冻结验证命令绑定完整 verifier 观察值，
	// 生成与模型命令隔离的独立判定。
	DecideVerificationWithObservation(raw json.RawMessage, planRef string, commandIndex int, configFingerprint, observation string) (AuthorizationDecision, error)
	// PlanPatch 返回确定性单文件改写计划；仅对 apply_patch 有意义。
	PlanPatch(raw json.RawMessage) (PatchPlan, error)
	// WorkspaceObservation 返回工具视图的稳定摘要，用于绑定审批。
	WorkspaceObservation() (string, error)
}
