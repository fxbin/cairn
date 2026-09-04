// keysfile 承载 ADR-0015 定义的本机凭据文件：0600 的 KEY=VALUE 文件装载
// 与合并写入。密钥值只在本文件与进程环境间流动，永不进入配置 YAML、事
// 件、制品、遥测或错误信息；错误文本只携带行号与变量名定位，不携带值。
//
// Author: fxbin
package credential

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeysFileName 是工作区内凭据文件的固定相对路径段；文件位于 .agent/ 目
// 录下，与配置文件同目录且同样被 Git 忽略。
const KeysFileName = ".agent/keys.env"

// UserKeysPath 返回用户级凭据文件路径（ADR-0016 装载链的第二层）：工作区
// 凭据缺失时按同名变量补缺，永不覆盖工作区或显式导出的值。
func UserKeysPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cairn", "keys.env"), nil
}

// keysFileRequiredPerm 是凭据文件的强制权限；更宽的权限意味着其他本地账
// 号可读，装载失败关闭。
const keysFileRequiredPerm os.FileMode = 0o600

// keysFileDirPerm 是凭据文件所在目录的权限。
const keysFileDirPerm os.FileMode = 0o700

// LoadEnvFile 把凭据文件装载进进程环境并返回实际写入的变量名清单：已存
// 在的环境变量优先，文件值不覆盖（显式导出 > 文件）。文件不存在视为空装
// 载；权限过宽或行形态非法时整文件拒绝装载并返回不含值的错误。
func LoadEnvFile(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Mode().Perm() != keysFileRequiredPerm {
		return nil, fmt.Errorf("keys file %s has permission %o, requires 0600", path, info.Mode().Perm())
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var loaded []string
	for lineNumber, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("keys file %s line %d: expected NAME=VALUE", path, lineNumber+1)
		}
		if !validEnvName(name) {
			return nil, fmt.Errorf("keys file %s line %d: invalid variable name", path, lineNumber+1)
		}
		if os.Getenv(name) != "" {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			return nil, err
		}
		loaded = append(loaded, name)
	}
	return loaded, nil
}

// UpsertEnvFile 把 name=value 合并写入凭据文件：保留其他行与既有行序、
// 更新同名行、新变量追加到末尾；写入经临时文件与原子重命名，文件与目录
// 权限强制收紧。
func UpsertEnvFile(path, name, value string) error {
	if !validEnvName(name) {
		return fmt.Errorf("invalid variable name %q", name)
	}
	if value == "" || strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("credential value must be non-empty single line")
	}
	if err := os.MkdirAll(filepath.Dir(path), keysFileDirPerm); err != nil {
		return err
	}
	names, entries := readEnvFile(path)
	if _, exists := entries[name]; !exists {
		names = append(names, name)
	}
	entries[name] = value
	var builder strings.Builder
	for _, existing := range names {
		builder.WriteString(existing)
		builder.WriteString("=")
		builder.WriteString(entries[existing])
		builder.WriteString("\n")
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(builder.String()), keysFileRequiredPerm); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// EnvFileHas 报告凭据文件中是否已配置指定变量；文件不存在或不可读时返回
// false，由调用方以"未配置"呈现，不构成安全问题。
func EnvFileHas(path, name string) bool {
	_, entries := readEnvFile(path)
	_, ok := entries[name]
	return ok
}

// readEnvFile 单次读取凭据文件的有效条目：names 保持文件行序，entries 为
// 键值映射（始终非 nil，文件不存在按空文件处理）；装载路径的严格校验在
// LoadEnvFile，写路径对新输入自有校验。
func readEnvFile(path string) ([]string, map[string]string) {
	entries := make(map[string]string)
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, entries
	}
	var names []string
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, value, found := strings.Cut(line, "="); found && validEnvName(name) {
			if _, seen := entries[name]; !seen {
				names = append(names, name)
			}
			entries[name] = value
		}
	}
	return names, entries
}

// validEnvName 校验本文件接受的环境变量名形态：大写字母、数字与下划线，
// 首字符不为数字；比 shell 更严格，杜绝奇怪的注入面。
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		upper := character >= 'A' && character <= 'Z'
		digit := character >= '0' && character <= '9'
		underscore := character == '_'
		if !upper && !digit && !underscore {
			return false
		}
	}
	return name[0] >= 'A' && name[0] <= 'Z' || name[0] == '_'
}
