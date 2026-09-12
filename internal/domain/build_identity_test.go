package domain

import (
	"strings"
	"testing"
)

// TestValidateBuildIdentityFlavors 表驱动锁定构建身份校验合同：plain 与 otelc
// 是仅有的两种合法 flavor；工具链事实只在 otelc 下允许出现且必须成对完整；
// 既有 plain 合同（dirty 三态、指纹必填、nil 兼容）保持不变。
func TestValidateBuildIdentityFlavors(t *testing.T) {
	validPlain := BuildIdentity{Dirty: "unknown", Flavor: "plain", Fingerprint: "sha256:f"}
	validOtelc := BuildIdentity{Dirty: "false", Flavor: "otelc", Fingerprint: "sha256:g", OtelcVersion: "v1.1.0", InstrumentationDigest: "sha256:i"}
	cases := []struct {
		name     string
		identity *BuildIdentity
		wantErr  string
	}{
		{"nil 身份兼容旧事件", nil, ""},
		{"plain 合法", &validPlain, ""},
		{"otelc 携带完整工具链事实合法", &validOtelc, ""},
		{"未知 flavor 拒绝", &BuildIdentity{Dirty: "unknown", Flavor: "instrumented", Fingerprint: "sha256:f"}, "unsupported"},
		{"plain 携带工具链版本拒绝", &BuildIdentity{Dirty: "unknown", Flavor: "plain", Fingerprint: "sha256:f", OtelcVersion: "v1.1.0"}, "must not carry"},
		{"plain 携带插桩摘要拒绝", &BuildIdentity{Dirty: "unknown", Flavor: "plain", Fingerprint: "sha256:f", InstrumentationDigest: "sha256:i"}, "must not carry"},
		{"otelc 缺工具链版本拒绝", &BuildIdentity{Dirty: "unknown", Flavor: "otelc", Fingerprint: "sha256:f", InstrumentationDigest: "sha256:i"}, "requires toolchain"},
		{"otelc 缺插桩摘要拒绝", &BuildIdentity{Dirty: "unknown", Flavor: "otelc", Fingerprint: "sha256:f", OtelcVersion: "v1.1.0"}, "requires toolchain"},
		{"dirty 非法拒绝", &BuildIdentity{Dirty: "maybe", Flavor: "plain", Fingerprint: "sha256:f"}, "dirty"},
		{"指纹缺失拒绝", &BuildIdentity{Dirty: "unknown", Flavor: "plain"}, "fingerprint"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateBuildIdentity(testCase.identity)
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("validate = %v, want containing %q", err, testCase.wantErr)
			}
		})
	}
}

// TestComputeFingerprintPlainShapeUnchanged 锁定 plain 身份摘要的黄金值：可选
// 工具链字段缺省不进入编码，摘要与历史二进制逐字节一致；otelc 事实一旦出现
// 必然改变摘要（build_fingerprint 与 config_fingerprint 分离的域基础）。
func TestComputeFingerprintPlainShapeUnchanged(t *testing.T) {
	const golden = "sha256:80f53aa8058f8877b5d87a2e6569c0a5087011fa0297ea50c51ad41f592f1b66"
	plain := BuildIdentity{Dirty: "unknown", Flavor: "plain"}
	if got := plain.ComputeFingerprint(); got != golden {
		t.Fatalf("plain fingerprint = %s, want golden %s", got, golden)
	}
	otelc := BuildIdentity{Dirty: "unknown", Flavor: "otelc", OtelcVersion: "v1.1.0", InstrumentationDigest: "sha256:i"}
	if otelc.ComputeFingerprint() == golden {
		t.Fatal("otelc facts must change the build fingerprint")
	}
}
