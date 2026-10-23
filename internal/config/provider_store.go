// Package config 提供命名连接预设的校验与原子管理，CLI 与桌面共用同一写入边界。
// Author: fxbin
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/lock"
	"gopkg.in/yaml.v3"
)

// SaveProviderPreset 保存显式连接预设；同名替换必须显式授权，非法连接不落盘。
func SaveProviderPreset(path, name string, preset ProviderPreset, overwrite bool) error {
	candidate := Defaults()
	candidate.Provider.Presets = map[string]ProviderPreset{name: preset}
	if err := resolveProviderConnection(&candidate, providerOverlay{Use: &name}, &resolutionRecorder{}); err != nil {
		return err
	}
	if err := Validate(candidate); err != nil {
		return err
	}
	return updateProviderDocument(path, true, func(provider, presets *yaml.Node) error {
		for i := 0; i < len(presets.Content); i += 2 {
			if presets.Content[i].Value != name {
				continue
			}
			if !overwrite {
				return fmt.Errorf("preset %q already exists (use --overwrite)", name)
			}
			return presets.Content[i+1].Encode(preset)
		}
		value := new(yaml.Node)
		if err := value.Encode(preset); err != nil {
			return err
		}
		presets.Content = append(presets.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: name}, value)
		return nil
	})
}

// RemoveProviderPreset 删除未激活预设；当前激活项与缺失项失败关闭。
func RemoveProviderPreset(path, name string) error {
	return updateProviderDocument(path, false, func(provider, presets *yaml.Node) error {
		for i := 0; i < len(provider.Content); i += 2 {
			if provider.Content[i].Value == "use" && provider.Content[i+1].Value == name {
				return fmt.Errorf("preset %q is active (switch first)", name)
			}
		}
		for i := 0; i < len(presets.Content); i += 2 {
			if presets.Content[i].Value == name {
				presets.Content = append(presets.Content[:i], presets.Content[i+2:]...)
				return nil
			}
		}
		return fmt.Errorf("preset %q is not defined in provider.presets", name)
	})
}

// UseProviderPreset 清除内联连接后切换至已命名预设；整个候选配置校验通过才原子发布。
func UseProviderPreset(path, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("preset name is required")
	}
	return updateProviderDocument(path, false, func(provider, presets *yaml.Node) error {
		exists := false
		for i := 0; i < len(presets.Content); i += 2 {
			if presets.Content[i].Value == name {
				exists = true
				break
			}
		}
		if !exists {
			return fmt.Errorf("preset %q is not defined in provider.presets", name)
		}
		kept := make([]*yaml.Node, 0, len(provider.Content))
		for i := 0; i < len(provider.Content); i += 2 {
			switch provider.Content[i].Value {
			case "base_url", "model", "api_key_env", "request_timeout", "id", "admission", "capability", "generation", "protocol", "use":
				continue
			}
			kept = append(kept, provider.Content[i], provider.Content[i+1])
		}
		provider.Content = append(kept, &yaml.Node{Kind: yaml.ScalarNode, Value: "use"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name})
		return nil
	})
}

// providerMapping 获取或补齐配置中的映射节点，拒绝非映射与别名造成的含混更新。
func providerMapping(parent *yaml.Node, key string) (*yaml.Node, error) {
	if parent.Kind != yaml.MappingNode {
		return nil, errors.New("provider config section must be a mapping")
	}
	for i := 0; i < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			value := parent.Content[i+1]
			if value.Kind != yaml.MappingNode {
				return nil, errors.New("provider config section must be a mapping")
			}
			return value, nil
		}
	}
	value := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, value)
	return value, nil
}

// updateProviderDocument 串行执行读改写，以独占临时文件校验并同步后发布；失败保留原文件。
func updateProviderDocument(path string, allowMissing bool, mutate func(*yaml.Node, *yaml.Node) error) (err error) {
	guard, err := lock.Acquire(path + ".lock")
	if err != nil {
		return errors.New("config is locked or unavailable")
	}
	defer func() { err = errors.Join(err, guard.Release()) }()
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		content = []byte("version: 2\nprovider:\n  api_key_env: AGENT_API_KEY\n  base_url: https://api.example.com/v1\n  model: placeholder\n  admission: required\n  id: custom\n")
	} else if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil || len(document.Content) != 1 {
		return errors.New("parse config failed")
	}
	provider, err := providerMapping(document.Content[0], "provider")
	if err != nil {
		return err
	}
	presets, err := providerMapping(provider, "presets")
	if err != nil {
		return err
	}
	if err := mutate(provider, presets); err != nil {
		return err
	}
	encoded, err := yaml.Marshal(&document)
	if err != nil {
		return errors.New("encode config failed")
	}
	mode := os.FileMode(0o600)
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() {
			return errors.New("config must be a regular file")
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return errors.New("create config temporary failed")
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if _, writeErr := temporary.Write(encoded); writeErr != nil {
		_ = temporary.Close()
		return errors.New("write config failed")
	}
	if err := errors.Join(temporary.Sync(), temporary.Close()); err != nil {
		return errors.New("sync config failed")
	}
	if _, err := Load(temporaryName, Overrides{}); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	if err := os.Chmod(temporaryName, mode); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return errors.New("replace config failed")
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
