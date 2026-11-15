// [INPUT]: 共享合同、显式授权来源与原审批回归。
// [OUTPUT]: 提供 MCP 授权约束或其最小合同回归。
// [POS]: 能力层保留协议身份，Core 只传递不透明绑定。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
)

type operationSource struct {
	proposal contract.MCPProposal
	found    bool
	err      error
}

func (source *operationSource) Proposal(string, json.RawMessage) (contract.MCPProposal, bool, error) {
	return source.proposal, source.found, source.err
}

type operationWorkspace struct {
	calls int
	err   error
}

func (workspace *operationWorkspace) WorkspaceObservation() (string, error) {
	workspace.calls++
	return "sha256:workspace", workspace.err
}
func operationProposal() contract.MCPProposal {
	return contract.MCPProposal{Server: "docs", Identity: "server", EnvironmentDigest: "environment", CatalogDigest: "catalog", ToolDigest: "schema", ToolName: "mcp__docs__lookup", Arguments: json.RawMessage(`{"id":1}`), Effect: "allow", Summary: "调用 <docs>& lookup"}
}

// TestOperationPolicyPreservesApprovalBytes 固定迁移前黄金指纹，含 HTML 转义和 stdio 观察。
func TestOperationPolicyPreservesApprovalBytes(t *testing.T) {
	for _, tc := range []struct {
		stdio       bool
		mode        string
		effect      contract.AuthorizationEffect
		fingerprint string
	}{
		{false, "allow", contract.AuthorizationAllow, "sha256:ccd3d0229a8c7857f0c8ac2fb4f5eec40d8e03715799021622f8bc8d012ef1c5"},
		{true, "ask", contract.AuthorizationAsk, "sha256:315d7fb005ff4c8de246f9144da0fe2e1a1001272aba7983f33e4815f7ecfa54"},
	} {
		proposal := operationProposal()
		proposal.Stdio = tc.stdio
		source := &operationSource{proposal: proposal, found: true}
		workspace := &operationWorkspace{}
		rule, err := NewOperationPolicy(source, workspace, tc.mode)
		if err != nil {
			t.Fatal(err)
		}
		got, claimed, err := rule.DecideTool(proposal.ToolName, proposal.Arguments)
		if err != nil || !claimed || got.Effect != tc.effect || got.Fingerprint != tc.fingerprint || got.ExecutionBinding != proposal.EnvironmentDigest || got.Summary != proposal.Summary {
			t.Fatalf("decision=%+v claimed=%v err=%v", got, claimed, err)
		}
		if tc.stdio && (got.WorkspaceObservation != "sha256:workspace" || workspace.calls != 1) {
			t.Fatal("stdio observation lost")
		}
		if !tc.stdio && (got.WorkspaceObservation != "" || workspace.calls != 0) {
			t.Fatal("HTTP observed workspace")
		}
	}
}

// TestOperationPolicyRestrictsStdio HTTP 不继承宿主执行模式，stdio 必须叠加执行限制。
func TestOperationPolicyRestrictsStdio(t *testing.T) {
	for _, stdio := range []bool{false, true} {
		for _, host := range []string{"allow", "ask", "deny", ""} {
			for _, local := range []string{"allow", "ask", "deny", ""} {
				proposal := operationProposal()
				proposal.Stdio = stdio
				proposal.Effect = local
				workspace := &operationWorkspace{}
				rule, err := NewOperationPolicy(&operationSource{proposal: proposal, found: true}, workspace, host)
				if err != nil {
					t.Fatal(err)
				}
				got, claimed, err := rule.DecideTool(proposal.ToolName, proposal.Arguments)
				want := contract.AuthorizationEffect(local)
				if local == "" {
					want = contract.AuthorizationAsk
				}
				if stdio {
					switch host {
					case "deny":
						want = contract.AuthorizationDeny
					case "ask", "":
						if want == contract.AuthorizationAllow {
							want = contract.AuthorizationAsk
						}
					}
				}
				if err != nil || !claimed || got.Effect != want {
					t.Fatalf("stdio=%v host=%s local=%s decision=%+v err=%v", stdio, host, local, got, err)
				}
			}
		}
	}
}

// TestOperationPolicyRejectsMissingAndInvalidSources 无效装配或观察阻止授权，来源错误保持脱敏拒绝。
func TestOperationPolicyRejectsMissingAndInvalidSources(t *testing.T) {
	source := &operationSource{proposal: operationProposal(), found: true}
	workspace := &operationWorkspace{}
	for _, makeRule := range []func() (*OperationPolicy, error){
		func() (*OperationPolicy, error) { return NewOperationPolicy(nil, workspace, "ask") },
		func() (*OperationPolicy, error) { return NewOperationPolicy(source, nil, "ask") },
		func() (*OperationPolicy, error) { return NewOperationPolicy(source, workspace, "maybe") },
	} {
		if _, err := makeRule(); err == nil {
			t.Fatal("invalid sources accepted")
		}
	}
	var zero OperationPolicy
	if _, _, err := zero.DecideTool("test", json.RawMessage(`{}`)); err == nil {
		t.Fatal("zero rule accepted")
	}
	rule, err := NewOperationPolicy(source, workspace, "ask")
	if err != nil {
		t.Fatal(err)
	}
	source.err = errors.New("private credential cause")
	got, claimed, err := rule.DecideTool(source.proposal.ToolName, json.RawMessage(`{ "id": 1 }`))
	if err != nil || !claimed || got.Effect != contract.AuthorizationDeny || got.RuleID != "invalid_mcp_call" || strings.Contains(got.Summary, "private") || got.Fingerprint != "sha256:d4d793f95dd84f56f9134d96a7aa6bc2168b96803505ca98fab3c235a7362b5c" {
		t.Fatalf("source rejection=%+v claimed=%v err=%v", got, claimed, err)
	}
	source.err = nil
	source.found = false
	if _, claimed, err := rule.DecideTool("other", json.RawMessage(`{}`)); err != nil || claimed {
		t.Fatal("unclaimed source changed fallback")
	}
	source.found = true
	source.proposal.Effect = "maybe"
	if _, _, err := rule.DecideTool(source.proposal.ToolName, source.proposal.Arguments); err == nil {
		t.Fatal("invalid local effect accepted")
	}
	source.proposal.Effect = "allow"
	source.proposal.Stdio = true
	workspace.err = errors.New("private workspace cause")
	if _, _, err := rule.DecideTool(source.proposal.ToolName, source.proposal.Arguments); err == nil || err.Error() != "observe workspace for MCP approval failed" {
		t.Fatalf("observation failure=%v", err)
	}
}
