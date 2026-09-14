package artifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRedactsBeforePersistingAndUsesStrictPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := New(root, 64)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put([]byte("token=top-secret-value\nBearer abcdefghijklmnop\nkeep"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ref.TruncatedPreview, "top-secret") || !strings.Contains(ref.TruncatedPreview, "[REDACTED]") {
		t.Fatalf("preview leaked secret: %#v", ref)
	}
	digest := strings.TrimPrefix(ref.SHA256, "sha256:")
	content, err := os.ReadFile(filepath.Join(root, "sha256", digest[:2], digest))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "top-secret") || !strings.Contains(string(content), "[REDACTED]") {
		t.Fatalf("artifact leaked secret: %q", content)
	}
	info, err := os.Stat(filepath.Join(root, "sha256", digest[:2], digest))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact permissions = %v, %v", info.Mode().Perm(), err)
	}
}

func TestStoreContentAddressingDeduplicates(t *testing.T) {
	store, err := New(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Put([]byte("same"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put([]byte("same"), "text/plain")
	if err != nil || first.SHA256 != second.SHA256 {
		t.Fatalf("refs = %#v %#v, %v", first, second, err)
	}
}

func TestStoreRejectsArtifactsAboveConfiguredLimit(t *testing.T) {
	store, err := New(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	store.MaxBytes = 3
	if _, err := store.Put([]byte("four"), "text/plain"); err == nil {
		t.Fatal("oversized artifact was persisted")
	}
}

func TestRedactJSONReplacesCredentialValuesWithoutBreakingJSON(t *testing.T) {
	redacted := RedactJSON([]byte(`{"token":"very-secret","nested":{"api_key":"also-secret"},"message":"Bearer abcdefghijklmnop"}`))
	if strings.Contains(string(redacted), "secret") || !strings.Contains(string(redacted), "[REDACTED]") {
		t.Fatalf("redaction failed: %s", redacted)
	}
	var decoded map[string]any
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("redaction broke JSON: %v", err)
	}
}

func TestRedactJSONCoversExpandedCredentialVocabulary(t *testing.T) {
	const sentinel = "credential-sentinel-value"
	input := []byte(`{"AWS_ACCESS_KEY_ID":"` + sentinel + `","DEPLOY_PRIVATE_KEY":"` + sentinel + `","DB_PASSWD":"` + sentinel + `","GITHUB_PAT":"` + sentinel + `"}`)
	redacted := RedactJSON(input)
	if strings.Contains(string(redacted), sentinel) {
		t.Fatalf("expanded credential key leaked: %s", redacted)
	}
	text := Redact([]byte("GITHUB_PAT=" + sentinel))
	if strings.Contains(string(text), sentinel) {
		t.Fatalf("credential assignment leaked: %s", text)
	}
}

func TestRedactJSONPreservesTokenUsageAndBudgetCounters(t *testing.T) {
	redacted := RedactJSON([]byte(`{"token":"very-secret","session_token":"also-secret","usage":{"prompt_tokens":123,"completion_tokens":45,"total_tokens":168},"budget":{"max_input_tokens":250000,"max_output_tokens":50000}}`))
	if strings.Contains(string(redacted), "very-secret") || strings.Contains(string(redacted), "also-secret") {
		t.Fatalf("redaction leaked credential: %s", redacted)
	}
	var decoded struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Budget struct {
			MaxInputTokens  int `json:"max_input_tokens"`
			MaxOutputTokens int `json:"max_output_tokens"`
		} `json:"budget"`
	}
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Usage.PromptTokens != 123 || decoded.Usage.CompletionTokens != 45 || decoded.Usage.TotalTokens != 168 || decoded.Budget.MaxInputTokens != 250000 || decoded.Budget.MaxOutputTokens != 50000 {
		t.Fatalf("token facts were redacted: %#v", decoded)
	}
}

func TestRedactJSONKeepsSourceLiteralWithCredentialLikeTextValid(t *testing.T) {
	input := []byte(`{"data":{"content":"API_KEY = \\\"sk-abcdefghijklmnop\\\"\\n"}}`)
	redacted := RedactJSON(input)
	if strings.Contains(string(redacted), "sk-abcdefghijklmnop") || !strings.Contains(string(redacted), "[REDACTED]") {
		t.Fatalf("redaction leaked credential-like source literal: %s", redacted)
	}
	var decoded struct {
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("redaction broke source-bearing JSON: %v; %s", err, redacted)
	}
	if !strings.Contains(decoded.Data.Content, "[REDACTED]") {
		t.Fatalf("decoded source was not redacted: %q", decoded.Data.Content)
	}
}

// TestRedactDoesNotFlagTypeAnnotations 防回归：凭据脱敏不得误伤 Python 类型注解或短赋值，
// 否则 auth 类任务的合法补丁（如 def hash_token(token: str)）会被错误拒绝。
func TestRedactDoesNotFlagTypeAnnotations(t *testing.T) {
	cases := []string{
		`def hash_token(token: str) -> str:`,
		`def hash_token(token: Optional[str] = None) -> None:`,
		`token = request.headers.get("Authorization", "")`,
		`access_token: str = field(default="")`,
	}
	for _, in := range cases {
		got := string(Redact([]byte(in)))
		if strings.Contains(got, "[REDACTED]") {
			t.Fatalf("type annotation / short assignment wrongly redacted: %q -> %q", in, got)
		}
		if got != in {
			t.Fatalf("non-secret code was modified: %q -> %q", in, got)
		}
	}
}

// TestRedactStillFlagsQuotedAndLongCredentials 确认收紧后仍能脱敏真实凭据：
// 引号包裹（≥4 字符）或足够长（≥16 字符）的裸值。
func TestRedactStillFlagsQuotedAndLongCredentials(t *testing.T) {
	cases := []string{
		`api_key = "sk-live-abc123def456"`,
		`token: "eyJhbGciOiJIUzI1NiJ9.payload.sig"`,
		`password = "hunter2-secure!"`,
		`SECRET = aGlnaGVudHJvcHkxMjM0NTY3OA`,
	}
	for _, in := range cases {
		got := string(Redact([]byte(in)))
		if !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("real credential was not redacted: %q -> %q", in, got)
		}
	}
}

// TestRedactCatchesQuotedJSONKeyForms 锁定文本脱敏对 JSON 引号键形态的覆盖：
// 键与冒号隔着引号时值仍被脱敏，短值与类型注解不受误伤。
func TestRedactCatchesQuotedJSONKeyForms(t *testing.T) {
	secret := []byte(`{"api_key":"AKIA1234567890ABCD","password":"hunter2secret"}`)
	redacted := string(Redact(secret))
	if strings.Contains(redacted, "AKIA1234567890ABCD") || strings.Contains(redacted, "hunter2secret") {
		t.Fatalf("quoted JSON credentials survived: %s", redacted)
	}
	benign := []byte(`def hash_token(token: str) -> str: return token`)
	if got := string(Redact(benign)); got != string(benign) {
		t.Fatalf("benign annotation was redacted: %s", got)
	}
}

// TestPutUsesStructuralRedactionForJSONArtifacts 锁定 JSON 制品走递归键脱敏：
// 内嵌于嵌套对象的凭据值在制品内容中被清除且结构保持合法 JSON。
func TestPutUsesStructuralRedactionForJSONArtifacts(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "artifacts"), 128)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte(`{"nested":{"password":"super-secret-value"},"note":"plain"}`)
	reference, err := store.Put(content, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(reference.SHA256, "sha256:")
	stored, err := os.ReadFile(filepath.Join(store.Root, "sha256", digest[:2], digest))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "super-secret-value") {
		t.Fatalf("nested credential survived JSON artifact redaction: %s", stored)
	}
	if !json.Valid(stored) {
		t.Fatalf("redacted artifact is no longer valid JSON: %s", stored)
	}
	if !strings.Contains(string(stored), "plain") {
		t.Fatalf("non-sensitive content was removed: %s", stored)
	}
}

