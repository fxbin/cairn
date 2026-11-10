// Package tools 从用户配置目录与显式工作区内有界发现 AGENTS.md；快照的数据
// 合同、校验与回放归合同层，本文件只负责文件系统读取与脱敏。
// Author: fxbin
package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fxbin/cairn/internal/contract"
)

const (
	// projectInstructionsGitIgnoreMaxBytes 限制读取 .gitignore 的字节数；超长时
	// 只按前缀解析已读到的部分。
	projectInstructionsGitIgnoreMaxBytes = 64 * 1024
)

// LoadProjectInstructions 只发现显式工作区内可见的 AGENTS.md；不遍历目录链接或运行态目录，也不解释正文命令。
func LoadProjectInstructions(workspace Workspace) (snapshot contract.ProjectInstructions, err error) {
	return LoadProjectInstructionsWithUser(workspace, "")
}

// LoadProjectInstructionsWithUser 仅读取指定用户配置目录中的单个 AGENTS.md，再有界发现工作区来源；空目录参数禁用用户来源。
func LoadProjectInstructionsWithUser(workspace Workspace, userDirectory string) (snapshot contract.ProjectInstructions, err error) {
	snapshot.Version = contract.ProjectInstructionsVersion
	sourceBytes := 0
	if userDirectory != "" {
		document, readBytes, loadErr := readUserProjectInstruction(userDirectory)
		if loadErr != nil {
			return snapshot, fmt.Errorf("load user AGENTS.md instructions: %w", loadErr)
		}
		sourceBytes = readBytes
		if document.Content != "" {
			snapshot.Documents = append(snapshot.Documents, document)
		}
	}
	root, err := os.OpenRoot(workspace.Root)
	if err != nil {
		return snapshot, errors.New("open project instructions workspace failed")
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	entries := 0
	// 工作区 .gitignore 声明为本地材料的顶层目录不属于项目源码：其中的
	// AGENTS.md 是别的项目（或别的任务）的规约，注入进来既污染上下文，也会
	// 把无关规约的正文计入本次运行的指令预算。
	localMaterial := projectInstructionsLocalMaterialDirs(workspace)
	var visit func(string) error
	visit = func(relative string) error {
		_, resolved, resolveErr := workspace.Resolve(relative)
		if resolveErr != nil || projectInstructionsIgnored(workspace, resolved) {
			return errors.New("project instructions directory is unavailable")
		}
		directory, openErr := root.Open(resolved)
		if openErr != nil {
			return errors.New("open project instructions directory failed")
		}
		children, readErr := directory.ReadDir(contract.ProjectInstructionsMaxEntries - entries + 1)
		closeErr := directory.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
			return errors.New("read project instructions directory failed")
		}
		entries += len(children)
		if entries > contract.ProjectInstructionsMaxEntries {
			return fmt.Errorf("project instructions discovery exceeds %d entries", contract.ProjectInstructionsMaxEntries)
		}
		for _, child := range children {
			candidate := path.Join(relative, child.Name())
			if projectInstructionsIgnored(workspace, candidate) {
				continue
			}
			if child.IsDir() && relative == "." && localMaterial[child.Name()] {
				continue
			}
			if child.Name() == contract.ProjectInstructionsFilename {
				document, readBytes, loadErr := readProjectInstruction(root, workspace, candidate, contract.ProjectInstructionsMaxBytes-sourceBytes)
				if loadErr != nil {
					return fmt.Errorf("load project instructions %q: %w", string(contract.Redact([]byte(candidate))), loadErr)
				}
				sourceBytes += readBytes
				if document.Content == "" {
					continue
				}
				snapshot.Documents = append(snapshot.Documents, document)
				if len(snapshot.Documents) > contract.ProjectInstructionsMaxDocuments {
					return fmt.Errorf("project instructions exceed %d documents", contract.ProjectInstructionsMaxDocuments)
				}
			} else if child.IsDir() {
				if visitErr := visit(candidate); visitErr != nil {
					return visitErr
				}
			}
		}
		return nil
	}
	if err := visit("."); err != nil {
		return contract.ProjectInstructions{}, err
	}
	sort.Slice(snapshot.Documents, func(left, right int) bool {
		return contract.ProjectInstructionDocumentLess(snapshot.Documents[left], snapshot.Documents[right])
	})
	return snapshot, snapshot.Validate()
}

