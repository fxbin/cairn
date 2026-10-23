// 本文件在应用层解析 Provider 凭据来源，不把密钥写入配置或进程环境。
//
// Author: fxbin
package app

import (
	"errors"
	"os"

	"github.com/fxbin/cairn/internal/credential"
	"github.com/fxbin/cairn/internal/tools"
)

// providerCredential 按环境、工作区凭据、用户凭据解析连接密钥；工作区文件
// 经过统一路径边界，不能借符号链接读取工作区外的文件。
func providerCredential(workspacePath, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if value := os.Getenv(name); value != "" {
		return value, nil
	}
	if workspacePath == "" {
		current, err := os.Getwd()
		if err != nil {
			return "", errors.New("cannot resolve credential workspace")
		}
		workspacePath = current
	}
	workspace, err := tools.NewWorkspace(workspacePath, nil, 0, 0, 0)
	if err != nil {
		return "", errors.New("cannot resolve credential workspace")
	}
	path, _, err := workspace.Resolve(credential.KeysFileName)
	if err != nil {
		return "", errors.New("workspace credential path is invalid")
	}
	userPath, err := credential.UserKeysPath()
	if err != nil {
		return "", errors.New("cannot resolve user credentials")
	}
	return credential.ResolveEnv(name, path, userPath)
}