// TestOpenVerifiesContentHash 锁定制品读取完整性：正确内容可读回，篡改或
// 缺失按引用失败关闭。
func TestOpenVerifiesContentHash(t *testing.T) {
	store, err := New(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put([]byte("prior-context-card"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	content, err := store.Open(ref)
	if err != nil || string(content) != "prior-context-card" {
		t.Fatalf("应读回制品内容: %q err=%v", content, err)
	}
	tampered := Ref{SHA256: ref.SHA256, Bytes: ref.Bytes, MediaType: ref.MediaType}
	tampered.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := store.Open(tampered); err == nil {
		t.Fatal("摘要不符的引用应失败关闭")
	}
	missing := Ref{SHA256: "abcdef0000000000000000000000000000000000000000000000000000000000"}
	if _, err := store.Open(missing); err == nil {
		t.Fatal("缺失制品应返回错误")
	}
}

func TestRedactJSONPreservesExactNumbers(t *testing.T) {
	input := []byte(`{"byte_offset":9007199254740993,"total_bytes":9223372036854775807,"ratio":0.1234567890123456789,"exponent":1e400,"api_key":"private-value"}`)
	redacted := RedactJSON(input)
	var value struct {
		Offset   json.Number `json:"byte_offset"`
		Total    json.Number `json:"total_bytes"`
		Ratio    json.Number `json:"ratio"`
		Exponent json.Number `json:"exponent"`
		Key      string      `json:"api_key"`
	}
	if err := json.Unmarshal(redacted, &value); err != nil {
		t.Fatal(err)
	}
	if value.Offset.String() != "9007199254740993" || value.Total.String() != "9223372036854775807" || value.Ratio.String() != "0.1234567890123456789" || value.Exponent.String() != "1e400" || value.Key != "[REDACTED]" {
		t.Fatalf("numbers or redaction changed: %s", redacted)
	}
	if trailing := RedactJSON([]byte(`{"number":1} trailing`)); !strings.Contains(string(trailing), "trailing") {
		t.Fatalf("accepted trailing content: %s", trailing)
	}
}
