// artifact_ref_test.go 锁定制品引用迁移到合同层后的序列化金标准：字段顺序、
// JSON 名称与往返结果必须与迁移前逐字节一致。
//
// Author: fxbin
package contract_test

import (
	"encoding/json"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
)

// TestArtifactRefSerializationContract 用金标准样本固定 Ref 的 JSON 字节：
// 事件载荷中该对象作为原始 JSON 嵌入，字段顺序或名称变化都会改变事件哈希。
func TestArtifactRefSerializationContract(t *testing.T) {
	want := `{"sha256":"sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae","bytes":42,"media_type":"application/json","truncated_preview":"preview"}`
	reference := contract.Ref{
		SHA256:           "sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae",
		Bytes:            42,
		MediaType:        "application/json",
		TruncatedPreview: "preview",
	}
	encoded, err := json.Marshal(reference)
	if err != nil {
		t.Fatalf("marshal artifact ref: %v", err)
	}
	if string(encoded) != want {
		t.Fatalf("artifact ref JSON changed:\n got: %s\nwant: %s", encoded, want)
	}
	var decoded contract.Ref
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatalf("unmarshal artifact ref: %v", err)
	}
	if decoded != reference {
		t.Fatalf("artifact ref round trip mismatch: got %#v want %#v", decoded, reference)
	}
}
