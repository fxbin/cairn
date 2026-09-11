// reference.go 实现任务输入的 @ 文件引用展开（T3.1）：提交前把任务文本中
// 的 @相对路径 解析为工作区边界内的文件内容并附加到任务原文。引用展开是
// 用户显式行为，展开后的完整文本如实进入任务事件（审计可回放）；解析越
// 界、缺失或超限时失败关闭，提示后留在输入态由用户修正。
//
// Author: fxbin
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fxbin/cairn/internal/tools"
)

// referencePattern 匹配任务文本中的 @引用 token：@ 后跟路径字符序列，前
// 面不能是路径或文字字符（避免匹配邮箱与普通单词内的 @）。
var referencePattern = regexp.MustCompile(`(?:^|\s)@([^\s@]+)`)

// referencePreviewLimit 是单个引用文件附加到任务的字节上限；超限截断并
// 标注总量，完整内容模型可经 read_file 工具获取。
const referencePreviewLimit = 8192

// expandTaskReferences 解析并展开任务文本中的 @ 文件引用：每个引用经工作
// 区解析（边界内、拒绝绝对路径与越界符号链接）、读取并截断到上限后按
// 引用块附加到任务原文末尾。无引用时原文原样返回。
func expandTaskReferences(task string, workspace tools.Workspace) (string, error) {
	matches := referencePattern.FindAllStringSubmatch(task, -1)
	if len(matches) == 0 {
		return task, nil
	}
	var blocks []string
	seen := make(map[string]bool)
	for _, match := range matches {
		reference := match[1]
		if seen[reference] {
			continue
		}
		seen[reference] = true
		resolved, _, err := workspace.Resolve(reference)
		if err != nil {
			return "", fmt.Errorf("引用 @%s 越出工作区或不可解析: %w", reference, err)
		}
		content, err := os.ReadFile(resolved)
		if err != nil {
			return "", fmt.Errorf("引用 @%s 读取失败: %w", reference, err)
		}
		blocks = append(blocks, referenceBlock(reference, content))
	}
	return task + "\n\n" + strings.Join(blocks, "\n"), nil
}

// referenceBlock 生成单个引用的附加块：路径头 + 截断内容；截断时标注总量
// 并提示以 read_file 获取完整内容。
func referenceBlock(reference string, content []byte) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "[引用文件 @%s]\n", reference)
	if len(content) > referencePreviewLimit {
		builder.Write(content[:referencePreviewLimit])
		fmt.Fprintf(&builder, "\n…（已截断，共 %d 字节；完整内容请用 read_file 读取 %s）", len(content), reference)
	} else {
		builder.Write(content)
	}
	return builder.String()
}

// workspaceForRoot 为引用解析构造工具层工作区视图；忽略清单与读取上限与
// 生产装配的安全默认一致。
func workspaceForRoot(root string) (tools.Workspace, error) {
	return tools.NewWorkspace(root, nil, referencePreviewLimit*4, 512, 1<<20)
}

// workspaceRootOrDefault 把可能为空的工作区参数解析为当前目录。
func workspaceRootOrDefault(workspacePath string) string {
	if workspacePath == "" {
		if current, err := os.Getwd(); err == nil {
			return current
		}
		return "."
	}
	return filepath.Clean(workspacePath)
}
