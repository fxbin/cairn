//go:build darwin || linux

// sandbox_confinement_test.go 锁定沙箱规则无法生成时的稳定原因码：错误文本保持原样，
// 只读推导与真实启动给出同一个原因码。只在有原生沙箱的平台上编译。
// Author: fxbin
package tools

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxConfinementReasonOfClassifiesErrors 锁定归类的兜底：没有错误时为空串，
// 未附原因码的错误按推导失败处理，缺少原生沙箱的错误归为不可用。
func TestSandboxConfinementReasonOfClassifiesErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want SandboxConfinementReason
	}{
		{name: "no error", want: ""},
		{name: "os sandbox unavailable", err: errOSSandboxUnavailable, want: SandboxConfinementUnavailable},
		{name: "unclassified error", err: errors.New("read root directory"), want: SandboxConfinementPlanFailed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := sandboxConfinementReasonOf(testCase.err); got != testCase.want {
				t.Fatalf("reason = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestSandboxTempRootsRejectionsCarryConfinementReason 锁定临时根被拒绝的错误带原因码，
// 且错误文本仍以命令结果里看到的形式出现。
func TestSandboxTempRootsRejectionsCarryConfinementReason(t *testing.T) {
	cases := []struct {
		name  string
		input sandboxTempInput
		scope sandboxTempScope
	}{
		{name: "filesystem root", input: fixedSandboxTempInput("/", "/Users/example", identityResolve), scope: noWorkspace},
		{name: "covers workspace in read-only profile", input: fixedSandboxTempInput("/Users/example/projects", "/Users/example", identityResolve), scope: sandboxTempScope{WorkspaceRoot: "/Users/example/projects/ws"}},
		{name: "contains home directory", input: fixedSandboxTempInput("/Users", "/Users/example", identityResolve), scope: noWorkspace},
		{name: "home directory unavailable", input: fixedSandboxTempInput("/srv/tmp", "", identityResolve), scope: noWorkspace},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := sandboxTempRoots(fixedTempRootsForTest, testCase.input, testCase.scope)
			if err == nil {
				t.Fatal("expected the temp dir to be rejected")
			}
			if got := sandboxConfinementReasonOf(err); got != SandboxConfinementTempDir {
				t.Fatalf("reason = %q, want %q (error: %v)", got, SandboxConfinementTempDir, err)
			}
			if !strings.HasPrefix(err.Error(), "sandbox temp dir ") {
				t.Fatalf("error text must stay as shown to commands: %v", err)
			}
		})
	}
}

// TestOSSandboxConfinementReasonMatchesRealConfinement 锁定只读推导与真实启动同源：
// 默认环境可以生成规则；过宽的临时目录与缺失的工作区根给出对应原因码，真实启动给出
// 同一个原因码。没有原生沙箱的平台上只做不可用的归类。
func TestOSSandboxConfinementReasonMatchesRealConfinement(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	options := SeatbeltOptions{WorkspaceRoot: workspace.Root, WriteMode: SeatbeltWriteWorkspace}
	if !OSSandboxAvailable() {
		if reason := OSSandboxConfinementReason(options); reason != SandboxConfinementUnavailable {
			t.Fatalf("reason = %q without an OS sandbox", reason)
		}
		return
	}
	t.Run("default environment derives rules", func(t *testing.T) {
		if reason := OSSandboxConfinementReason(options); reason != "" {
			t.Fatalf("default environment must derive sandbox rules, reason = %q", reason)
		}
	})
	t.Run("broad temp dir is rejected before start", func(t *testing.T) {
		t.Setenv("TMPDIR", string(filepath.Separator))
		if reason := OSSandboxConfinementReason(options); reason != SandboxConfinementTempDir {
			t.Fatalf("reason = %q, want %q", reason, SandboxConfinementTempDir)
		}
		_, cleanup, confineErr := ConfineArgv(options, []string{"true"})
		if cleanup != nil {
			_ = cleanup()
		}
		if reason := sandboxConfinementReasonOf(confineErr); reason != SandboxConfinementTempDir {
			t.Fatalf("real confinement reason = %q, want %q (error: %v)", reason, SandboxConfinementTempDir, confineErr)
		}
	})
	t.Run("missing workspace root", func(t *testing.T) {
		if reason := OSSandboxConfinementReason(SeatbeltOptions{WriteMode: SeatbeltWriteWorkspace}); reason != SandboxConfinementWorkspaceRoot {
			t.Fatalf("reason = %q, want %q", reason, SandboxConfinementWorkspaceRoot)
		}
	})
}

// TestSandboxConfinementAdviceCoversEveryReason 锁定每个原因码都有处置提示，提示里不含路径。
func TestSandboxConfinementAdviceCoversEveryReason(t *testing.T) {
	for _, reason := range []SandboxConfinementReason{SandboxConfinementUnavailable, SandboxConfinementWorkspaceRoot, SandboxConfinementTempDir, SandboxConfinementPlanFailed} {
		advice := SandboxConfinementAdvice(reason)
		if advice == "" || strings.Contains(advice, string(filepath.Separator)) {
			t.Fatalf("advice for %q must be present and path-free: %q", reason, advice)
		}
	}
	if SandboxConfinementAdvice("") != "" || SandboxConfinementAdvice("unknown") != "" {
		t.Fatal("no reason means no advice")
	}
}
