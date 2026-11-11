// model_search_test 锁定模型目录检索的过滤、排序与结果上限。
//
// Author: fxbin
package tools

import "testing"

// TestSearchModelsFiltersAndSorts 锁定关键字过滤与稳定排序。
func TestSearchModelsFiltersAndSorts(t *testing.T) {
	entries := []ModelSearchEntry{
		{ID: "glm-4", Name: "GLM-4", Provider: "zhipu"},
		{ID: "claude-3", Name: "Claude 3", Provider: "anthropic"},
		{ID: "gpt-4o", Name: "GPT-4o", Provider: "openai"},
	}
	got := SearchModels("g", entries)
	if len(got) != 2 {
		t.Fatalf("应命中 2 条，得到 %+v", got)
	}
	if got[0].Provider != "openai" || got[1].Provider != "zhipu" {
		t.Fatalf("应按 Provider 排序: %+v", got)
	}
	if all := SearchModels("", entries); len(all) != 3 {
		t.Fatalf("空查询应返回全部: %+v", all)
	}
	if none := SearchModels("zzz", entries); len(none) != 0 {
		t.Fatalf("无匹配应为空: %+v", none)
	}
}

// TestSearchModelsCapsResults 锁定结果数量上限。
func TestSearchModelsCapsResults(t *testing.T) {
	entries := make([]ModelSearchEntry, 0, ModelSearchMaxResults+5)
	for i := 0; i < ModelSearchMaxResults+5; i++ {
		entries = append(entries, ModelSearchEntry{ID: "m", Name: "m", Provider: "p"})
	}
	if got := SearchModels("", entries); len(got) != ModelSearchMaxResults {
		t.Fatalf("应截断到 %d，得到 %d", ModelSearchMaxResults, len(got))
	}
}
