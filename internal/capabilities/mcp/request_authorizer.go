// 本文件把 MCP 模型请求声明约束装饰到既有 Policy，不执行工具或连接外部服务。
// Author: fxbin
package mcp

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// PolicySource 仅提供既有授权、冻结验证和工作区观察合同，不暴露执行能力。
type PolicySource interface {
	// Decide 返回基础 Policy 对工具提议的授权，不在此执行工具。
	Decide(string, json.RawMessage) (contract.AuthorizationDecision, error)
	// DecideVerificationWithObservation 绑定冻结计划及验证观察值，保留独立审批。
	DecideVerificationWithObservation(json.RawMessage, string, int, string, string) (contract.AuthorizationDecision, error)
	// PlanPatch 返回既有工作区边界下的确定性改写计划。
	PlanPatch(json.RawMessage) (contract.PatchPlan, error)
	// WorkspaceObservation 返回同一工作区的审批观察值。
	WorkspaceObservation() (string, error)
}

// DeclarationSource 只读取当前可见 MCP 工具声明的指纹，不发现或连接服务器。
type DeclarationSource interface {
	// MCPDeclarationFingerprint 返回指定工具当前可见声明的摘要及是否存在。
	MCPDeclarationFingerprint(string) (string, bool)
}

// RequestAuthorizer 在底层 Policy 之后收紧请求声明；指纹和执行身份始终来自 Policy。
// 调用方串行重置、捕获和判定；nil 声明快照保留无模型请求的脚本计划合同。
type RequestAuthorizer struct {
	policy              PolicySource
	declarations        DeclarationSource
	requestDeclarations map[string]string
}

// NewRequestAuthorizer 显式绑定 Policy 与声明来源，缺少任一依赖即失败关闭。
func NewRequestAuthorizer(policy PolicySource, declarations DeclarationSource) (*RequestAuthorizer, error) {
	authorizer := &RequestAuthorizer{policy: policy, declarations: declarations}
	if err := authorizer.validateSources(); err != nil {
		return nil, err
	}
	return authorizer, nil
}

// validateSources 保证零值或缺失装配不能绕过授权及声明边界。
func (authorizer *RequestAuthorizer) validateSources() error {
	if authorizer == nil || authorizer.policy == nil {
		return errors.New("MCP request authorizer requires a policy source")
	}
	if authorizer.declarations == nil {
		return errors.New("MCP request authorizer requires a declaration source")
	}
	return nil
}

// ResetRequestScope 清除先前请求快照；nil 表示尚无模型请求，不替脚本计划追加声明约束。
func (authorizer *RequestAuthorizer) ResetRequestScope() error {
	if err := authorizer.validateSources(); err != nil {
		return err
	}
	authorizer.requestDeclarations = nil
	return nil
}

// CaptureRequest 冻结同一份模型可见规格中的 MCP 声明指纹，只影响当前请求。
// 空规格仍表示确有一次模型请求，不能等同于未发生请求的 nil 快照。
func (authorizer *RequestAuthorizer) CaptureRequest(specs []ports.ToolSpec) error {
	if err := authorizer.validateSources(); err != nil {
		return err
	}
	authorizer.requestDeclarations = make(map[string]string)
	for _, spec := range specs {
		if strings.HasPrefix(spec.Name, "mcp__") {
			digest, found := authorizer.declarations.MCPDeclarationFingerprint(spec.Name)
			if found {
				authorizer.requestDeclarations[spec.Name] = digest
			}
		}
	}
	return nil
}

// Decide 先取得原 Policy 判定，再对未声明或已漂移的 MCP 工具施加拒绝。
// 约束只改拒绝分类与说明，不授予权限、不改审批指纹或执行身份。
func (authorizer *RequestAuthorizer) Decide(name string, raw json.RawMessage) (contract.AuthorizationDecision, error) {
	if err := authorizer.validateSources(); err != nil {
		return contract.AuthorizationDecision{}, err
	}
	decision, err := authorizer.policy.Decide(name, raw)
	if err != nil {
		return decision, err
	}
	if !strings.HasPrefix(name, "mcp__") || authorizer.requestDeclarations == nil {
		return decision, nil
	}
	expected, declared := authorizer.requestDeclarations[name]
	matched := false
	if declared {
		digest, found := authorizer.declarations.MCPDeclarationFingerprint(name)
		matched = found && digest == expected
	}
	if !matched {
		decision.Effect = contract.AuthorizationDeny
		decision.RuleID = "mcp_declaration_changed"
		decision.FailureCode = "mcp_not_declared"
		decision.Summary = "MCP tool was not declared with this schema in the model request; discover or search it, then call it in a subsequent request"
	}
	return decision, nil
}

// DecideVerificationWithObservation 原样委托冻结验证的独立授权，不套用模型声明约束。
func (authorizer *RequestAuthorizer) DecideVerificationWithObservation(raw json.RawMessage, planRef string, commandIndex int, configFingerprint, observation string) (contract.AuthorizationDecision, error) {
	if err := authorizer.validateSources(); err != nil {
		return contract.AuthorizationDecision{}, err
	}
	return authorizer.policy.DecideVerificationWithObservation(raw, planRef, commandIndex, configFingerprint, observation)
}

// PlanPatch 原样委托既有确定性改写规划，不创建第二条工作区规划路径。
func (authorizer *RequestAuthorizer) PlanPatch(raw json.RawMessage) (contract.PatchPlan, error) {
	if err := authorizer.validateSources(); err != nil {
		return contract.PatchPlan{}, err
	}
	return authorizer.policy.PlanPatch(raw)
}

// WorkspaceObservation 原样委托同一 Policy 的工作区观察，保持审批绑定一致。
func (authorizer *RequestAuthorizer) WorkspaceObservation() (string, error) {
	if err := authorizer.validateSources(); err != nil {
		return "", err
	}
	return authorizer.policy.WorkspaceObservation()
}
