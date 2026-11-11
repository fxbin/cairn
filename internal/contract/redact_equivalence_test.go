// redact_equivalence_test.go 锁定脱敏合同迁移后的行为等价：词干判定、
// 兼容入口与文本脱敏输出必须与迁移前逐字节一致。
//
// Author: fxbin
package contract_test

import (
	"testing"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/runtime/credential"
)

// TestSensitiveKeyVocabulary 锁定合同层词表的命中与放行边界，覆盖连字符
// 归一化与 _pat 后缀；这些边界直接决定脱敏是否会漏判或误伤正常字段。
func TestSensitiveKeyVocabulary(t *testing.T) {
	sensitive := []string{
		"ACCESS_TOKEN", "client-secret", "DB_PASSWORD", "DB_PASSWD", "passphrase",
		"API_KEY", "apikey", "CREDENTIAL", "AWS_ACCESS_KEY_ID", "DEPLOY_PRIVATE_KEY",
		"CLIENT_KEY", "SESSION_KEY", "SIGNING_KEY", "LICENSE_KEY", "GITHUB_PAT",
	}
	for _, name := range sensitive {
		if !contract.SensitiveKey(name) {
			t.Errorf("SensitiveKey(%q) = false, want true", name)
		}
	}
	insensitive := []string{"PATH", "AUTHOR", "AUTH_TYPE", "PRIMARY_KEY", "PWD", "OLDPWD", "MONKEY", "HOCKEY"}
	for _, name := range insensitive {
		if contract.SensitiveKey(name) {
			t.Errorf("SensitiveKey(%q) = true, want false", name)
		}
	}
}

// TestCredentialKeyDelegatesToContract 锁定 credential.Key 与合同层判定逐项
// 一致，避免兼容入口在后续调整中与词表脱节。
func TestCredentialKeyDelegatesToContract(t *testing.T) {
	names := []string{
		"ACCESS_TOKEN", "client-secret", "DB_PASSWD", "GITHUB_PAT", "API-KEY",
		"PATH", "AUTHOR", "PRIMARY_KEY", "OLDPWD",
	}
	for _, name := range names {
		if credential.Key(name) != contract.SensitiveKey(name) {
			t.Errorf("credential.Key(%q) 与 contract.SensitiveKey 不一致", name)
		}
	}
}

// TestRedactOutputByteEquivalent 锁定文本脱敏在合同迁移后的逐字节输出，
// 同时覆盖键名敏感、非敏感字段放行与固定格式替换三条路径。
func TestRedactOutputByteEquivalent(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"DB_PASSWORD=hunter2", "DB_PASSWORD=[REDACTED]"},
		{"API_KEY=abcdefghijklmnop", "API_KEY=[REDACTED]"},
		{"GITHUB_PAT=abcdefghijkl", "GITHUB_PAT=[REDACTED]"},
		{"client-secret: abcdefghijklmnopqrst", "client-secret:[REDACTED]"},
		{"PORT=8080", "PORT=8080"},
		{"AUTHOR=nobody", "AUTHOR=nobody"},
		{"Bearer abcdefghijklmnop", "Bearer [REDACTED]"},
		{"sk-abcdefghijklmnop", "[REDACTED]"},
	}
	for _, test := range cases {
		if got := string(contract.Redact([]byte(test.input))); got != test.want {
			t.Errorf("Redact(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}
