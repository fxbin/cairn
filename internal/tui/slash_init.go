// Package tui 的 /init 命令：在工作区原子写入 AGENTS.md 项目规约骨架，
// 已存在时只读展示摘要并拒绝覆盖；不解释正文命令。
// Author: fxbin
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/tools"
)

// initAGENTSDefault 是 /init 生成的最小项目规约骨架，不含密钥或路径。
const initAGENTSDefault = `# 项目协作规约

## 沟通
- 使用中文沟通、编写注释与提交说明。

## 边界
- 不提交密钥、凭据与本地状态目录。
- 运行、修改前先说明范围。

## 验证
- 变更后运行相关测试并记录实际检查项。
`

// initResult 描述 /init 的可展示结果。
type initResult struct {
	Created  bool
	Path     string
	Existing bool
	Bytes    int
}

// runInitCommand 在工作区写入 AGENTS.md 骨架；文件已存在则拒绝覆盖。
func runInitCommand(workspace string) (initResult, error) {
	root := workspace
	if root == "" {
		current, err := os.Getwd()
		if err != nil {
			return initResult{}, fmt.Errorf("resolve workspace: %w", err)
		}
		root = current
	}
	ws, err := tools.NewWorkspace(root, nil, 0, 0, 0)
	if err != nil {
		return initResult{}, fmt.Errorf("workspace: %w", err)
	}
	target, _, err := ws.Resolve("AGENTS.md")
	if err != nil {
		return initResult{}, fmt.Errorf("resolve AGENTS.md: %w", err)
	}
	if info, statErr := os.Lstat(target); statErr == nil {
		if !info.Mode().IsRegular() {
			return initResult{}, fmt.Errorf("AGENTS.md exists and is not a regular file")
		}
		return initResult{Existing: true, Path: target, Bytes: int(info.Size())}, nil
	} else if !os.IsNotExist(statErr) {
		return initResult{}, fmt.Errorf("stat AGENTS.md: %w", statErr)
	}
	if !strings.HasSuffix(target, "AGENTS.md") {
		return initResult{}, fmt.Errorf("unexpected AGENTS.md path")
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".agents-*.tmp")
	if err != nil {
		return initResult{}, fmt.Errorf("create temp: %w", err)
	}
	tempName := temporary.Name()
	defer os.Remove(tempName)
	content := []byte(initAGENTSDefault)
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return initResult{}, fmt.Errorf("write temp: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return initResult{}, fmt.Errorf("sync temp: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return initResult{}, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tempName, target); err != nil {
		return initResult{}, fmt.Errorf("publish AGENTS.md: %w", err)
	}
	return initResult{Created: true, Path: target, Bytes: len(content)}, nil
}
