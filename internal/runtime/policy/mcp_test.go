// [INPUT]: 真实 Gate、共享提议合同与原审批夹具。
// [OUTPUT]: 验证宿主执行限制及身份漂移仍使审批失效。
// [POS]: Policy 与 MCP 的接线回归，不弱化原安全断言。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package policy

import (
	"encoding/json"
	"testing"

	mcpcapability "github.com/fxbin/cairn/internal/capabilities/mcp"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// TestMCPPolicyCannotBypassHostExecutionPolicy 服务器允许模式不能覆盖本机执行禁令或审批。
func TestMCPPolicyCannotBypassHostExecutionPolicy(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		global, local string
		want          Effect
	}{{"deny", "allow", Deny}, {"ask", "allow", Ask}, {"allow", "ask", Ask}, {"allow", "deny", Deny}} {
		gate, err := New("ask", test.global, nil, tools.ApplyPatch{Workspace: workspace})
		if err != nil {
			t.Fatal(err)
		}
		decision, err := decideMCPTest(t, gate, contract.MCPProposal{Stdio: true, Effect: test.local, Arguments: json.RawMessage(`{}`)})
		if err != nil || decision.Effect != test.want {
			t.Fatalf("global=%s local=%s decision=%#v error=%v", test.global, test.local, decision, err)
		}
	}
}

// TestMCPApprovalBindsIdentityAndArguments 身份、目录、实际环境和输入漂移都要求重新审批。
func TestMCPApprovalBindsIdentityAndArguments(t *testing.T) {
	gate := Gate{}
	base := contract.MCPProposal{Effect: "ask", Server: "docs", Identity: "server", EnvironmentDigest: "environment", CatalogDigest: "catalog", ToolDigest: "schema", ToolName: "mcp__docs__lookup", Arguments: json.RawMessage(`{"id":1}`)}
	original, err := decideMCPTest(t, &gate, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*contract.MCPProposal){func(p *contract.MCPProposal) { p.Identity = "other" }, func(p *contract.MCPProposal) { p.EnvironmentDigest = "other" }, func(p *contract.MCPProposal) { p.CatalogDigest = "other" }, func(p *contract.MCPProposal) { p.ToolDigest = "other" }, func(p *contract.MCPProposal) { p.Arguments = json.RawMessage(`{"id":2}`) }} {
		proposal := base
		change(&proposal)
		decision, err := decideMCPTest(t, &gate, proposal)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Fingerprint == original.Fingerprint {
			t.Fatal("approval survived a changed MCP operation")
		}
	}
}

// policyMCPSource 为原审批断言提供能力侧的本地提议，不建立连接。
type policyMCPSource struct{ proposal contract.MCPProposal }

func (source policyMCPSource) Proposal(string, json.RawMessage) (contract.MCPProposal, bool, error) {
	return source.proposal, true, nil
}

// decideMCPTest 仅适配规则接线；原安全断言保持不变。
func decideMCPTest(t *testing.T, gate *Gate, proposal contract.MCPProposal) (Decision, error) {
	t.Helper()
	rule, err := mcpcapability.NewOperationPolicy(policyMCPSource{proposal: proposal}, gate, string(gate.ExecMode))
	if err != nil {
		return Decision{}, err
	}
	gate.ToolRules = []ToolRule{rule}
	return gate.Decide("mcp_test_call", proposal.Arguments)
}
