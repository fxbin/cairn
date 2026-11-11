package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProjectInstructionsFixture 在临时目录里建立工作区骨架，返回工作区根。
func writeProjectInstructionsFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestProjectInstructionsSkipGitIgnoredLocalMaterial 锁定发现边界：工作区
// .gitignore 声明为本地材料的目录不属于项目源码，其中的 AGENTS.md 不得作为
// 本项目的规约被注入。
func TestProjectInstructionsSkipGitIgnoredLocalMaterial(t *testing.T) {
	root := writeProjectInstructionsFixture(t, map[string]string{
		".gitignore":                      "# 本地研究材料\n/benchmarks/\n/docs/\n",
		"AGENTS.md":                       "# 项目规约\n",
		"benchmarks/foreign/AGENTS.md":    "# 别的项目的规约\n",
		"docs/decisions/AGENTS.md":        "# 文档目录里的规约\n",
		"src/AGENTS.md":                   "# 子目录规约\n",
		"src/deep/nested/AGENTS.md":       "# 深层规约\n",
		"notignored/AGENTS.md":            "# 普通目录规约\n",
		"benchmarks/foreign/CLAUDE.md":    "# 无关文件\n",
		"benchmarks/README.md":            "# 无关文件\n",
		"benchmarks/foreign/sub/AGENT.md": "# 无关文件\n",
	})
	workspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadProjectInstructions(workspace)
	if err != nil {
		t.Fatalf("加载项目规约失败: %v", err)
	}
	paths := make([]string, 0, len(snapshot.Documents))
	for _, document := range snapshot.Documents {
		paths = append(paths, document.Path)
	}
	joined := strings.Join(paths, ",")
	for _, forbidden := range []string{"benchmarks", "docs"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("本地材料目录 %s 的规约被当作项目规约注入: %v", forbidden, paths)
		}
	}
	for _, expected := range []string{"AGENTS.md", "src/AGENTS.md", "src/deep/nested/AGENTS.md", "notignored/AGENTS.md"} {
		found := false
		for _, path := range paths {
			if path == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("项目规约 %s 未被发现: %v", expected, paths)
		}
	}
}

// TestProjectInstructionsIgnoreLocalMaterialWithoutGitignore 锁定无 .gitignore
// 时的既有行为：发现范围不受影响，仍受运行态目录名单约束。
func TestProjectInstructionsIgnoreLocalMaterialWithoutGitignore(t *testing.T) {
	root := writeProjectInstructionsFixture(t, map[string]string{
		"AGENTS.md":               "# 项目规约\n",
		"benchmarks/AGENTS.md":    "# 别的目录规约\n",
		".vidt/scratch/AGENTS.md": "# 运行态目录规约\n",
	})
	workspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadProjectInstructions(workspace)
	if err != nil {
		t.Fatalf("加载项目规约失败: %v", err)
	}
	for _, document := range snapshot.Documents {
		if strings.HasPrefix(document.Path, ".vidt/") {
			t.Fatalf("运行态目录的规约被注入: %s", document.Path)
		}
	}
	if len(snapshot.Documents) != 2 {
		t.Fatalf("规约数量 = %d，期望 2（根规约与 benchmarks 规约）", len(snapshot.Documents))
	}
}
