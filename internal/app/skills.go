// Package app 装配技能目录与显式本机管理用例，不连接 Provider 或执行技能代码。
// Author: fxbin
package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/tools"
	"github.com/fxbin/cairn/internal/userdir"
)

// SkillStoreFor 解析原生安装目录；工作区目标先经过 Workspace.Resolve 防止逃逸。
func (service Service) SkillStoreFor(workspace, scope string) (tools.SkillStore, error) {
	if scope == "user" {
		path, err := userdir.Path("skills")
		if err != nil {
			return tools.SkillStore{}, errors.New("resolve user skill directory failed")
		}
		return tools.SkillStore{Root: path}, nil
	}
	if scope != "workspace" {
		return tools.SkillStore{}, errors.New("skill scope must be workspace or user")
	}
	view, err := tools.NewWorkspace(workspace, nil, 0, 0, 0)
	if err != nil {
		return tools.SkillStore{}, err
	}
	path, _, err := view.Resolve(filepath.Join(".agent", "skills"))
	if err != nil {
		return tools.SkillStore{}, errors.New("workspace skill directory escapes workspace")
	}
	return tools.SkillStore{Root: path}, nil
}

// Skills 以项目原生、项目共享、用户原生、用户共享的固定优先级读取目录。
func (service Service) Skills(workspace string, shared bool) (*tools.SkillCatalog, error) {
	project, err := service.SkillStoreFor(workspace, "workspace")
	if err != nil {
		return nil, err
	}
	userRoot, err := userdir.Path("skills")
	if err != nil {
		return nil, err
	}
	roots := []tools.SkillRoot{{Path: project.Root, Scope: "workspace"}}
	if shared {
		view, err := tools.NewWorkspace(workspace, nil, 0, 0, 0)
		if err != nil {
			return nil, err
		}
		path, _, err := view.Resolve(filepath.Join(".agents", "skills"))
		if err != nil {
			return nil, errors.New("shared workspace skill directory escapes workspace")
		}
		roots = append(roots, tools.SkillRoot{Path: path, Scope: "workspace-shared"})
	}
	roots = append(roots, tools.SkillRoot{Path: userRoot, Scope: "user"})
	if shared {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("resolve shared user skill directory failed")
		}
		roots = append(roots, tools.SkillRoot{Path: filepath.Join(home, ".agents", "skills"), Scope: "user-shared", AllowLinkedPackages: true})
	}
	return tools.DiscoverSkills(roots)
}

// configuredSkillTool 只在配置明确启用时装配内置技能工具，未启用不扫描用户目录。
func (service Service) configuredSkillTool(workspace string, cfg config.Config) (*tools.Skill, error) {
	if cfg.Skills == nil || !cfg.Skills.Enabled {
		return nil, nil
	}
	catalog, err := service.SkillsWithConfig(workspace, *cfg.Skills)
	if err != nil {
		return nil, err
	}
	return &tools.Skill{Catalog: catalog}, nil
}

// SkillsWithConfig 按配置装配技能根：原生/共享之外，可选 skillshare 默认源
// 与 extra_dirs（只读、允许符号链接包）。目录不存在时跳过，不报错。
func (service Service) SkillsWithConfig(workspace string, cfg config.SkillsConfig) (*tools.SkillCatalog, error) {
	roots := make([]tools.SkillRoot, 0, 6)
	project, err := service.SkillStoreFor(workspace, "workspace")
	if err != nil {
		return nil, err
	}
	roots = append(roots, tools.SkillRoot{Path: project.Root, Scope: "workspace"})
	if cfg.Shared {
		view, err := tools.NewWorkspace(workspace, nil, 0, 0, 0)
		if err != nil {
			return nil, err
		}
		path, _, err := view.Resolve(filepath.Join(".agents", "skills"))
		if err != nil {
			return nil, errors.New("shared workspace skill directory escapes workspace")
		}
		roots = append(roots, tools.SkillRoot{Path: path, Scope: "workspace-shared"})
	}
	userRoot, err := userdir.Path("skills")
	if err == nil {
		roots = append(roots, tools.SkillRoot{Path: userRoot, Scope: "user"})
	}
	if cfg.Shared {
		if home, err := os.UserHomeDir(); err == nil {
			roots = append(roots, tools.SkillRoot{
				Path: filepath.Join(home, ".agents", "skills"), Scope: "user-shared", AllowLinkedPackages: true,
			})
		}
	}
	if cfg.Skillshare {
		if path, err := skillshareSkillsDir(); err == nil {
			if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
				roots = append(roots, tools.SkillRoot{Path: path, Scope: "skillshare", AllowLinkedPackages: true})
			}
		}
	}
	for _, dir := range cfg.ExtraDirs {
		path := strings.TrimSpace(dir)
		if path == "" {
			continue
		}
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			roots = append(roots, tools.SkillRoot{Path: path, Scope: "external", AllowLinkedPackages: true})
		}
	}
	return tools.DiscoverSkills(roots)
}

// skillshareSkillsDir 返回 skillshare 默认 skills 源目录。
func skillshareSkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// macOS/Linux 约定；Windows 可用 extra_dirs 显式配置。
	return filepath.Join(home, ".config", "skillshare", "skills"), nil
}
