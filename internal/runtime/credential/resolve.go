// 本文件提供不修改进程环境的本机凭据解析，供长驻桌面进程隔离不同工作区。
//
// Author: fxbin
package credential

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ResolveEnv 按显式环境、调用方给定的文件顺序解析一个变量；文件必须是
// 0600 的普通文件，整份语法验证通过后才返回值，错误中不包含凭据内容。
// 解析不调用 Setenv，因此并发工作区不会相互覆盖或继承文件凭据。
func ResolveEnv(name string, paths ...string) (string, error) {
	if name == "" {
		return "", nil
	}
	if err := ValidateEnvReference(name); err != nil {
		return "", err
	}
	if value := os.Getenv(name); value != "" {
		return value, nil
	}
	for _, path := range paths {
		value, err := resolveEnvFile(path, name)
		if err != nil {
			return "", err
		}
		if value != "" {
			return value, nil
		}
	}
	return "", nil
}

// maxCredentialFileBytes 限制凭据文件内存占用，超限文件失败关闭。
const maxCredentialFileBytes int64 = 1024 * 1024

// resolveEnvFile 验证整份凭据文件并选择首个同名非空条目；缺失文件视为空。
func resolveEnvFile(path, selected string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("cannot inspect credential file")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != keysFileRequiredPerm {
		return "", errors.New("credential file must be a regular file with permission 0600")
	}
	if info.Size() > maxCredentialFileBytes {
		return "", errors.New("credential file exceeds size limit")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("cannot read credential file")
	}
	value := ""
	for index, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, entry, found := strings.Cut(line, "=")
		if !found || !validEnvName(name) {
			return "", fmt.Errorf("credential file line %d is invalid", index+1)
		}
		if name == selected && value == "" {
			value = entry
		}
	}
	return value, nil
}

// ValidateEnvReference 只验证凭据引用的变量名形态；错误不回显误填的密钥正文。
func ValidateEnvReference(name string) error {
	if !validEnvName(name) {
		return errors.New("credential reference must be an uppercase environment variable name, not a key value")
	}
	return nil
}
