package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/tools"
)

func TestGateUsesHashBoundPatchFingerprintAndProtectsPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := New("ask", "ask", []string{".git"}, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := gate.Decide("apply_patch", json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"`+patchHash([]byte("old"))+`","replacements":[{"old":"old","new":"new","occurrence":1}]}`))
	if err != nil || decision.Effect != Ask || decision.Fingerprint == "" {
		t.Fatalf("decision = %#v, %v", decision, err)
	}
	denied, err := gate.Decide("apply_patch", json.RawMessage(`{"path":".git/config","mode":"create","content":"x"}`))
	if err != nil || denied.Effect != Deny || denied.RuleID != "protected_path" {
		t.Fatalf("decision = %#v, %v", denied, err)
	}
}

func TestGateFingerprintsInvalidOperationsForLoopDetection(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "apply_patch", raw: json.RawMessage(`{"path":"missing.txt","mode":"update","patch":"bad"}`)},
		{name: "exec_command", raw: json.RawMessage(`{"argv":[]}`)},
	} {
		decision, err := gate.Decide(test.name, test.raw)
		if err != nil || decision.Effect != Deny || decision.Fingerprint == "" {
			t.Fatalf("decision for %s = %#v err=%v", test.name, decision, err)
		}
	}
}

// TestGateExposesStalePatchAsRecoverablePrecondition 锁定已提供但过期的摘要
// 不再伪装为普通策略拒绝；反馈仅包含安全恢复所需的路径、当前摘要和固定动作。
func TestGateExposesStalePatchAsRecoverablePrecondition(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("current file body"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := New("allow", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := gate.Decide("apply_patch", json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"`+patchHash([]byte("stale"))+`","replacements":[{"old":"current","new":"next"}]}`))
	if err != nil || decision.Effect != Deny || decision.RuleID != "stale_patch_precondition" || decision.FailureCode != tools.CodeStalePrecondition {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
	var details struct {
		Path          string `json:"path"`
		CurrentSHA256 string `json:"current_sha256"`
		NextAction    string `json:"next_action"`
	}
	if err := json.Unmarshal(decision.FailureData, &details); err != nil || details.Path != "file.txt" || details.CurrentSHA256 != patchHash([]byte("current file body")) || !strings.Contains(details.NextAction, "read_file") {
		t.Fatalf("details=%#v err=%v", details, err)
	}
	if strings.Contains(string(decision.FailureData), "current file body") || strings.Contains(decision.Summary, "current file body") {
		t.Fatalf("stale feedback leaked file body: %#v", decision)
	}
}

