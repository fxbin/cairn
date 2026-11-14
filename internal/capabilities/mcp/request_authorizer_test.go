package mcp

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// requestPolicy 记录同一底层 Policy 的输入与输出，不具备执行工具或网络能力。
type requestPolicy struct {
	decision                contract.AuthorizationDecision
	decisionErr             error
	calls                   int
	name                    string
	raw                     json.RawMessage
	verificationDecision    contract.AuthorizationDecision
	verificationErr         error
	verificationCalls       int
	verificationRaw         json.RawMessage
	planRef                 string
	commandIndex            int
	configFingerprint       string
	verificationObservation string
	patch                   contract.PatchPlan
	patchErr                error
	patchCalls              int
	patchRaw                json.RawMessage
	observation             string
	observationErr          error
	observationCalls        int
}

func (policy *requestPolicy) Decide(name string, raw json.RawMessage) (contract.AuthorizationDecision, error) {
	policy.calls++
	policy.name = name
	policy.raw = append(json.RawMessage(nil), raw...)
	return policy.decision, policy.decisionErr
}
func (policy *requestPolicy) DecideVerificationWithObservation(raw json.RawMessage, planRef string, index int, fingerprint, observation string) (contract.AuthorizationDecision, error) {
	policy.verificationCalls++
	policy.verificationRaw = append(json.RawMessage(nil), raw...)
	policy.planRef = planRef
	policy.commandIndex = index
	policy.configFingerprint = fingerprint
	policy.verificationObservation = observation
	return policy.verificationDecision, policy.verificationErr
}
func (policy *requestPolicy) PlanPatch(raw json.RawMessage) (contract.PatchPlan, error) {
	policy.patchCalls++
	policy.patchRaw = append(json.RawMessage(nil), raw...)
	return policy.patch, policy.patchErr
}
func (policy *requestPolicy) WorkspaceObservation() (string, error) {
	policy.observationCalls++
	return policy.observation, policy.observationErr
}

// requestDeclarations 提供可变声明目录，只允许按名称读指纹。
type requestDeclarations struct {
	fingerprints map[string]string
	lookups      int
	beforeLookup func()
}

func (source *requestDeclarations) MCPDeclarationFingerprint(name string) (string, bool) {
	source.lookups++
	if source.beforeLookup != nil {
		source.beforeLookup()
	}
	digest, found := source.fingerprints[name]
	return digest, found
}

var _ PolicySource = (*RequestAuthorizer)(nil)

// newTestRequestAuthorizer 使用完整判定字段验证装饰器不会丢失执行身份或审批证据。
func newTestRequestAuthorizer(t *testing.T) (*RequestAuthorizer, *requestPolicy, *requestDeclarations) {
	t.Helper()
	policy := &requestPolicy{decision: contract.AuthorizationDecision{
		MCPIdentity: "identity-original", Effect: contract.AuthorizationAllow, RuleID: "base-policy", Fingerprint: "sha256:approval-original", Summary: "base summary", WorkspaceObservation: "sha256:workspace", FailureCode: "base-code", FailureData: json.RawMessage(`{"base":true}`),
	}}
	source := &requestDeclarations{fingerprints: make(map[string]string)}
	authorizer, err := NewRequestAuthorizer(policy, source)
	if err != nil {
		t.Fatal(err)
	}
	return authorizer, policy, source
}

// requireRequestDenial 锁定旧拒绝字面，并确保未被声明约束修改的证据全部保留。
func requireRequestDenial(t *testing.T, got, base contract.AuthorizationDecision) {
	t.Helper()
	expected := base
	expected.Effect = contract.AuthorizationDeny
	expected.RuleID = "mcp_declaration_changed"
	expected.FailureCode = "mcp_not_declared"
	expected.Summary = "MCP tool was not declared with this schema in the model request; discover or search it, then call it in a subsequent request"
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("denial changed policy evidence: got=%+v want=%+v", got, expected)
	}
}

