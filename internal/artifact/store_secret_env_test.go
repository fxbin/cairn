package artifact

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactRemovesShortCredentialEnvAssignments 锁定行锚定裸赋值脱敏：短的
// 环境式凭据（DB_PASSWORD=hunter2）不限长度必须清除，非凭据键不受影响。
func TestRedactRemovesShortCredentialEnvAssignments(t *testing.T) {
	input := "DB_PASSWORD=hunter2\nSESSION_KEY=abc\nPORT=8080\n"
	output := string(Redact([]byte(input)))
	for _, required := range []string{"DB_PASSWORD=[REDACTED]", "SESSION_KEY=[REDACTED]", "PORT=8080"} {
		if !strings.Contains(output, required) {
			t.Fatalf("output %q missing %q", output, required)
		}
	}
	if strings.Contains(output, "hunter2") || strings.Contains(output, "abc\n") {
		t.Fatalf("credential value survived: %q", output)
	}
}

// TestRedactKeepsNonCredentialCodeSyntax 锁定误伤边界：带空格的比较/赋值、
// 非行首的标志式参数不是环境式裸赋值，不得被行锚定规则改动。
func TestRedactKeepsNonCredentialCodeSyntax(t *testing.T) {
	input := "if password == \"\" {\n\ttoken := os.Getenv(\"TOKEN\")\n}\n--password=short\npassword = spaced\n"
	if output := string(Redact([]byte(input))); output != input {
		t.Fatalf("non-env syntax must stay untouched:\ninput : %q\noutput: %q", input, output)
	}
}

// TestRedactJSONScrubsBareCredentialAssignmentInStringLeaf 锁定 JSON 字符串
// 叶子的行锚定脱敏：工具参数（如 apply_patch content）中的短裸凭据赋值会随
// RedactJSON 一并被清除，从而使 containsSecretJSON 能将其判定为敏感。
func TestRedactJSONScrubsBareCredentialAssignmentInStringLeaf(t *testing.T) {
	payload, err := json.Marshal(map[string]any{"content": "DB_PASSWORD=hunter2\nPORT=8080\n"})
	if err != nil {
		t.Fatal(err)
	}
	redacted := string(RedactJSON(payload))
	if strings.Contains(redacted, "hunter2") || !strings.Contains(redacted, "DB_PASSWORD=[REDACTED]") {
		t.Fatalf("string leaf not scrubbed: %s", redacted)
	}
	if !strings.Contains(redacted, "PORT=8080") {
		t.Fatalf("non-credential line must survive: %s", redacted)
	}
}

// TestRedactQuotedCredentialAtEOF 覆盖截断值的双/单引号与短片段，同时保留完整短值和非敏感键。
func TestRedactQuotedCredentialAtEOF(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "double long EOF", input: `API_KEY="boundary-value`, want: "API_KEY=[REDACTED]"},
		{name: "single long EOF", input: `DB_PASSWORD='boundary-value`, want: "DB_PASSWORD=[REDACTED]"},
		{name: "double one EOF", input: `API_KEY="Z`, want: "API_KEY=[REDACTED]"},
		{name: "double two EOF", input: `API_KEY="Z7`, want: "API_KEY=[REDACTED]"},
		{name: "double three EOF", input: `API_KEY="Z7X`, want: "API_KEY=[REDACTED]"},
		{name: "single one EOF", input: `DB_PASSWORD='Z`, want: "DB_PASSWORD=[REDACTED]"},
		{name: "single two EOF", input: `DB_PASSWORD='Z7`, want: "DB_PASSWORD=[REDACTED]"},
		{name: "single three EOF", input: `DB_PASSWORD='Z7X`, want: "DB_PASSWORD=[REDACTED]"},
		{name: "closed short double", input: `API_KEY="Z7X"`, want: `API_KEY="Z7X"`},
		{name: "closed short single", input: `DB_PASSWORD='Z7X'`, want: `DB_PASSWORD='Z7X'`},
		{name: "public double EOF", input: `MESSAGE="boundary-value`, want: `MESSAGE="boundary-value`},
		{name: "public single EOF", input: `PORT='8080`, want: `PORT='8080`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := string(Redact([]byte(test.input))); got != test.want {
				t.Error("文本 EOF 脱敏不符合正负控")
			}
			payload, err := json.Marshal(struct {
				Stdout string `json:"stdout"`
			}{Stdout: test.input})
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Stdout string `json:"stdout"`
			}
			if err := json.Unmarshal(RedactJSON(payload), &result); err != nil {
				t.Fatal(err)
			}
			if result.Stdout != test.want {
				t.Error("JSON 叶子的 EOF 脱敏不符合正负控")
			}
		})
	}
}
