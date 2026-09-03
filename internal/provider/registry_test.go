package provider

import (
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
)

// TestProtocolMappingIsClosedAndBijective 锁定 D1 双词表映射：唯一条目双向
// 闭合，未知值失败关闭，历史字面值不被重新解释。
func TestProtocolMappingIsClosedAndBijective(t *testing.T) {
	protocol, err := ProtocolFromKind(contract.ProviderKindOpenAICompatible)
	if err != nil || protocol != contract.ProtocolOpenAIChatCompletions {
		t.Fatalf("kind→protocol mapping failed: %q %v", protocol, err)
	}
	kind, err := KindFromProtocol(contract.ProtocolOpenAIChatCompletions)
	if err != nil || kind != contract.ProviderKindOpenAICompatible {
		t.Fatalf("protocol→kind mapping failed: %q %v", kind, err)
	}
	if _, err := ProtocolFromKind("unknown_kind"); err == nil {
		t.Fatal("unknown kind must fail closed")
	}
	if _, err := KindFromProtocol("unknown-protocol"); err == nil {
		t.Fatal("unknown protocol must fail closed")
	}
}

// TestLookupFailsClosedOnUnknownProvider 锁定静态注册表：custom 是唯一成员，
// 未知或形状非法的 ProviderID 失败关闭，没有隐式回退。
func TestLookupFailsClosedOnUnknownProvider(t *testing.T) {
	definition, err := Lookup(contract.ProviderIDCustom)
	if err != nil || definition.ProtocolID != contract.ProtocolOpenAIChatCompletions {
		t.Fatalf("custom lookup failed: %+v %v", definition, err)
	}
	for _, invalid := range []string{"", "OpenAI", "zhipu_ai", "-lead", "a" + strings.Repeat("b", providerIDMaxLen) + "c"} {
		if _, err := Lookup(invalid); err == nil {
			t.Fatalf("provider id %q must be rejected", invalid)
		}
	}
	if _, err := Lookup("unregistered"); err == nil {
		t.Fatal("unregistered provider must fail closed without custom fallback")
	}
}

// TestResolveSelectionDerivesModelRefAndDigest 锁定 Selection 派生合同：
// provider_id 缺省归一为 custom；model_ref 由 provider_id 与原始 model 派生；
// endpoint_digest 复用无凭据规范化摘要；model 缺失或基地址非法失败关闭。
func TestResolveSelectionDerivesModelRefAndDigest(t *testing.T) {
	selection, err := ResolveSelection(SelectionFacts{Model: "glm-4.7", BaseURL: "https://API.Example.com/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	if selection.SchemaVersion != providerSelectionSchemaV1 {
		t.Fatalf("schema version = %d", selection.SchemaVersion)
	}
	if selection.ProviderID != contract.ProviderIDCustom {
		t.Fatalf("default provider id = %q", selection.ProviderID)
	}
	if selection.ProtocolID != contract.ProtocolOpenAIChatCompletions {
		t.Fatalf("protocol = %q", selection.ProtocolID)
	}
	if selection.ModelRef != contract.ProviderIDCustom+"/glm-4.7" {
		t.Fatalf("model ref = %q", selection.ModelRef)
	}
	if !strings.HasPrefix(selection.EndpointDigest, "sha256:") {
		t.Fatalf("endpoint digest = %q", selection.EndpointDigest)
	}
	if _, err := ResolveSelection(SelectionFacts{BaseURL: "https://api.example.com"}); err == nil {
		t.Fatal("missing model must fail closed")
	}
	if _, err := ResolveSelection(SelectionFacts{Model: "m", BaseURL: "not-a-url"}); err == nil {
		t.Fatal("invalid base URL must fail closed")
	}
}

// TestResolveSelectionIsDeterministicForSameFacts 锁定同语义同指纹前提：
// 相同身份事实（含大小写与尾斜杠差异的规范化等价基地址）产出相同 selection。
func TestResolveSelectionIsDeterministicForSameFacts(t *testing.T) {
	first, err := ResolveSelection(SelectionFacts{Model: "m", BaseURL: "https://api.example.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveSelection(SelectionFacts{Model: "m", BaseURL: "https://API.EXAMPLE.com/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("normalized-equivalent facts must yield identical selections: %+v vs %+v", first, second)
	}
}
