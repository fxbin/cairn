// task_handoff_test.go 回归任务交接开关：未配置不改冻结指纹、显式开启改变指纹、
// 非法取值失败关闭、YAML 键可加载，以及模式对场景的覆盖关系。
package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTaskHandoffAbsentKeepsFingerprintAndModesChangeIt 锁定任务交接未配置时不改冻结
// 指纹，也不在指纹视图里留下空字段；三种合法模式各自改变指纹且互不相同。
func TestTaskHandoffAbsentKeepsFingerprintAndModesChangeIt(t *testing.T) {
	base := validDefaults()
	without := base.Fingerprint()
	encodedAbsent, err := json.Marshal(base.Context)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encodedAbsent, []byte(`"task_handoff"`)) {
		t.Fatal("absent task handoff must not add a null field to the JSON fingerprint view")
	}
	seen := map[string]bool{without: true}
	for _, mode := range []TaskHandoffMode{TaskHandoffRework, TaskHandoffAssignment, TaskHandoffAll} {
		enabled := base
		enabled.Context.TaskHandoff = &TaskHandoffConfig{Mode: mode}
		if err := Validate(enabled); err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		fingerprint := enabled.Fingerprint()
		if seen[fingerprint] {
			t.Fatalf("mode %q must produce a fingerprint different from the others", mode)
		}
		seen[fingerprint] = true
	}
	if base.Fingerprint() != without {
		t.Fatal("building other configurations must not mutate the base one")
	}
}

// TestTaskHandoffRejectsUnknownOrMissingMode 锁定开关的封闭取值：写了这段配置就必须
// 明确给出三种模式之一，空值、拼写变体与“off”一律拒绝。
func TestTaskHandoffRejectsUnknownOrMissingMode(t *testing.T) {
	for _, mode := range []TaskHandoffMode{"", "off", "always", "REWORK", "Rework"} {
		configuration := validDefaults()
		configuration.Context.TaskHandoff = &TaskHandoffConfig{Mode: mode}
		err := Validate(configuration)
		if err == nil || !strings.Contains(err.Error(), "context.task_handoff.mode") {
			t.Fatalf("mode %q = %v, want a validation error naming the field", mode, err)
		}
	}
}

// TestTaskHandoffLoadsFromYAMLAndRejectsBadModeThere 锁定 YAML 键名与加载路径上的校验。
func TestTaskHandoffLoadsFromYAMLAndRejectsBadModeThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := validProviderYAML + "context:\n  task_handoff:\n    mode: rework\n"
	if err := os.WriteFile(path, []byte("version: 2\nprovider:\n"+body), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Context.TaskHandoff == nil || loaded.Context.TaskHandoff.Mode != TaskHandoffRework {
		t.Fatalf("task handoff = %+v", loaded.Context.TaskHandoff)
	}
	if err := os.WriteFile(path, []byte("version: 2\nprovider:\n"+validProviderYAML+"context:\n  task_handoff:\n    mode: sometimes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, Overrides{}); err == nil || !strings.Contains(err.Error(), "context.task_handoff.mode") {
		t.Fatalf("unknown mode in YAML = %v", err)
	}
	if _, err := Load("", Overrides{BaseURL: "https://provider.test/v1", Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
}

// TestTaskHandoffModeCoversTargets 锁定模式与场景的对应关系。
func TestTaskHandoffModeCoversTargets(t *testing.T) {
	cases := []struct {
		mode   TaskHandoffMode
		target TaskHandoffMode
		want   bool
	}{
		{TaskHandoffRework, TaskHandoffRework, true},
		{TaskHandoffRework, TaskHandoffAssignment, false},
		{TaskHandoffAssignment, TaskHandoffAssignment, true},
		{TaskHandoffAssignment, TaskHandoffRework, false},
		{TaskHandoffAll, TaskHandoffRework, true},
		{TaskHandoffAll, TaskHandoffAssignment, true},
	}
	for _, testCase := range cases {
		if got := testCase.mode.Covers(testCase.target); got != testCase.want {
			t.Fatalf("mode %q covers %q = %t, want %t", testCase.mode, testCase.target, got, testCase.want)
		}
	}
}
