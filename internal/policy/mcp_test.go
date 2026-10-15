package policy

import (
	"encoding/json"
	"testing"

	"github.com/fxbin/cairn/internal/tools"
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
		decision, err := gate.decideMCP(tools.MCPProposal{Stdio: true, Effect: test.local, Arguments: json.RawMessage(`{}`)})
		if err != nil || decision.Effect != test.want {
			t.Fatalf("global=%s local=%s decision=%#v error=%v", test.global, test.local, decision, err)
		}
	}
}

// TestMCPApprovalBindsIdentityAndArguments 身份、目录、实际环境和输入漂移都要求重新审批。
func TestMCPApprovalBindsIdentityAndArguments(t *testing.T) {
	gate := Gate{}
	base := tools.MCPProposal{Effect: "ask", Server: "docs", Identity: "server", EnvironmentDigest: "environment", CatalogDigest: "catalog", ToolDigest: "schema", ToolName: "mcp__docs__lookup", Arguments: json.RawMessage(`{"id":1}`)}
	original, err := gate.decideMCP(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*tools.MCPProposal){func(p *tools.MCPProposal) { p.Identity = "other" }, func(p *tools.MCPProposal) { p.EnvironmentDigest = "other" }, func(p *tools.MCPProposal) { p.CatalogDigest = "other" }, func(p *tools.MCPProposal) { p.ToolDigest = "other" }, func(p *tools.MCPProposal) { p.Arguments = json.RawMessage(`{"id":2}`) }} {
		proposal := base
		change(&proposal)
		decision, err := gate.decideMCP(proposal)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Fingerprint == original.Fingerprint {
			t.Fatal("approval survived a changed MCP operation")
		}
	}
}
