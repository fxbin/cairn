// thinking_effort.go 定义思考力度档位，供 Provider 请求画像与 UI 调节。
// Author: fxbin
package config

import "strings"

// ThinkingEffort 是思考力度档位。
type ThinkingEffort string

const (
	// ThinkingOff 关闭思考。
	ThinkingOff ThinkingEffort = "off"
	// ThinkingLow 低力度。
	ThinkingLow ThinkingEffort = "low"
	// ThinkingMedium 中等。
	ThinkingMedium ThinkingEffort = "medium"
	// ThinkingHigh 高力度。
	ThinkingHigh ThinkingEffort = "high"
)

// ThinkingEffortLevels 是合法档位列表。
var ThinkingEffortLevels = []ThinkingEffort{ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh}

// ParseThinkingEffort 解析档位；非法值失败关闭。
func ParseThinkingEffort(value string) (ThinkingEffort, bool) {
	switch ThinkingEffort(strings.ToLower(strings.TrimSpace(value))) {
	case ThinkingOff:
		return ThinkingOff, true
	case ThinkingLow:
		return ThinkingLow, true
	case ThinkingMedium:
		return ThinkingMedium, true
	case ThinkingHigh:
		return ThinkingHigh, true
	}
	return "", false
}

// NormalizeThinkingEffort 归一化档位；空值返回 medium。
func NormalizeThinkingEffort(value string) ThinkingEffort {
	if effort, ok := ParseThinkingEffort(value); ok {
		return effort
	}
	return ThinkingMedium
}
