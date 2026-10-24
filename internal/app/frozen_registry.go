// 本文件导出评测冻结合同所需的工具注册表身份：装配路径与生产 Boot 共享，
// 保证冻结时刻钉住的模型可见工具面与真实运行完全同源。
//
// Author: fxbin
package app

import (
	"fmt"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/tools"
)

// FrozenToolRegistryFingerprint 以空工作区装配指定静态 Preset 的固定工具注册
// 表并返回其指纹。规格声明与工作区事实无关——指纹只反映模型可见面的身份；
// 实际执行时 Boot 按同一 Preset 装配。可选工具（tools.web / tools.exec_session）
// 不进入冻结指纹：评测默认关闭 opt-in，开启后应以真实配置另冻。
func FrozenToolRegistryFingerprint(preset string) (string, error) {
	empty := tools.Workspace{}
	registry, err := tools.NewRegistry(presetTools(empty, tools.ExecCommand{Workspace: empty}, preset, config.ToolsConfig{})...)
	if err != nil {
		return "", fmt.Errorf("assemble frozen tool registry for preset %q: %w", preset, err)
	}
	return registry.Fingerprint()
}
