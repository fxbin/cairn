package canonicaljson

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// TestMarshalSortsKeysAndKeepsArrayOrder 锁定核心规范化行为：对象键跨层按
// 字节序输出，数组保持原顺序，不同插入顺序的等价 map 得到相同字节串。
func TestMarshalSortsKeysAndKeepsArrayOrder(t *testing.T) {
	first := map[string]any{"b": 2, "a": 1, "nested": map[string]any{"z": true, "y": "文本"}, "list": []any{3, 1, 2}}
	second := map[string]any{"a": 1, "list": []any{3, 1, 2}, "nested": map[string]any{"y": "文本", "z": true}, "b": 2}
	left, err := Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	right, err := Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if string(left) != string(right) {
		t.Fatalf("canonical output diverges: %s vs %s", left, right)
	}
	if string(left) != `{"a":1,"b":2,"list":[3,1,2],"nested":{"y":"文本","z":true}}` {
		t.Fatalf("unexpected canonical form: %s", left)
	}
}

// TestMarshalRejectsDuplicateKeysInEmbeddedJSON 验证内嵌原始 JSON 携带的重复
// 键被显式拒绝，而不是静默取后值；这是冻结指纹证据完整性的底线。
func TestMarshalRejectsDuplicateKeysInEmbeddedJSON(t *testing.T) {
	type payload struct {
		Name string          `json:"name"`
		Raw  json.RawMessage `json:"raw"`
	}
	value := payload{Name: "x", Raw: json.RawMessage(`{"k":1,"k":2}`)}
	if _, err := Marshal(value); err == nil {
		t.Fatal("expected duplicate key rejection")
	}
}

// TestMarshalRejectsFloatingPointValues 覆盖反射预检：float64 字段即使值为
// 整数、嵌套指针、切片元素与 map 值都不得进入指纹字节串。
func TestMarshalRejectsFloatingPointValues(t *testing.T) {
	type nested struct {
		Rate *float64 `json:"rate"`
	}
	whole := 0.0
	cases := []struct {
		name  string
		value any
	}{
		{"integer-valued float field", struct {
			Temperature float64 `json:"temperature"`
		}{Temperature: 2}},
		{"pointer to float", nested{Rate: &whole}},
		{"float in slice", struct {
			Vals []any `json:"vals"`
		}{Vals: []any{1.5}}},
		{"float in map", map[string]any{"price": 0.25}},
	}
	for _, item := range cases {
		if _, err := Marshal(item.value); err == nil {
			t.Fatalf("%s: expected float rejection", item.name)
		} else if !strings.Contains(err.Error(), "floating-point") {
			t.Fatalf("%s: unexpected error %v", item.name, err)
		}
	}
}

// TestMarshalRejectsNonCanonicalNumberLiterals 验证解析层兜底：内嵌 JSON 中
// 的前导零、小数与指数字面量一律失败关闭。
func TestMarshalRejectsNonCanonicalNumberLiterals(t *testing.T) {
	cases := []string{
		`{"n":007}`,
		`{"n":1.5}`,
		`{"n":1e3}`,
		`{"n":-0}`,
		`[1.0]`,
	}
	for _, raw := range cases {
		if _, err := Marshal(json.RawMessage(raw)); err == nil {
			t.Fatalf("literal %s should be rejected", raw)
		}
	}
}

// TestMarshalStableOutputAcrossRuns 断言相同输入的两次序列化逐字节一致，且
// 禁用 HTML 转义后特殊字符不改写指纹形态。
func TestMarshalStableOutputAcrossRuns(t *testing.T) {
	value := map[string]any{"text": "<a>&\"b\"", "num": 42, "flag": false, "nil": nil}
	first, err := Marshal(value)
	if err != nil {
		t.Fatalf("first marshal: %v", err)
	}
	second, err := Marshal(value)
	if err != nil {
		t.Fatalf("second marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("unstable output: %s vs %s", first, second)
	}
	if strings.Contains(string(first), `\u003c`) || !strings.Contains(string(first), "<a>") {
		t.Fatalf("html escaping leaked into canonical form: %s", first)
	}
}

// TestFingerprintUsesPrefixAndDeterminism 锁定指纹公式：域前缀参与摘要、
// 输出携带算法标识、语义不同的输入不碰撞。
func TestFingerprintUsesPrefixAndDeterminism(t *testing.T) {
	value := map[string]any{"task_ids": []any{"TopicEye", "TokenTracker"}, "attempts": 3}
	first, err := Fingerprint("cairn-eval-set-v1", value)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	second, err := Fingerprint("cairn-eval-set-v1", value)
	if err != nil {
		t.Fatalf("fingerprint again: %v", err)
	}
	if first != second {
		t.Fatalf("fingerprint not deterministic: %s vs %s", first, second)
	}
	other, err := Fingerprint("cairn-other-domain-v1", value)
	if err != nil {
		t.Fatalf("prefixed fingerprint: %v", err)
	}
	if other == first {
		t.Fatal("different prefixes must produce different fingerprints")
	}
	if matched := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).FindString(first); matched != first {
		t.Fatalf("fingerprint format mismatch: %s", first)
	}
}
