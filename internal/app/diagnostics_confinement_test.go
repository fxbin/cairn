package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// TestDoctorReportsConfinementReasonBeforeCommandsAreRefused 锁定 doctor 在 host 收紧
// 档位下提前给出沙箱规则无法生成的原因码：默认环境为空，临时目录过宽时为
// temp_dir_rejected，报告里只多出原因码而没有路径，就绪判定不因此改变。
func TestDoctorReportsConfinementReasonBeforeCommandsAreRefused(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Sandbox.Backend = "host"
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	workspacePath, stateDir := t.TempDir(), t.TempDir()

	report, err := service.Doctor(context.Background(), workspacePath, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Sandbox.HostConfined {
		if report.Sandbox.ConfinementReason != "" {
			t.Fatalf("confinement reason = %q without host confinement", report.Sandbox.ConfinementReason)
		}
		return
	}
	if report.Sandbox.ConfinementReason != "" || report.Sandbox.ConfinementAdvice() != "" {
		t.Fatalf("default environment must derive sandbox rules, reason = %q", report.Sandbox.ConfinementReason)
	}
	readyBefore := report.Ready

	t.Setenv("TMPDIR", string(filepath.Separator))
	rejected, err := service.Doctor(context.Background(), workspacePath, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Sandbox.ConfinementReason != tools.SandboxConfinementTempDir {
		t.Fatalf("confinement reason = %q, want %q", rejected.Sandbox.ConfinementReason, tools.SandboxConfinementTempDir)
	}
	if rejected.Ready != readyBefore {
		t.Fatalf("confinement reason must not change readiness: before=%t after=%t", readyBefore, rejected.Ready)
	}
	encoded, err := json.Marshal(rejected.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"backend":"host","host_confined":true,"seatbelt_available":true,"confinement_reason":"temp_dir_rejected"}`; string(encoded) != want {
		t.Fatalf("sandbox health = %s, want %s", encoded, want)
	}
}
