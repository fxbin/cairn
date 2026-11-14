// Package skills 从已持久化的成功事实恢复技能历史，不读取或执行技能源。
// Author: fxbin
package skills

import (
	"encoding/json"
	"errors"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// HistorySource 仅接纳技能任务条件和成功落盘后的结果，不具备执行或资源读取能力。
// PrepareSkills 必须清空此前激活投影；AcceptSkillResult 保留历史来源和完整正文。
type HistorySource interface {
	PrepareSkills(string)
	AcceptSkillResult(string, contract.ToolResult) error
}

// History 在串行回放中关联提议、操作成功回执与工具结果；事实源仍是 Journal。
// 临时映射仅用于核验，不能替代已持久化事件或授予技能权限。
type History struct {
	source    HistorySource
	proposals map[domain.OperationID]string
	succeeded map[domain.OperationID]successReceipt
}

// toolProposal 保留原提议载荷的解码类型，避免迁移后容忍原先拒绝的字段类型。
// 只有工具名称参与关联；调用标识与参数不用于执行或重新授权。
type toolProposal struct {
	CallID    string          `json:"call_id"`
	ToolName  string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
}

// successReceipt 保留具名操作的完整工具结果，失败终态不能激活技能。
type successReceipt struct {
	ToolName string              `json:"tool_name"`
	Result   contract.ToolResult `json:"result"`
}

// NewHistory 绑定显式提供的技能历史接纳端口，不发现或执行任何技能。
func NewHistory(source HistorySource) *History {
	return &History{source: source}
}

// HandlesToolResult 只声明接纳固定 skill 工具，不扩大可执行工具集。
func (history *History) HandlesToolResult(name string) bool {
	return name == "skill"
}

// ObserveToolResult 仅供调用方在结果持久化成功后更新技能投影；其他工具为空操作。
// 缺少来源或来源拒绝结果时失败关闭，不重试、重读或补造激活证据。
func (history *History) ObserveToolResult(name string, result contract.ToolResult) error {
	if !history.HandlesToolResult(name) {
		return nil
	}
	if history == nil || history.source == nil {
		return errors.New("skill history requires a result source")
	}
	return history.source.AcceptSkillResult(name, result)
}

// BeginReplay 从创建事件提取历史任务并重置源及临时投影，不读取当前技能目录。
// 重复调用会重新开始回放，旧操作回执不得跨回放激活技能；畸形创建事件失败关闭。
func (history *History) BeginReplay(events []domain.Event) error {
	if history == nil || history.source == nil {
		return errors.New("skill history requires a result source")
	}
	task := ""
	for _, event := range events {
		if event.Type != domain.EventRunCreated {
			continue
		}
		var created domain.RunCreatedPayload
		if json.Unmarshal(event.Payload, &created) != nil {
			return errors.New("decode skill task context failed")
		}
		task = created.Task
		break
	}
	history.source.PrepareSkills(task)
	history.proposals = make(map[domain.OperationID]string)
	history.succeeded = make(map[domain.OperationID]successReceipt)
	return nil
}

// ReplayEvent 按 Journal 顺序关联操作；成功工具结果必须有同一操作的成功回执，
// 且完整结果的历史 canonical 指纹逐位一致。调用前须完成 BeginReplay；
// 不相关事件或没有操作标识的事件为空操作，失败结果不能激活技能。
func (history *History) ReplayEvent(event domain.Event) error {
	if event.OperationID == nil {
		return nil
	}
	switch event.Type {
	case domain.EventToolCallProposed, domain.EventOperationSucceeded, domain.EventToolResultAppended:
	default:
		return nil
	}
	if history == nil || history.source == nil {
		return errors.New("skill history requires a result source")
	}
	if history.proposals == nil || history.succeeded == nil {
		return errors.New("skill history replay must begin before events")
	}
	operationID := *event.OperationID
	switch event.Type {
	case domain.EventToolCallProposed:
		var proposal toolProposal
		if json.Unmarshal(event.Payload, &proposal) == nil {
			history.proposals[operationID] = proposal.ToolName
		}
	case domain.EventOperationSucceeded:
		var receipt successReceipt
		if json.Unmarshal(event.Payload, &receipt) == nil {
			history.succeeded[operationID] = receipt
		}
	case domain.EventToolResultAppended:
		if history.proposals[operationID] != "skill" {
			return nil
		}
		var result contract.ToolResult
		if json.Unmarshal(event.Payload, &result) != nil {
			return errors.New("decode persisted skill result failed")
		}
		if !result.OK || result.Code != "ok" {
			return nil
		}
		receipt, found := history.succeeded[operationID]
		if !found || receipt.ToolName != "skill" || !receipt.Result.OK || receipt.Result.Code != "ok" {
			return errors.New("skill activation has no successful operation receipt")
		}
		a, err := canonicaljson.Fingerprint("cairn-skill-result-v1", receipt.Result)
		b, otherErr := canonicaljson.Fingerprint("cairn-skill-result-v1", result)
		if err != nil || otherErr != nil || a != b {
			return errors.New("skill activation does not match its operation receipt")
		}
		return history.ObserveToolResult("skill", result)
	}
	return nil
}
