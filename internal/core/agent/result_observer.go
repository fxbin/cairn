// Package agent 按持久化事实调度显式装配的能力投影，不执行扩展回放中的外部动作。
// Author: fxbin
package agent

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// ReplayContributor 以固定事件顺序恢复能力投影；初始化和逐事件接纳都不得执行工具或连接外部服务。
// BeginReplay 清除上次派生状态，ReplayEvent 只读取历史事实；任一步失败立即停止恢复。
type ReplayContributor interface {
	BeginReplay([]domain.Event) error
	ReplayEvent(domain.Event) error
}

// ToolResultObserver 接纳已持久化的工具结果，且必须具备对应的历史恢复能力。
// HandlesToolResult 在一次运行中保持确定性；同一工具最多属于一个观察器。
type ToolResultObserver interface {
	ReplayContributor
	HandlesToolResult(string) bool
	ObserveToolResult(string, contract.ToolResult) error
}

// resultObserver 校验唯一归属和安全元数据；缺少状态工具的投影不能降级为基础工具。
func (loop *Loop) resultObserver(name string) (ToolResultObserver, error) {
	var selected ToolResultObserver
	for index, observer := range loop.ResultObservers {
		if observer == nil {
			return nil, fmt.Errorf("tool result observer %d is nil", index)
		}
		if !observer.HandlesToolResult(name) {
			continue
		}
		if selected != nil {
			return nil, errors.New("tool result has multiple projection owners")
		}
		selected = observer
	}
	if loop.Tools == nil {
		return nil, errors.New("tool registry is required for result projection")
	}
	if selected == nil && loop.Tools.RequiresResultObserver(name) {
		return nil, errors.New("tool result requires an explicitly configured projection observer")
	}
	return selected, nil
}

// validateResultObservers 在工具启动前核对接线，避免外部副作用执行后才发现无法投影。
func (loop *Loop) validateResultObservers(name string) error {
	_, err := loop.resultObserver(name)
	return err
}

// observeToolResult 只在操作终态和工具结果均已持久化之后通知唯一观察器。
func (loop *Loop) observeToolResult(name string, result contract.ToolResult) error {
	if result.OK {
		if err := loop.validatePersistedResultObserver(name); err != nil {
			return err
		}
	}
	observer, err := loop.resultObserver(name)
	if err != nil {
		return err
	}
	if observer != nil {
		return observer.ObserveToolResult(name, result)
	}
	return nil
}

// restoreResultProjections 按装配顺序清空并逐事件回放；未知成功工具同样必须证明投影归属。
// 模型声明仍在对应历史安全点冻结，不能使用最终目录反推更早的模型请求。
func (loop *Loop) restoreResultProjections(events []domain.Event) error {
	loop.mcpModelDeclarations = nil
	for index, observer := range loop.ResultObservers {
		if observer == nil {
			return fmt.Errorf("tool result observer %d is nil", index)
		}
		if err := observer.BeginReplay(events); err != nil {
			return err
		}
	}
	for _, spec := range loop.Tools.Specs() {
		if err := loop.validateResultObservers(spec.Name); err != nil {
			return err
		}
	}
	proposals := make(map[domain.OperationID]string)
	for _, event := range events {
		if event.OperationID != nil {
			switch event.Type {
			case domain.EventToolCallProposed:
				var proposed toolCallPayload
				if err := json.Unmarshal(event.Payload, &proposed); err != nil {
					return errors.New("decode persisted tool proposal for result projection failed")
				}
				proposals[*event.OperationID] = proposed.ToolName
			case domain.EventVerificationCommandProposed:
				proposals[*event.OperationID] = "exec_command"
			case domain.EventToolResultAppended:
				var result contract.ToolResult
				if err := json.Unmarshal(event.Payload, &result); err != nil {
					return errors.New("decode persisted tool result for projection failed")
				}
				if result.OK {
					if err := loop.validatePersistedResultObserver(proposals[*event.OperationID]); err != nil {
						return err
					}
				}
			}
		}
		for _, observer := range loop.ResultObservers {
			if err := observer.ReplayEvent(event); err != nil {
				return err
			}
		}
		if event.Type == domain.EventModelRequestStarted {
			loop.captureMCPDeclarations(loop.Tools.Specs())
		}
	}
	return nil
}

// validatePersistedResultObserver 拒绝既无已知工具声明也无投影归属的历史成功，普通未知失败仍可回放。
func (loop *Loop) validatePersistedResultObserver(name string) error {
	observer, err := loop.resultObserver(name)
	if err != nil {
		return err
	}
	if observer != nil {
		return nil
	}
	for _, spec := range loop.Tools.Specs() {
		if spec.Name == name {
			return nil
		}
	}
	return errors.New("unknown successful tool result requires a projection observer")
}
