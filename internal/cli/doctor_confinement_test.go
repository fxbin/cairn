package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// TestDoctorHumanOutputExplainsRejectedTempDir 锁定 doctor 的人类可读输出在沙箱规则无法
// 生成时给出原因码与处置提示：默认环境没有这一行，临时目录过宽时出现；没有原生沙箱的
// 平台上两种情况都没有这一行。
func TestDoctorHumanOutputExplainsRejectedTempDir(t *testing.T) {
	workspace := t.TempDir()
	configPath := config.DefaultPath(workspace)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	configYAML := "version: 2\nprovider:\n  base_url: https://provider.test/v1\n  model: test-model\n  api_key_env: CAIRN_CLI_CONFINEMENT_KEY\n  capability:\n    tool_calls: true\n    streaming_usage: true\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAIRN_CLI_CONFINEMENT_KEY", "present")
	runDoctor := func(t *testing.T) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		command := Command{Stdout: &stdout, Stderr: &stderr}
		if code := command.Run(context.Background(), []string{"doctor", "--workspace", workspace, "--state-dir", t.TempDir(), "--config", configPath}); code != ExitOK {
			t.Fatalf("doctor exit=%d stderr=%q", code, stderr.String())
		}
		return stdout.String()
	}
	if strings.Contains(runDoctor(t), "sandbox confinement:") {
		t.Fatal("default environment must not report a confinement reason")
	}
	if !tools.OSSandboxAvailable() {
		t.Setenv("TMPDIR", string(filepath.Separator))
		if strings.Contains(runDoctor(t), "sandbox confinement:") {
			t.Fatal("platforms without an OS sandbox do not confine host commands")
		}
		return
	}
	t.Setenv("TMPDIR", string(filepath.Separator))
	output := runDoctor(t)
	for _, required := range []string{"sandbox confinement: " + string(tools.SandboxConfinementTempDir), tools.SandboxConfinementAdvice(tools.SandboxConfinementTempDir)} {
		if !strings.Contains(output, required) {
			t.Fatalf("doctor output missing %q: %s", required, output)
		}
	}
}
