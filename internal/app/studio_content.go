// studio_content.go 为 CLI 提供内容包、提示模板、技能摘要与工作区哈希的
// Application 门面；Studio 层不得直接依赖 internal/tools。
// Author: fxbin
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/tools"
)

// NewTemplateStore 构造提示模板存储。
func NewTemplateStore(root string) (tools.TemplateStore, error) {
	return tools.NewTemplateStore(root)
}

// TemplateSummary 返回单条模板的展示摘要。
func TemplateSummary(document tools.TemplateDocument) string {
	return tools.TemplateSummary(document)
}

// RenderTemplate 以显式参数渲染模板，不执行脚本。
func RenderTemplate(document tools.TemplateDocument, values map[string]string) (string, error) {
	return tools.RenderTemplate(document, values)
}

// InspectPackage 只读检查内容包清单，不执行脚本。
func InspectPackage(source string) (tools.PackageManifest, error) {
	return tools.InspectPackage(source)
}

// NewPackageStore 构造内容包存储。
func NewPackageStore(root string) (tools.PackageStore, error) {
	return tools.NewPackageStore(root)
}

// PackageSummary 返回单条内容包的展示摘要。
func PackageSummary(entry tools.PackageEntry) string {
	return tools.PackageSummary(entry)
}

// SkillSummary 返回单条技能的展示摘要。
func SkillSummary(entry tools.SkillEntry) string {
	return tools.SkillSummary(entry)
}

// RunSkillshareAudit 在沙箱边界内审计 skillshare 来源包。
func RunSkillshareAudit(ctx context.Context, name string) (string, error) {
	return tools.RunSkillshareAudit(ctx, name)
}

// WorkspaceHash 复刻装配的工作区哈希算法（符号链接解析后的绝对路径摘要），
// 保证证据与决定分区和 run 门完全一致；与探测落盘共用同一实现。
func WorkspaceHash(workspacePath string, configuration config.Config) (string, error) {
	root := workspacePath
	if root == "" {
		current, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root = current
	}
	workspace, err := tools.NewWorkspace(root, configuration.Workspace.Ignore, configuration.Workspace.MaxReadBytes, configuration.Workspace.MaxSearchFiles, configuration.Workspace.MaxSearchBytes)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(workspace.Root))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