// TestRequestAuthorizerSameResponseNextRequestAndSchemaDrift 验证发现只影响后续请求，漂移与失踪均拒绝。
func TestRequestAuthorizerSameResponseNextRequestAndSchemaDrift(t *testing.T) {
	authorizer, policy, source := newTestRequestAuthorizer(t)
	const tool = "mcp__docs__lookup"
	raw := json.RawMessage(`{ "id": 1 }`)
	if err := authorizer.CaptureRequest([]ports.ToolSpec{{Name: "mcp_discover"}, {Name: "tool_search"}}); err != nil {
		t.Fatal(err)
	}
	source.fingerprints[tool] = "schema-v1"
	got, err := authorizer.Decide(tool, raw)
	if err != nil {
		t.Fatal(err)
	}
	requireRequestDenial(t, got, policy.decision)
	if policy.calls != 1 || policy.name != tool || string(policy.raw) != string(raw) || source.lookups != 0 {
		t.Fatal("policy input changed or newly discovered tool was adopted in same response")
	}
	specs := []ports.ToolSpec{{Name: tool}}
	if err := authorizer.CaptureRequest(specs); err != nil {
		t.Fatal(err)
	}
	source.beforeLookup = func() {
		if policy.calls < 2 {
			t.Fatal("declaration constraint ran before base Policy")
		}
	}
	got, err = authorizer.Decide(tool, raw)
	if err != nil || !reflect.DeepEqual(got, policy.decision) {
		t.Fatalf("next request lost policy result: %+v %v", got, err)
	}
	source.beforeLookup = nil
	source.fingerprints[tool] = "schema-v2"
	got, err = authorizer.Decide(tool, raw)
	if err != nil {
		t.Fatal(err)
	}
	requireRequestDenial(t, got, policy.decision)
	if err := authorizer.CaptureRequest(specs); err != nil {
		t.Fatal(err)
	}
	got, err = authorizer.Decide(tool, raw)
	if err != nil || !reflect.DeepEqual(got, policy.decision) {
		t.Fatalf("fresh schema was not adopted: %+v %v", got, err)
	}
	delete(source.fingerprints, tool)
	got, err = authorizer.Decide(tool, raw)
	if err != nil {
		t.Fatal(err)
	}
	requireRequestDenial(t, got, policy.decision)
}

