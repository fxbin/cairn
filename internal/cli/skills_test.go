package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillsManagementRedactsLegacyDirectoryNames 管理旧目录仍使用原名称，普通输出及 NDJSON 均不回显凭据式名称。
func TestSkillsManagementRedactsLegacyDirectoryNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, jsonOutput := range []bool{false, true} {
		workspace := t.TempDir()
		name := "sk-" + strings.Repeat("a", 40)
		path := filepath.Join(workspace, ".agent", "skills", name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"disable", "enable", "remove"} {
			var stdout, stderr bytes.Buffer
			args := []string{"skills", action, name, "--workspace", workspace}
			if jsonOutput {
				args = append(args, "--json")
			}
			command := Command{Stdout: &stdout, Stderr: &stderr}
			if exit := command.Run(context.Background(), args); exit != ExitOK {
				t.Fatalf("%s: %d %s", action, exit, stderr.String())
			}
			if strings.Contains(stdout.String()+stderr.String(), name) || !strings.Contains(stdout.String(), "[REDACTED]") {
				t.Fatalf("unsafe output: %q %q", stdout.String(), stderr.String())
			}
			if jsonOutput && !json.Valid(bytes.TrimSpace(stdout.Bytes())) {
				t.Fatal("redaction broke NDJSON")
			}
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("management used redacted operational target")
		}
	}
}

// TestSkillsCLIManagementWithoutProviderConfig 真实管理入口不需要模型或密钥，并保持纯 NDJSON 输出。
func TestSkillsCLIManagementWithoutProviderConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	source := filepath.Join(t.TempDir(), "review-code")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("---\nname: review-code\ndescription: 审查代码\n---\nCLI_SKILL_INSTRUCTIONS"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"install", source}, {"list"}, {"disable", "review-code"}, {"show", "review-code"}, {"enable", "review-code"}, {"remove", "review-code"}} {
		var stdout, stderr bytes.Buffer
		command := Command{Stdout: &stdout, Stderr: &stderr}
		arguments := append([]string{"skills"}, args...)
		arguments = append(arguments, "--workspace", workspace, "--json")
		if code := command.Run(context.Background(), arguments); code != ExitOK {
			t.Fatalf("%v exit=%d %s", args, code, stderr.String())
		}
		lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
		if len(lines) != 1 || !json.Valid([]byte(lines[0])) {
			t.Fatalf("not NDJSON: %q", stdout.String())
		}
		if args[0] == "list" && strings.Contains(stdout.String(), "CLI_SKILL_INSTRUCTIONS") {
			t.Fatal("list eagerly included body")
		}
		if args[0] == "show" && !strings.Contains(stdout.String(), "CLI_SKILL_INSTRUCTIONS") {
			t.Fatal("show omitted disabled skill")
		}
	}
}
