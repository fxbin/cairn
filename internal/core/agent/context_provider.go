// Package agent 定义可选上下文投影的消费方边界。
// Author: fxbin
package agent

import (
	"fmt"
	"slices"
	"strings"
)

// ContextProvider 把已核验的运行状态投影为用户级文本块，不接受权限或协议消息。
// 实现必须同步、确定性且无副作用；不得访问网络、重读当前资源或改变激活状态。
// Loop 会在消息重建与闭合 Turn 安全点多次调用；相同状态必须给出相同结果。
// 恢复所需状态须先由 Journal 历史恢复，压缩时文本作为持久上下文完整保留。
// 返回错误将阻止后续模型请求，不能静默丢失正文；空块与空列表均不增加消息。
type ContextProvider interface {
	ContextBlocks() ([]string, error)
}

// refreshContextProviders 按装配顺序原子替换安全点投影，失败时不使用部分结果。
// 所有文本统一以 user 角色插在会话前情后、原始任务前，纳入既有请求预算；
// 不单独截断提供器正文，无法容纳持久上下文时沿用原有预算失败关闭。
func (loop *Loop) refreshContextProviders() (bool, error) {
	var blocks []string
	for index, provider := range loop.ContextProviders {
		if provider == nil {
			return false, fmt.Errorf("context provider %d is nil", index)
		}
		projected, err := provider.ContextBlocks()
		if err != nil {
			return false, fmt.Errorf("project context provider %d: %w", index, err)
		}
		for _, block := range projected {
			if strings.TrimSpace(block) != "" {
				blocks = append(blocks, block)
			}
		}
	}
	changed := !slices.Equal(loop.contextBlocks, blocks)
	loop.contextBlocks = blocks
	return changed, nil
}
