// 运行展示词表：把同一运行事实在 CLI、TUI 与桌面桥接上的说法收敛到一处。
// 词表与事件 schema、payloadSchemas 和哈希链解耦：它只读取已回放出的阶段、
// 可信判定与操作标识，不新增也不改写任何事件字段。
// Author: fxbin
package contract

// RunPhase 是运行阶段的封闭枚举。取值与 domain 投影对外暴露的阶段词表一一
// 对应：created/running/cancelling/completed/failed/cancelled 来自运行状态，
// waiting_approval 与 waiting_reconcile 来自待决审批与悬而未决副作用。
type RunPhase string

const (
	// RunPhaseCreated 是运行已创建但尚未开始的阶段。
	RunPhaseCreated RunPhase = "created"
	// RunPhaseRunning 是模型循环正在执行的阶段。
	RunPhaseRunning RunPhase = "running"
	// RunPhaseCancelling 是已请求终止但仍有未关闭轮次或待决操作的阶段。
	RunPhaseCancelling RunPhase = "cancelling"
	// RunPhaseCompleted 是运行正常收束的终态阶段。
	RunPhaseCompleted RunPhase = "completed"
	// RunPhaseFailed 是运行以失败收束的终态阶段。
	RunPhaseFailed RunPhase = "failed"
	// RunPhaseCancelled 是运行被取消收束的终态阶段。
	RunPhaseCancelled RunPhase = "cancelled"
	// RunPhaseWaitingApproval 是等待人工一次性审批决定的阶段。
	RunPhaseWaitingApproval RunPhase = "waiting_approval"
	// RunPhaseWaitingReconcile 是等待人工对账悬而未决副作用的阶段。
	RunPhaseWaitingReconcile RunPhase = "waiting_reconcile"
)

// UnknownDisplayLabel 是词表未覆盖取值的回落展示词。界面遇到未知取值时必须
// 显示它，而不是猜测运行语义。
const UnknownDisplayLabel = "未知"

// RunPhaseLabel 返回运行阶段的展示词。词表未覆盖的取值回落到
// UnknownDisplayLabel；同一阶段在任何界面上只有这一个展示词。
func RunPhaseLabel(phase string) string {
	switch RunPhase(phase) {
	case RunPhaseCreated:
		return "已创建"
	case RunPhaseRunning:
		return "执行中"
	case RunPhaseCancelling:
		return "收尾取消中"
	case RunPhaseCompleted:
		return "运行完成"
	case RunPhaseFailed:
		return "运行失败"
	case RunPhaseCancelled:
		return "已取消"
	case RunPhaseWaitingApproval:
		return "待审批"
	case RunPhaseWaitingReconcile:
		return "待对账"
	default:
		return UnknownDisplayLabel
	}
}

// CompletionTrust 是完成证据可信等级的封闭枚举。unknown 表示本次运行没有
// 任何验证事实，不能据此宣称已验证。
type CompletionTrust string

const (
	// CompletionTrustUnknown 表示没有验证事实，完成证据等级未知。
	CompletionTrustUnknown CompletionTrust = "unknown"
	// CompletionTrustUntrusted 表示存在验证事实但未覆盖全部变更。
	CompletionTrustUntrusted CompletionTrust = "untrusted"
	// CompletionTrustTrusted 表示验证成功且覆盖全部变更。
	CompletionTrustTrusted CompletionTrust = "trusted"
)

// CompletionTrustLabel 返回完成证据可信等级的展示词。词表未覆盖的等级回落
// 到 UnknownDisplayLabel。
func CompletionTrustLabel(trust CompletionTrust) string {
	switch trust {
	case CompletionTrustTrusted:
		return "可信"
	case CompletionTrustUntrusted:
		return "非可信"
	case CompletionTrustUnknown:
		return "未验证"
	default:
		return UnknownDisplayLabel
	}
}

// ClassifyCompletionTrust 由可信判定与验证事实推导可信等级：可信判定成立
// 即 trusted；没有验证事件即 unknown；其余（有验证但未绑定全部变更）为
// untrusted。调用方不得凭模型自述抬高等级。
func ClassifyCompletionTrust(trusted bool, verificationSeq uint64) CompletionTrust {
	switch {
	case trusted:
		return CompletionTrustTrusted
	case verificationSeq == 0:
		return CompletionTrustUnknown
	default:
		return CompletionTrustUntrusted
	}
}

// CompletionTrustOf 由完成证据投影推导可信等级；它与
// ClassifyCompletionTrust 共用同一判定公式。
func CompletionTrustOf(evidence CompletionEvidence) CompletionTrust {
	return ClassifyCompletionTrust(evidence.Trusted, evidence.VerificationSeq)
}

// ReconcileActionHint 是等待对账时各界面共用的动作指引。它只描述人工必须
// 执行的动作，不代替授权：悬而未决副作用必须经人工对账收束，禁止盲目重试。
const ReconcileActionHint = "cairn resume <run-id> --resolve <operation-id>=<verdict> --note <text> --evidence <ref>"
