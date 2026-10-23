// tab_test 锁定 BRW-02 标签登记、导航绑定与所有权边界。
//
// Author: fxbin
package browser

import "testing"

// TestTabRegistryOpenAndDuplicate 锁定登记与重复 ID 拒绝。
func TestTabRegistryOpenAndDuplicate(t *testing.T) {
	registry := NewTabRegistry()
	if err := registry.Open(Tab{ID: "tab-1", URL: "https://example.com"}); err != nil {
		t.Fatalf("登记应成功: %v", err)
	}
	if err := registry.Open(Tab{ID: "tab-1"}); err == nil {
		t.Fatal("重复 ID 应拒绝")
	}
	if err := registry.Open(Tab{ID: ""}); err == nil {
		t.Fatal("空 ID 应拒绝")
	}
	if tab, ok := registry.Get("tab-1"); !ok || tab.State != TabStateOpen {
		t.Fatalf("标签状态不符: %+v %v", tab, ok)
	}
}

// TestApplyNavigationBindsTarget 锁定导航绑定明确目标。
func TestApplyNavigationBindsTarget(t *testing.T) {
	registry := NewTabRegistry()
	_ = registry.Open(Tab{ID: "tab-1", URL: "https://a.example", OwnedByRun: "run-1"})
	result, err := registry.ApplyNavigation(NavigationRequest{TabID: "tab-1", Action: NavGo, URL: "https://b.example", RunID: "run-1"})
	if err != nil || result.URL != "https://b.example" {
		t.Fatalf("跳转应成功: %+v %v", result, err)
	}
	// 非属主 run 不得导航
	if _, err := registry.ApplyNavigation(NavigationRequest{TabID: "tab-1", Action: NavGo, URL: "https://x", RunID: "run-2"}); err == nil {
		t.Fatal("非属主 run 应拒绝")
	}
	// 目标缺失
	if _, err := registry.ApplyNavigation(NavigationRequest{TabID: "tab-x", Action: NavReload}); err == nil {
		t.Fatal("缺失标签应拒绝")
	}
	// go 必须带 URL
	if _, err := registry.ApplyNavigation(NavigationRequest{TabID: "tab-1", Action: NavGo}); err == nil {
		t.Fatal("go 缺 URL 应拒绝")
	}
	// close 后不可再导航
	if _, err := registry.ApplyNavigation(NavigationRequest{TabID: "tab-1", Action: NavClose, RunID: "run-1"}); err != nil {
		t.Fatalf("关闭应成功: %v", err)
	}
	if _, err := registry.ApplyNavigation(NavigationRequest{TabID: "tab-1", Action: NavReload}); err == nil {
		t.Fatal("已关闭标签应拒绝导航")
	}
}
