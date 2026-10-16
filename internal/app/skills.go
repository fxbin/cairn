// Package app 装配技能目录与显式本机管理用例，不连接 Provider 或执行技能代码。
// Author: fxbin
package app

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/tools"
)

// SkillStoreFor 解析原生安装目录；工作区目标先经过 Workspace.Resolve 防止逃逸。
func (service Service) SkillStoreFor(workspace, scope string) (tools.SkillStore, error) {
	if scope == "user" {
		path, err := config.UserConfigPath()
		if err != nil {
			return tools.SkillStore{}, errors.New("resolve user skill directory failed")
		}
		return tools.SkillStore{Root: filepath.Join(filepath.Dir(path), "skills")}, nil
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
	user, err := service.SkillStoreFor(workspace, "user")
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
	roots = append(roots, tools.SkillRoot{Path: user.Root, Scope: "user"})
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
	catalog, err := service.Skills(workspace, cfg.Skills.Shared)
	if err != nil {
		return nil, err
	}
	return &tools.Skill{Catalog: catalog}, nil
}
