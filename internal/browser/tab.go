// Package browser 定义受控浏览器连接的标签、导航、观察与操作合同；
// 不开放无认证控制端口，不授予桌面特权。
// Author: fxbin
package browser

import (
	"fmt"
	"strings"
	"time"
)

// TabState 是标签生命周期状态。
type TabState string

const (
	// TabStateOpen 表示可导航标签。
	TabStateOpen TabState = "open"
	// TabStateClosed 表示已关闭标签。
	TabStateClosed TabState = "closed"
	// TabStateDetached 表示连接断开后的孤儿标签。
	TabStateDetached TabState = "detached"
)

// Tab 是受控标签的身份与状态快照。
type Tab struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	Title      string    `json:"title"`
	State      TabState  `json:"state"`
	OwnedByRun string    `json:"owned_by_run,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// NavigationAction 是导航动作类型。
type NavigationAction string

const (
	// NavGo 跳转到显式 URL。
	NavGo NavigationAction = "go"
	// NavBack 后退。
	NavBack NavigationAction = "back"
	// NavForward 前进。
	NavForward NavigationAction = "forward"
	// NavReload 刷新。
	NavReload NavigationAction = "reload"
	// NavClose 关闭标签。
	NavClose NavigationAction = "close"
)

// NavigationRequest 绑定明确标签目标，避免误操作用户无关标签。
type NavigationRequest struct {
	TabID  string           `json:"tab_id"`
	Action NavigationAction `json:"action"`
	URL    string           `json:"url,omitempty"`
	RunID  string           `json:"run_id,omitempty"`
}

// NavigationResult 是导航后的可观察事实。
type NavigationResult struct {
	TabID     string           `json:"tab_id"`
	Action    NavigationAction `json:"action"`
	URL       string           `json:"url,omitempty"`
	State     TabState         `json:"state"`
	AppliedAt time.Time        `json:"applied_at"`
}

// TabRegistry 管理受控标签；并发安全由调用方保证或包装。
type TabRegistry struct {
	tabs map[string]*Tab
}

// NewTabRegistry 创建空标签表。
func NewTabRegistry() *TabRegistry {
	return &TabRegistry{tabs: make(map[string]*Tab)}
}

// Open 登记一个受控标签；ID 为空或重复时失败关闭。
func (registry *TabRegistry) Open(tab Tab) error {
	if strings.TrimSpace(tab.ID) == "" {
		return fmt.Errorf("tab id is required")
	}
	if _, exists := registry.tabs[tab.ID]; exists {
		return fmt.Errorf("tab %q already exists", tab.ID)
	}
	if tab.State == "" {
		tab.State = TabStateOpen
	}
	if tab.UpdatedAt.IsZero() {
		tab.UpdatedAt = time.Now().UTC()
	}
	copied := tab
	registry.tabs[tab.ID] = &copied
	return nil
}

// Get 返回标签快照。
func (registry *TabRegistry) Get(tabID string) (Tab, bool) {
	tab, found := registry.tabs[tabID]
	if !found {
		return Tab{}, false
	}
	return *tab, true
}

// List 返回全部标签（含已关闭），顺序不定。
func (registry *TabRegistry) List() []Tab {
	out := make([]Tab, 0, len(registry.tabs))
	for _, tab := range registry.tabs {
		out = append(out, *tab)
	}
	return out
}

// ApplyNavigation 对明确目标执行导航；目标缺失或已关闭时失败关闭。
func (registry *TabRegistry) ApplyNavigation(request NavigationRequest) (NavigationResult, error) {
	if strings.TrimSpace(request.TabID) == "" {
		return NavigationResult{}, fmt.Errorf("tab_id is required")
	}
	tab, exists := registry.tabs[request.TabID]
	if !exists {
		return NavigationResult{}, fmt.Errorf("tab %q not found", request.TabID)
	}
	if tab.State == TabStateClosed {
		return NavigationResult{}, fmt.Errorf("tab %q is closed", request.TabID)
	}
	if request.RunID != "" && tab.OwnedByRun != "" && tab.OwnedByRun != request.RunID {
		return NavigationResult{}, fmt.Errorf("tab %q is not owned by run %s", request.TabID, request.RunID)
	}
	now := time.Now().UTC()
	switch request.Action {
	case NavGo:
		if strings.TrimSpace(request.URL) == "" {
			return NavigationResult{}, fmt.Errorf("url is required for go")
		}
		tab.URL = request.URL
	case NavBack, NavForward, NavReload:
		// 只读确认存在；实际历史由连接层负责
	case NavClose:
		tab.State = TabStateClosed
	default:
		return NavigationResult{}, fmt.Errorf("unknown navigation action %q", request.Action)
	}
	tab.UpdatedAt = now
	return NavigationResult{
		TabID:     tab.ID,
		Action:    request.Action,
		URL:       tab.URL,
		State:     tab.State,
		AppliedAt: now,
	}, nil
}