// readUserProjectInstruction 缺省文件不产生规约；存在的文件复用受限根读取，禁止链接逃出用户配置目录或读取凭据。
func readUserProjectInstruction(directory string) (document contract.ProjectInstruction, readBytes int, err error) {
	if _, statErr := os.Lstat(filepath.Join(directory, contract.ProjectInstructionsFilename)); errors.Is(statErr, os.ErrNotExist) {
		return document, 0, nil
	} else if statErr != nil {
		return document, 0, errors.New("inspect user instructions source failed")
	}
	workspace, err := NewWorkspace(directory, []string{"keys.env"}, 0, 0, 0)
	if err != nil {
		return document, 0, errors.New("open user instructions directory failed")
	}
	root, err := os.OpenRoot(workspace.Root)
	if err != nil {
		return document, 0, errors.New("open user instructions root failed")
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	document, readBytes, err = readProjectInstruction(root, workspace, contract.ProjectInstructionsFilename, contract.ProjectInstructionsMaxBytes)
	if err == nil && document.Content != "" {
		document.Source = contract.ProjectInstructionUser
	}
	return document, readBytes, err
}

// projectInstructionsLocalMaterialDirs 读取工作区根的 .gitignore，收集其中声明
// 的顶层本地材料目录（形如 "/benchmarks/" 的根锚定目录模式）。只识别这种最简
// 形式：其余 gitignore 语法一律不解释，宁可少跳过也不误判项目源码。被忽略的
// 本地材料不属于项目源码，其中的 AGENTS.md 不作为本项目的规约。
func projectInstructionsLocalMaterialDirs(workspace Workspace) map[string]bool {
	names := map[string]bool{}
	content, err := os.ReadFile(filepath.Join(workspace.Root, ".gitignore"))
	if err != nil {
		return names
	}
	if len(content) > projectInstructionsGitIgnoreMaxBytes {
		content = content[:projectInstructionsGitIgnoreMaxBytes]
	}
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(trimmed, "/") || !strings.HasSuffix(trimmed, "/") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(trimmed, "/"), "/")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		names[name] = true
	}
	return names
}

// projectInstructionsIgnored 复用工具可见性，并排除仅供 Cairn 协作留证的运行态目录。
func projectInstructionsIgnored(workspace Workspace, relative string) bool {
	if workspace.Ignore(relative) {
		return true
	}
	for _, segment := range strings.Split(filepath.ToSlash(relative), "/") {
		switch segment {
		case ".git", ".agent", ".cairn", ".vidt", "node_modules", "vendor":
			return true
		}
	}
	return false
}

// readProjectInstruction 在工作区解析和根句柄双重约束内有界读取普通文件，返回实际原文字节数；链接目标同样受忽略规则约束。
func readProjectInstruction(root *os.Root, workspace Workspace, relative string, remaining int) (document contract.ProjectInstruction, readBytes int, err error) {
	if !contract.ValidProjectInstructionPath(relative) {
		return document, 0, errors.New("invalid project instructions source path")
	}
	_, resolved, err := workspace.Resolve(relative)
	if err != nil || projectInstructionsIgnored(workspace, resolved) {
		return document, 0, errors.New("project instructions source escapes visible workspace")
	}
	info, err := root.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return document, 0, errors.New("project instructions source must be a readable regular file")
	}
	file, err := root.Open(resolved)
	if err != nil {
		return document, 0, errors.New("open project instructions source failed")
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return document, 0, errors.New("project instructions source is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, int64(remaining)+1))
	if err != nil {
		return document, 0, errors.New("read project instructions source failed")
	}
	if len(raw) > remaining {
		return document, 0, fmt.Errorf("project instructions exceed combined limit of %d bytes", contract.ProjectInstructionsMaxBytes)
	}
	if !contract.ValidProjectInstructionText(string(raw)) {
		return document, 0, errors.New("project instructions must contain UTF-8 Markdown without control characters")
	}
	if strings.TrimSpace(string(raw)) == "" {
		return document, len(raw), nil
	}
	content := string(contract.Redact(raw))
	return contract.ProjectInstruction{Path: relative, Scope: path.Dir(relative), SHA256: contract.ProjectInstructionDigest([]byte(content)), SourceSHA256: contract.ProjectInstructionDigest(raw), Content: content}, len(raw), nil
}