func TestVerificationFingerprintCannotBeUsedAsModelCommandApproval(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := New("allow", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	arguments := json.RawMessage(`{"argv":["/usr/bin/true"]}`)
	model, err := gate.Decide("exec_command", arguments)
	if err != nil || model.Effect != Ask {
		t.Fatalf("model decision=%#v err=%v", model, err)
	}
	first, err := gate.DecideVerification(arguments, "sha256:plan-a", 0, "sha256:config")
	if err != nil || first.Effect != Ask || first.Fingerprint == model.Fingerprint {
		t.Fatalf("first verification decision=%#v model=%#v err=%v", first, model, err)
	}
	second, err := gate.DecideVerification(arguments, "sha256:plan-b", 0, "sha256:config")
	if err != nil || second.Fingerprint == first.Fingerprint {
		t.Fatalf("second verification decision=%#v first=%#v err=%v", second, first, err)
	}
}

func TestExecApprovalFingerprintBindsNormalizedArgumentsAndWorkspaceObservation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := New("allow", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defaulted, err := gate.Decide("exec_command", json.RawMessage(`{"argv":["/usr/bin/true"]}`))
	if err != nil || defaulted.Effect != Ask || defaulted.WorkspaceObservation == "" {
		t.Fatalf("defaulted decision=%#v err=%v", defaulted, err)
	}
	explicit, err := gate.Decide("exec_command", json.RawMessage(`{"argv":["/usr/bin/true"],"cwd":".","timeout_ms":120000,"max_output_bytes":262144}`))
	if err != nil || explicit.Fingerprint != defaulted.Fingerprint {
		t.Fatalf("normalized decision=%#v default=%#v err=%v", explicit, defaulted, err)
	}
	workspaceAlias, err := gate.Decide("exec_command", json.RawMessage(`{"argv":["/usr/bin/true"],"cwd":"/workspace"}`))
	if err != nil || workspaceAlias.Fingerprint != defaulted.Fingerprint {
		t.Fatalf("workspace alias decision=%#v default=%#v err=%v", workspaceAlias, defaulted, err)
	}
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterVisibleChange, err := gate.Decide("exec_command", json.RawMessage(`{"argv":["/usr/bin/true"]}`))
	if err != nil || afterVisibleChange.Fingerprint == defaulted.Fingerprint {
		t.Fatalf("visible change did not invalidate command approval: %#v err=%v", afterVisibleChange, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=first"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterSecretChange, err := gate.Decide("exec_command", json.RawMessage(`{"argv":["/usr/bin/true"]}`))
	if err != nil || afterSecretChange.Fingerprint != afterVisibleChange.Fingerprint {
		t.Fatalf("sensitive path changed command approval: %#v visible=%#v err=%v", afterSecretChange, afterVisibleChange, err)
	}
}

func TestDefaultSecretPathRulesProtectNestedCredentialFiles(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := New("ask", "ask", []string{".env", ".env.*", "**/*credential*"}, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	for _, protected := range []string{".env", "config/.env.production", "secrets/api-credential.txt"} {
		decision, err := gate.Decide("apply_patch", json.RawMessage(`{"path":"`+protected+`","mode":"create","content":"x"}`))
		if err != nil || decision.Effect != Deny || decision.RuleID != "protected_path" {
			t.Fatalf("path %q decision=%#v err=%v", protected, decision, err)
		}
	}
}

func patchHash(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// TestReadOnlyToolsMayReadProtectedPaths 锁定有意设计：ProtectedPaths 只约束
// 写入与命令执行（防篡改），只读工具对同一批路径恒 allow；保密边界由工作区
// 层的 sensitivePath/ignore 负责。
func TestReadOnlyToolsMayReadProtectedPaths(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := New("ask", "ask", []string{"frozen-config.yaml"}, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"list_files", "read_file", "search_text"} {
		decision, err := gate.Decide(tool, json.RawMessage(`{"path":"frozen-config.yaml"}`))
		if err != nil || decision.Effect != Allow || decision.RuleID != "read_only" {
			t.Fatalf("%s on protected path: decision = %#v, %v", tool, decision, err)
		}
	}
	denied, err := gate.Decide("apply_patch", json.RawMessage(`{"path":"frozen-config.yaml","mode":"create","content":"x"}`))
	if err != nil || denied.Effect != Deny || denied.RuleID != "protected_path" {
		t.Fatalf("write to protected path: decision = %#v, %v", denied, err)
	}
}

// TestExecSummaryQuotesEachArgument 锁定审批摘要的 argv 边界渲染：恶意实参
// 不能伪装成多条命令或无害展示文本。
func TestExecSummaryQuotesEachArgument(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"argv":["printf","next: git push --force; rm -rf x"]}`)
	decision, err := gate.Decide("exec_command", raw)
	if err != nil {
		t.Fatal(err)
	}
	want := `execute ["printf" "next: git push --force; rm -rf x"]`
	if decision.Summary != want {
		t.Fatalf("summary = %q, want %q", decision.Summary, want)
	}
}

// TestGateProtectsPathAliases 锁定写策略为 allow 或 ask 时，逻辑路径与内部链接
// 均不能绕过受保护目标；配置自身是链接时也保护其实际目标。
func TestGateProtectsPathAliases(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"protected", "guarded", "ordinary"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"protected/locked.txt", "ordinary/locked.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for alias, target := range map[string]string{
		"directory-alias":          "protected",
		"file-alias.txt":           "protected/locked.txt",
		"guarded-alias":            "guarded",
		"protected-alias":          "ordinary",
		"root-alias":               ".",
		"protected-file-alias.txt": "ordinary/locked.txt",
		".env.production":          "ordinary/locked.txt",
	} {
		if err := os.Symlink(target, filepath.Join(root, alias)); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, writeMode := range []string{"allow", "ask"} {
		t.Run(writeMode, func(t *testing.T) {
			for _, test := range []struct {
				name      string
				candidate string
				protected string
				update    bool
				allowed   bool
			}{
				{name: "dot", candidate: "./protected/new.txt", protected: "protected"},
				{name: "dotdot", candidate: "a/../protected/new.txt", protected: "protected"},
				{name: "directory-link", candidate: "directory-alias/new.txt", protected: "protected"},
				{name: "file-link", candidate: "file-alias.txt", protected: "protected/locked.txt", update: true},
				{name: "configured-directory-link", candidate: "ordinary/new.txt", protected: "protected-alias"},
				{name: "configured-root-link-file", candidate: "root-file.txt", protected: "root-alias"},
				{name: "configured-root-link-directory", candidate: "ordinary/new.txt", protected: "root-alias"},
				{name: "configured-file-link", candidate: "ordinary/locked.txt", protected: "protected-file-alias.txt", update: true},
				{name: "logical-link", candidate: "protected-alias/new.txt", protected: "protected-alias"},
				{name: "dot-logical-link", candidate: "./protected-alias/new.txt", protected: "protected-alias"},
				{name: "configured-dot", candidate: "protected/new.txt", protected: "./protected"},
				{name: "configured-dotdot", candidate: "protected/new.txt", protected: "a/../protected"},
				{name: "dot-glob", candidate: "./guarded/new.txt", protected: "guarded/*"},
				{name: "dotdot-glob", candidate: "a/../guarded/new.txt", protected: "guarded/*"},
				{name: "resolved-glob", candidate: "guarded-alias/new.txt", protected: "guarded/*"},
				{name: "logical-glob", candidate: "./guarded-alias/new.txt", protected: "guarded-alias/*"},
				{name: "logical-secret-glob", candidate: "./.env.production", protected: ".env.*", update: true},
				{name: "unprotected", candidate: "./ordinary/new.txt", protected: "protected", allowed: true},
				{name: "sibling-prefix", candidate: "protected-sibling.txt", protected: "protected", allowed: true},
				{name: "unprotected-link", candidate: "protected-alias/new.txt", protected: "protected", allowed: true},
				{name: "unmatched-glob", candidate: "ordinary/new.txt", protected: "guarded/*", allowed: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					gate, err := New(writeMode, "ask", []string{test.protected}, tools.ApplyPatch{Workspace: workspace})
					if err != nil {
						t.Fatal(err)
					}
					content := "new"
					arguments := tools.PatchArguments{Path: test.candidate, Mode: "create", ExpectedSHA256: "absent", Content: &content}
					if test.update {
						arguments.Mode = "update"
						arguments.ExpectedSHA256 = patchHash([]byte("old"))
						arguments.Content = nil
						arguments.Replacements = []tools.Replacement{{Old: "old", New: "new"}}
					}
					raw, err := json.Marshal(arguments)
					if err != nil {
						t.Fatal(err)
					}
					decision, err := gate.Decide("apply_patch", raw)
					wantEffect, wantRule := Deny, "protected_path"
					if test.allowed {
						wantEffect, wantRule = Effect(writeMode), "workspace_write"
					}
					if err != nil || decision.Effect != wantEffect || decision.RuleID != wantRule || decision.Fingerprint == "" {
						t.Fatalf("candidate %q with protected %q: decision=%#v err=%v; want %s/%s", test.candidate, test.protected, decision, err, wantEffect, wantRule)
					}
				})
			}
		})
	}
}

// TestGateClassifiesPatchRecoveryWithoutGrantingWrite 保持拒绝及指纹，并区分形状、匹配、过期摘要和路径保护。
func TestGateClassifiesPatchRecoveryWithoutGrantingWrite(t *testing.T) {
	root := t.TempDir()
	body := []byte("if ready:\n\treturn result\n")
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := New("allow", "allow", []string{"protected.txt"}, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		raw  json.RawMessage
		code string
		rule string
	}{
		{name: "missing path", raw: json.RawMessage(`{"content":"replacement"}`), code: "invalid_patch_arguments", rule: "invalid_patch"},
		{name: "mixed update", raw: json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"` + patchHash(body) + `","content":"replacement","replacements":[{"old":"ready","new":"changed"}]}`), code: "invalid_patch_arguments", rule: "invalid_patch"},
		{name: "text mismatch", raw: json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"` + patchHash(body) + `","replacements":[{"old":"if ready:\n    return result\n","new":"changed"}]}`), code: "patch_text_mismatch", rule: "invalid_patch"},
		{name: "stale first", raw: json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"` + patchHash([]byte("stale")) + `","replacements":[{"old":"missing","new":"changed"}]}`), code: "stale_precondition", rule: "stale_patch_precondition"},
		{name: "protected first", raw: json.RawMessage(`{"path":"protected.txt","mode":"update","content":"replacement"}`), rule: "protected_path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision, err := gate.Decide("apply_patch", test.raw)
			if err != nil || decision.Effect != Deny || decision.FailureCode != test.code || decision.RuleID != test.rule || decision.Fingerprint == "" {
				t.Fatalf("恢复分类改变了拒绝边界: %#v %v", decision, err)
			}
			second, err := gate.Decide("apply_patch", test.raw)
			if err != nil || second.Fingerprint != decision.Fingerprint {
				t.Fatal("失败恢复反馈扰动了同操作指纹")
			}
			if test.code != "" && (!json.Valid(decision.FailureData) || !strings.Contains(string(decision.FailureData), "next_action")) {
				t.Fatal("缺少结构化恢复动作")
			}
			if strings.Contains(string(decision.FailureData), "return result") {
				t.Error("Policy 恢复信息泄漏文件正文")
			}
			actual, err := os.ReadFile(path)
			if err != nil || string(actual) != string(body) {
				t.Fatalf("规划拒绝改变了文件: %v", err)
			}
		})
	}
}
