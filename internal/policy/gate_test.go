package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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
