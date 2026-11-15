// [INPUT]: 本地 MCP 提议来源、工作区观察与执行模式。
// [OUTPUT]: 提供只读 OperationPolicy 规则及原审批指纹。
// [POS]: MCP 操作授权能力，由 App 注入基础 Gate。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件将 MCP 本地授权投影转换为既有审批判定，不执行工具或发起网络请求。
// Author: fxbin
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/contract"
)

// ProposalSource 只读取本地 MCP 授权投影；未认领与无效投影必须明确区分。
type ProposalSource interface {
	// Proposal 返回工具本地投影；读取不得建立连接、刷新目录或执行工具。
	Proposal(string, json.RawMessage) (contract.MCPProposal, bool, error)
}

// WorkspaceObservationSource 提供与基础 Policy 相同的工作区观察，不授予权限。
type WorkspaceObservationSource interface {
	// WorkspaceObservation 返回 stdio 审批绑定的工作区观察值。
	WorkspaceObservation() (string, error)
}

// OperationPolicy 接纳明确装配的 MCP 规则；Gate 必须在其后统一应用 hooks 收紧。
// 规则只负责已认领的调用，未认领结果应交回基础 Gate 处理。
type OperationPolicy struct {
	source    ProposalSource
	workspace WorkspaceObservationSource
	execMode  contract.AuthorizationEffect
}

// operationApprovalValue 保留旧 MCP 审批数据的字段顺序和缺省观察值编码。
type operationApprovalValue struct {
	Proposal             contract.MCPProposal `json:"proposal"`
	WorkspaceObservation string               `json:"workspace_observation,omitempty"`
}

// operationApprovalEnvelope 保留旧 fingerprint 的 Name/Value JSON 外包装。
type operationApprovalEnvelope struct {
	Name  string                 `json:"name"`
	Value operationApprovalValue `json:"value"`
}

// invalidOperationEnvelope 将无效调用名及原参数绑定到旧拒绝指纹。
type invalidOperationEnvelope struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
}

// NewOperationPolicy 显式绑定来源与宿主执行策略；空执行模式使用安全默认 ask。
func NewOperationPolicy(source ProposalSource, workspace WorkspaceObservationSource, execMode string) (*OperationPolicy, error) {
	if source == nil {
		return nil, errors.New("MCP operation policy requires a proposal source")
	}
	if workspace == nil {
		return nil, errors.New("MCP operation policy requires a workspace observation source")
	}
	mode, err := operationPolicyEffect(execMode)
	if err != nil {
		return nil, err
	}
	return &OperationPolicy{source: source, workspace: workspace, execMode: mode}, nil
}

// DecideTool 迁移既有 MCP 规则：错误投影拒绝、stdio 叠加宿主策略，并绑定原审批身份。
// 未认领调用不生成未知工具拒绝；该回退和后续 hooks 仍属于基础 Gate。
func (policy *OperationPolicy) DecideTool(name string, raw json.RawMessage) (contract.AuthorizationDecision, bool, error) {
	if policy == nil || policy.source == nil || policy.workspace == nil {
		return contract.AuthorizationDecision{}, false, errors.New("MCP operation policy requires configured sources")
	}
	proposal, found, err := policy.source.Proposal(name, raw)
	if err != nil {
		return contract.AuthorizationDecision{Effect: contract.AuthorizationDeny, RuleID: "invalid_mcp_call", Fingerprint: invalidOperationFingerprint(name, raw), Summary: "invalid or unavailable MCP tool"}, true, nil
	}
	if !found {
		return contract.AuthorizationDecision{}, false, nil
	}
	effect, err := operationPolicyEffect(proposal.Effect)
	if err != nil {
		return contract.AuthorizationDecision{}, true, err
	}
	observation := ""
	if proposal.Stdio {
		if policy.execMode == contract.AuthorizationDeny {
			effect = contract.AuthorizationDeny
		} else if policy.execMode == contract.AuthorizationAsk && effect == contract.AuthorizationAllow {
			effect = contract.AuthorizationAsk
		}
		observation, err = policy.workspace.WorkspaceObservation()
		if err != nil {
			return contract.AuthorizationDecision{}, true, errors.New("observe workspace for MCP approval failed")
		}
	}
	return contract.AuthorizationDecision{MCPIdentity: proposal.EnvironmentDigest, Effect: effect, RuleID: "mcp_tool", Summary: proposal.Summary, WorkspaceObservation: observation, Fingerprint: operationApprovalFingerprint(proposal, observation)}, true, nil
}

// operationPolicyEffect 保留原模式解析词表与错误；未显式指定时必须要求审批。
func operationPolicyEffect(value string) (contract.AuthorizationEffect, error) {
	if value == "" {
		return contract.AuthorizationAsk, nil
	}
	effect := contract.AuthorizationEffect(value)
	if effect != contract.AuthorizationAllow && effect != contract.AuthorizationAsk && effect != contract.AuthorizationDeny {
		return "", fmt.Errorf("invalid policy effect %q", value)
	}
	return effect, nil
}

// operationApprovalFingerprint 原样序列化审批包装；不改为规范化 JSON 或加入宿主模式。
// 参数来自已校验的本地投影，序列化失败沿用旧 fingerprint 的失败行为。
func operationApprovalFingerprint(proposal contract.MCPProposal, observation string) string {
	encoded, err := json.Marshal(operationApprovalEnvelope{Name: "mcp_call", Value: operationApprovalValue{Proposal: proposal, WorkspaceObservation: observation}})
	if err != nil {
		panic(err)
	}
	return operationPolicyDigest(encoded)
}

// invalidOperationFingerprint 保留无效调用的名称与参数包装，拒绝错误不泄露来源详情。
func invalidOperationFingerprint(name string, raw json.RawMessage) string {
	encoded, err := json.Marshal(invalidOperationEnvelope{Name: name, Value: raw})
	if err != nil {
		panic(err)
	}
	return operationPolicyDigest(encoded)
}

// operationPolicyDigest 对旧审批序列化字节计算同一 SHA-256 引用。
func operationPolicyDigest(encoded []byte) string {
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}
