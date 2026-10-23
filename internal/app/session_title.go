// session_title.go 生成会话标题时选择最低档模型变体，降低命名成本。
// Author: fxbin
package app

import "strings"

// SessionTitleConfig 是标题生成配置。
type SessionTitleConfig struct {
	// PreferredModel 优先使用的低成本模型。
	PreferredModel string
	// FallbackModel 备选模型。
	FallbackModel string
	// MaxChars 标题最大长度。
	MaxChars int
}

// DefaultSessionTitleConfig 返回保守默认。
func DefaultSessionTitleConfig() SessionTitleConfig {
	return SessionTitleConfig{
		PreferredModel: "mini",
		FallbackModel:  "flash",
		MaxChars:       40,
	}
}

// ChooseTitleModel 按成本优先选择模型变体。
func ChooseTitleModel(config SessionTitleConfig, available []string) string {
	for _, preferred := range []string{config.PreferredModel, config.FallbackModel} {
		if preferred == "" {
			continue
		}
		for _, model := range available {
			if strings.Contains(strings.ToLower(model), strings.ToLower(preferred)) {
				return model
			}
		}
	}
	if len(available) > 0 {
		return available[0]
	}
	return config.FallbackModel
}

// SanitizeTitle 修剪标题到最大长度并去多余空白。
func SanitizeTitle(raw string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 40
	}
	trimmed := strings.TrimSpace(raw)
	runes := []rune(trimmed)
	if len(runes) > maxChars {
		return string(runes[:maxChars])
	}
	return trimmed
}
