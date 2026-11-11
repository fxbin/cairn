// model_search.go 提供模型目录的只读检索：按关键字过滤名称/标识，结果
// 有界；不触网络、不改配置。
// Author: fxbin
package tools

import (
	"sort"
	"strings"
)

// ModelSearchEntry 是可检索的模型目录条目（非敏感字段）。
type ModelSearchEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

// ModelSearchMaxResults 限制单次检索返回数量。
const ModelSearchMaxResults = 20

// SearchModels 按大小写不敏感子串过滤条目；query 为空返回前 N 条。
// 结果按 Provider、Name 稳定排序，保证可复现。
func SearchModels(query string, entries []ModelSearchEntry) []ModelSearchEntry {
	q := strings.TrimSpace(strings.ToLower(query))
	matched := make([]ModelSearchEntry, 0, len(entries))
	for _, entry := range entries {
		if q == "" ||
			strings.Contains(strings.ToLower(entry.ID), q) ||
			strings.Contains(strings.ToLower(entry.Name), q) ||
			strings.Contains(strings.ToLower(entry.Provider), q) {
			matched = append(matched, entry)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Provider != matched[j].Provider {
			return matched[i].Provider < matched[j].Provider
		}
		return matched[i].Name < matched[j].Name
	})
	if len(matched) > ModelSearchMaxResults {
		matched = matched[:ModelSearchMaxResults]
	}
	return matched
}
