package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// requiredScopeRegistry 用通用元数据要求同一授权实例提供完整请求作用域。
type requiredScopeRegistry struct{ ToolRegistry }

func (*requiredScopeRegistry) RequiresRequestScope() bool { return true }

// scopeAuthorizerProbe 保留捕获规格的原切片，核对 Provider 收到同一冻结投影。
type scopeAuthorizerProbe struct {
	Authorizer
	resets       int
	captured     [][]ports.ToolSpec
	resetError   error
	captureError error
}

func (scope *scopeAuthorizerProbe) ResetRequestScope() error {
	scope.resets++
	scope.captured = nil
	return scope.resetError
}

func (scope *scopeAuthorizerProbe) CaptureRequest(specs []ports.ToolSpec) error {
	scope.captured = append(scope.captured, specs)
	return scope.captureError
}

// captureOnlyAuthorizer 与 resetOnlyAuthorizer 模拟不完整的作用域装配。
type captureOnlyAuthorizer struct{ Authorizer }

func (*captureOnlyAuthorizer) CaptureRequest([]ports.ToolSpec) error { return nil }

type resetOnlyAuthorizer struct{ Authorizer }

func (*resetOnlyAuthorizer) ResetRequestScope() error { return nil }

// TestRequestScopeRequiresCompleteAuthorizer 缺失或部分作用域不得先请求模型或执行工具。
func TestRequestScopeRequiresCompleteAuthorizer(t *testing.T) {
	for _, variant := range []string{"missing", "capture-only", "reset-only"} {
		t.Run(variant, func(t *testing.T) {
			loop, _, registry, input := newResultObserverLoop(t, false)
			loop.Tools = &requiredScopeRegistry{ToolRegistry: loop.Tools}
			switch variant {
			case "capture-only":
				loop.Gate = &captureOnlyAuthorizer{Authorizer: loop.Gate}
			case "reset-only":
				loop.Gate = &resetOnlyAuthorizer{Authorizer: loop.Gate}
			}
			if _, err := loop.Run(context.Background(), input); err == nil {
				t.Fatal("incomplete scope authorizer ran")
			}
			if registry.calls != 0 || len(loop.Provider.(*scriptedProvider).requests) != 0 {
				t.Fatal("model or tool ran before scope wiring was validated")
			}
			events, err := loop.Journal.Load(context.Background(), input.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if countEvent(events, domain.EventOperationStarted) != 0 || countEvent(events, domain.EventRunCompleted) != 0 {
				t.Fatal("missing request scope produced an operation or completion")
			}
		})
	}
}

// TestRequestScopeUsesActualProviderToolProjection 实时授权与 Provider 必须消费同一份规格切片。
func TestRequestScopeUsesActualProviderToolProjection(t *testing.T) {
	loop, _, _, input := newResultObserverLoop(t, false)
	scope := &scopeAuthorizerProbe{Authorizer: loop.Gate}
	loop.Gate, loop.Tools = scope, &requiredScopeRegistry{ToolRegistry: loop.Tools}
	if _, err := loop.Run(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	requests := loop.Provider.(*scriptedProvider).requests
	if scope.resets != 1 || len(scope.captured) != 2 || len(requests) != 2 {
		t.Fatal("request scope was not reset and captured once per request")
	}
	for index, request := range requests {
		captured := scope.captured[index]
		if len(captured) == 0 || len(captured) != len(request.Tools) || &captured[0] != &request.Tools[0] {
			t.Fatal("scope used a different tool projection from Provider")
		}
	}
}

// TestRequestScopeErrorsStopBeforeModelAndExecution 重置及实时/历史捕获错误不能被忽略或造成假完成。
func TestRequestScopeErrorsStopBeforeModelAndExecution(t *testing.T) {
	for _, stage := range []string{"reset", "capture", "replay-capture"} {
		t.Run(stage, func(t *testing.T) {
			loop, _, registry, input := newResultObserverLoop(t, false)
			sentinel := errors.New("request scope unavailable")
			scope := &scopeAuthorizerProbe{Authorizer: loop.Gate}
			if stage == "reset" {
				scope.resetError = sentinel
			} else {
				scope.captureError = sentinel
			}
			loop.Gate, loop.Tools = scope, &requiredScopeRegistry{ToolRegistry: loop.Tools}
			var err error
			if stage == "replay-capture" {
				err = loop.restoreResultProjections([]domain.Event{{Type: domain.EventModelRequestStarted}})
			} else {
				_, err = loop.Run(context.Background(), input)
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("scope failure lost: %v", err)
			}
			if registry.calls != 0 || len(loop.Provider.(*scriptedProvider).requests) != 0 {
				t.Fatal("model or tool ran after request scope error")
			}
			events, loadErr := loop.Journal.Load(context.Background(), input.RunID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if countEvent(events, domain.EventRunCompleted) != 0 {
				t.Fatal("request scope failure produced false completion")
			}
		})
	}
}