// TestRequestAuthorizerCaptureFreezesVisibilityAndResetPreservesScriptContract 区分空模型请求与没有模型请求。
func TestRequestAuthorizerCaptureFreezesVisibilityAndResetPreservesScriptContract(t *testing.T) {
	authorizer, policy, source := newTestRequestAuthorizer(t)
	const tool = "mcp__docs__lookup"
	decide := func() contract.AuthorizationDecision {
		t.Helper()
		result, err := authorizer.Decide(tool, json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if got := decide(); !reflect.DeepEqual(got, policy.decision) {
		t.Fatal("initial script plan gained a model declaration requirement")
	}
	if err := authorizer.CaptureRequest([]ports.ToolSpec{{Name: tool}}); err != nil {
		t.Fatal(err)
	}
	source.fingerprints[tool] = "schema-v1"
	requireRequestDenial(t, decide(), policy.decision)
	if err := authorizer.CaptureRequest([]ports.ToolSpec{{Name: tool}}); err != nil {
		t.Fatal(err)
	}
	if got := decide(); !reflect.DeepEqual(got, policy.decision) {
		t.Fatal("visible declaration was not captured")
	}
	if err := authorizer.CaptureRequest(nil); err != nil {
		t.Fatal(err)
	}
	requireRequestDenial(t, decide(), policy.decision)
	lookups := source.lookups
	if err := authorizer.ResetRequestScope(); err != nil {
		t.Fatal(err)
	}
	if got := decide(); !reflect.DeepEqual(got, policy.decision) || source.lookups != lookups {
		t.Fatal("Reset did not restore no-model script contract")
	}
	if err := authorizer.CaptureRequest([]ports.ToolSpec{}); err != nil {
		t.Fatal(err)
	}
	requireRequestDenial(t, decide(), policy.decision)
}

// TestRequestAuthorizerNeverExpandsPolicy 覆盖 allow、ask、deny 和未知效果，完整保留指纹及身份。
func TestRequestAuthorizerNeverExpandsPolicy(t *testing.T) {
	for _, effect := range []contract.AuthorizationEffect{contract.AuthorizationAllow, contract.AuthorizationAsk, contract.AuthorizationDeny, "unknown"} {
		t.Run(string(effect), func(t *testing.T) {
			authorizer, policy, source := newTestRequestAuthorizer(t)
			policy.decision.Effect = effect
			const tool = "mcp__docs__lookup"
			source.fingerprints[tool] = "schema-v1"
			if err := authorizer.CaptureRequest([]ports.ToolSpec{{Name: tool}}); err != nil {
				t.Fatal(err)
			}
			got, err := authorizer.Decide(tool, json.RawMessage(`{}`))
			if err != nil || !reflect.DeepEqual(got, policy.decision) {
				t.Fatalf("matching declaration changed base Policy: %+v %v", got, err)
			}
			source.fingerprints[tool] = "schema-v2"
			got, err = authorizer.Decide(tool, json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			requireRequestDenial(t, got, policy.decision)
		})
	}
}

// TestRequestAuthorizerOrdinaryToolsBypassDeclarationChecks 保留帮助工具、内部回收及普通工具的底层授权合同。
func TestRequestAuthorizerOrdinaryToolsBypassDeclarationChecks(t *testing.T) {
	authorizer, policy, source := newTestRequestAuthorizer(t)
	if err := authorizer.CaptureRequest([]ports.ToolSpec{{Name: "read_file"}, {Name: "mcp_discover"}, {Name: "tool_search"}, {Name: contract.MCPQuiesceTool}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"read_file", "mcp_discover", "tool_search", contract.MCPQuiesceTool, "mcp_docs", ""} {
		got, err := authorizer.Decide(name, json.RawMessage(`{}`))
		if err != nil || !reflect.DeepEqual(got, policy.decision) {
			t.Fatalf("ordinary tool Policy changed: %q %+v %v", name, got, err)
		}
	}
	if source.lookups != 0 {
		t.Fatal("non-dynamic tools queried declarations")
	}
}

// TestRequestAuthorizerPropagatesPolicyError 在底层错误后停止，不查询或改写声明与判定。
func TestRequestAuthorizerPropagatesPolicyError(t *testing.T) {
	authorizer, policy, source := newTestRequestAuthorizer(t)
	const tool = "mcp__docs__lookup"
	source.fingerprints[tool] = "schema-v1"
	if err := authorizer.CaptureRequest([]ports.ToolSpec{{Name: tool}}); err != nil {
		t.Fatal(err)
	}
	source.fingerprints[tool] = "schema-v2"
	lookups := source.lookups
	sentinel := errors.New("policy failed")
	policy.decisionErr = sentinel
	got, err := authorizer.Decide(tool, json.RawMessage(`{}`))
	if !errors.Is(err, sentinel) || !reflect.DeepEqual(got, policy.decision) || source.lookups != lookups {
		t.Fatalf("policy error changed or constraint continued: %+v %v", got, err)
	}
}

// TestRequestAuthorizerDelegatesVerificationPatchAndObservation 保留同一 Policy 的全部参数、结果与错误。
func TestRequestAuthorizerDelegatesVerificationPatchAndObservation(t *testing.T) {
	authorizer, policy, source := newTestRequestAuthorizer(t)
	if err := authorizer.CaptureRequest(nil); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{ "argv": ["test"] }`)
	policy.verificationDecision = policy.decision
	policy.verificationDecision.Effect = contract.AuthorizationAsk
	policy.patch = contract.PatchPlan{Path: "target", Mode: "update", Before: []byte("before"), After: []byte("after"), BeforeHash: "sha256:before", AfterHash: "sha256:after", Exists: true}
	policy.observation = "sha256:observation"
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "source error"}[fail], func(t *testing.T) {
			var sentinel error
			if fail {
				sentinel = errors.New("delegated policy failed")
			}
			policy.verificationErr = sentinel
			policy.patchErr = sentinel
			policy.observationErr = sentinel
			got, err := authorizer.DecideVerificationWithObservation(raw, "sha256:plan", 3, "sha256:config", "sha256:observed")
			if !reflect.DeepEqual(got, policy.verificationDecision) || !errors.Is(err, sentinel) || string(policy.verificationRaw) != string(raw) || policy.planRef != "sha256:plan" || policy.commandIndex != 3 || policy.configFingerprint != "sha256:config" || policy.verificationObservation != "sha256:observed" {
				t.Fatalf("verification delegation changed: %+v %v", got, err)
			}
			patch, err := authorizer.PlanPatch(raw)
			if !reflect.DeepEqual(patch, policy.patch) || !errors.Is(err, sentinel) || string(policy.patchRaw) != string(raw) {
				t.Fatalf("patch delegation changed: %+v %v", patch, err)
			}
			observation, err := authorizer.WorkspaceObservation()
			if observation != policy.observation || !errors.Is(err, sentinel) {
				t.Fatalf("observation delegation changed: %q %v", observation, err)
			}
		})
	}
	if policy.verificationCalls != 2 || policy.patchCalls != 2 || policy.observationCalls != 2 || policy.calls != 0 || source.lookups != 0 {
		t.Fatalf("delegation used a different path: %+v", policy)
	}
}

// TestRequestAuthorizerRequiresBothSources 构造缺少任一来源失败，零值也不能默认允许。
func TestRequestAuthorizerRequiresBothSources(t *testing.T) {
	policy := &requestPolicy{}
	source := &requestDeclarations{}
	for _, test := range []struct {
		policy PolicySource
		source DeclarationSource
	}{{nil, nil}, {policy, nil}, {nil, source}} {
		authorizer, err := NewRequestAuthorizer(test.policy, test.source)
		if err == nil || authorizer != nil {
			t.Fatalf("missing source constructed an authorizer: %+v %v", authorizer, err)
		}
	}
	for _, authorizer := range []*RequestAuthorizer{nil, {}, {policy: policy}, {declarations: source}} {
		checks := []func() error{
			func() error { return authorizer.ResetRequestScope() },
			func() error { return authorizer.CaptureRequest(nil) },
			func() error { _, err := authorizer.Decide("mcp__docs__lookup", json.RawMessage(`{}`)); return err },
			func() error { _, err := authorizer.DecideVerificationWithObservation(nil, "", 0, "", ""); return err },
			func() error { _, err := authorizer.PlanPatch(nil); return err },
			func() error { _, err := authorizer.WorkspaceObservation(); return err },
		}
		for _, check := range checks {
			if err := check(); err == nil {
				t.Fatal("incomplete authorizer failed open")
			}
		}
	}
}
