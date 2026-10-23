// catalog_sync.go 实现 MCP 目录同步与失效处理：周期刷新目录快照，
// 检测漂移并标记失效工具；不自动重放工具调用。
// Author: fxbin
package mcpclient

import (
	"sync"
	"time"
)

// CatalogStatus 是目录同步状态。
type CatalogStatus string

const (
	// CatalogFresh 表示目录新鲜。
	CatalogFresh CatalogStatus = "fresh"
	// CatalogStale 表示超过 TTL 未刷新。
	CatalogStale CatalogStatus = "stale"
	// CatalogDrifted 表示检测到 schema 漂移。
	CatalogDrifted CatalogStatus = "drifted"
	// CatalogInvalid 表示目录损坏或不可读。
	CatalogInvalid CatalogStatus = "invalid"
)

// CatalogSyncState 是一次目录同步的可观测状态。
type CatalogSyncState struct {
	Server    string        `json:"server"`
	Status    CatalogStatus `json:"status"`
	Digest    string        `json:"digest,omitempty"`
	SyncedAt  time.Time     `json:"synced_at"`
	ToolCount int           `json:"tool_count"`
}

// CatalogSyncer 管理目录同步状态。
type CatalogSyncer struct {
	mu     sync.RWMutex
	states map[string]CatalogSyncState
	ttl    time.Duration
}

// NewCatalogSyncer 创建同步器；ttl <=0 使用默认 10 分钟。
func NewCatalogSyncer(ttl time.Duration) *CatalogSyncer {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &CatalogSyncer{states: make(map[string]CatalogSyncState), ttl: ttl}
}

// Record 记录一次同步结果。
func (syncer *CatalogSyncer) Record(server, digest string, toolCount int, at time.Time) {
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	syncer.states[server] = CatalogSyncState{
		Server: server, Status: CatalogFresh, Digest: digest, SyncedAt: at, ToolCount: toolCount,
	}
}

// MarkDrifted 标记 schema 漂移。
func (syncer *CatalogSyncer) MarkDrifted(server string) {
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	state := syncer.states[server]
	state.Server = server
	state.Status = CatalogDrifted
	syncer.states[server] = state
}

// MarkInvalid 标记目录损坏。
func (syncer *CatalogSyncer) MarkInvalid(server string) {
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	state := syncer.states[server]
	state.Server = server
	state.Status = CatalogInvalid
	syncer.states[server] = state
}

// Status 返回当前状态；超 TTL 自动降为 stale。
func (syncer *CatalogSyncer) Status(server string, now time.Time) (CatalogSyncState, bool) {
	syncer.mu.RLock()
	defer syncer.mu.RUnlock()
	state, found := syncer.states[server]
	if !found {
		return CatalogSyncState{}, false
	}
	if state.Status == CatalogFresh && now.Sub(state.SyncedAt) > syncer.ttl {
		state.Status = CatalogStale
	}
	return state, true
}

// Snapshot 返回全部状态副本。
func (syncer *CatalogSyncer) Snapshot(now time.Time) []CatalogSyncState {
	syncer.mu.RLock()
	defer syncer.mu.RUnlock()
	out := make([]CatalogSyncState, 0, len(syncer.states))
	for server := range syncer.states {
		state, _ := syncer.Status(server, now)
		out = append(out, state)
	}
	return out
}
