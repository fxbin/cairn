// Package agent 在模型请求及历史回放安全点冻结宿主授权作用域。
// Author: fxbin
package agent

import (
	"errors"

	"github.com/fxbin/cairn/internal/ports"
)

// RequestScope 由同一授权端口实例实现；快照只用于收紧判定，不改变执行事实。
// 实时请求使用实际发送的同一份规格，历史请求按原事件顺序重建；失败阻止继续调用模型。
type RequestScope interface {
	ResetRequestScope() error
	CaptureRequest([]ports.ToolSpec) error
}

// requestScopeRequirement 由工具提供器声明其安全判定需要请求作用域，防止能力装配只接执行而遗漏约束。
type requestScopeRequirement interface {
	RequiresRequestScope() bool
}

// requestScope 核对授权实例是否具备工具提供器要求的完整作用域合同。
// 普通基础工具和无此要求的授权组合保持空行为；部分接线不能静默降级。
func (loop *Loop) requestScope() (RequestScope, error) {
	scope, _ := loop.Gate.(RequestScope)
	if requirement, ok := loop.Tools.(requestScopeRequirement); ok && requirement.RequiresRequestScope() && scope == nil {
		return nil, errors.New("tool registry requires a request-scoped authorizer")
	}
	return scope, nil
}

// resetRequestScope 在恢复能力投影前清除上次请求的派生授权条件。
func (loop *Loop) resetRequestScope() error {
	scope, err := loop.requestScope()
	if err != nil {
		return err
	}
	if scope != nil {
		return scope.ResetRequestScope()
	}
	return nil
}

// captureRequestScope 冻结模型可见的工具规格，未装配可选作用域时不增加约束。
func (loop *Loop) captureRequestScope(specs []ports.ToolSpec) error {
	scope, err := loop.requestScope()
	if err != nil {
		return err
	}
	if scope != nil {
		return scope.CaptureRequest(specs)
	}
	return nil
}
