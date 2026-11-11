// evolution_gate.go 实现 MEM-06 演化候选（晋升门禁）、评测与晋升：候选/评测/晋升/沙箱
// 激活/试运行/回滚分别操作并标明证据与影响；不自动成为生产默认。
// Author: fxbin
package memory

import (
	"fmt"
	"strings"
	"time"
)

// PromotionStage 是候选生命周期阶段。
type PromotionStage string

const (
	// StageCandidate 仅登记候选。
	StageCandidate PromotionStage = "candidate"
	// StageEvaluated 已评测。
	StageEvaluated PromotionStage = "evaluated"
	// StagePromoted 已晋升（仍非生产默认）。
	StagePromoted PromotionStage = "promoted"
	// StageSandboxActive 沙箱激活。
	StageSandboxActive PromotionStage = "sandbox_active"
	// StageTrial 试运行中。
	StageTrial PromotionStage = "trial"
	// StageRolledBack 已回滚。
	StageRolledBack PromotionStage = "rolled_back"
	// StageProduction 生产默认（仅显式启用）。
	StageProduction PromotionStage = "production"
)

// PromotionCandidate 是一个演化候选及其证据。
type PromotionCandidate struct {
	ID        string         `json:"id"`
	Stage     PromotionStage `json:"stage"`
	Evidence  string         `json:"evidence,omitempty"`
	Impact    string         `json:"impact,omitempty"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// PromotionGate 管理候选阶段迁移；生产默认需显式启用。
type PromotionGate struct {
	items map[string]*PromotionCandidate
	// allowProduction 显式授权后才允许进入 production。
	allowProduction bool
}

// NewPromotionGate 创建门禁；allowProduction 默认 false。
func NewPromotionGate(allowProduction bool) *PromotionGate {
	return &PromotionGate{items: make(map[string]*PromotionCandidate), allowProduction: allowProduction}
}

// Register 登记候选。
func (gate *PromotionGate) Register(id, evidence string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("candidate id is required")
	}
	if _, exists := gate.items[id]; exists {
		return fmt.Errorf("candidate %q already exists", id)
	}
	gate.items[id] = &PromotionCandidate{
		ID: id, Stage: StageCandidate, Evidence: evidence, UpdatedAt: time.Now().UTC(),
	}
	return nil
}

// Promote 晋升阶段；评测证据缺失拒绝晋升。
func (gate *PromotionGate) Promote(id, impact string) error {
	item, found := gate.items[id]
	if !found {
		return fmt.Errorf("candidate %q not found", id)
	}
	if item.Stage != StageCandidate && item.Stage != StageEvaluated {
		return fmt.Errorf("candidate %q cannot promote from %s", id, item.Stage)
	}
	if strings.TrimSpace(item.Evidence) == "" {
		return fmt.Errorf("candidate %q has no evaluation evidence", id)
	}
	item.Stage = StagePromoted
	item.Impact = impact
	item.UpdatedAt = time.Now().UTC()
	return nil
}

// Evaluate 记录评测证据与影响。
func (gate *PromotionGate) Evaluate(id, evidence, impact string) error {
	item, found := gate.items[id]
	if !found {
		return fmt.Errorf("candidate %q not found", id)
	}
	item.Evidence = evidence
	item.Impact = impact
	if item.Stage == StageCandidate {
		item.Stage = StageEvaluated
	}
	item.UpdatedAt = time.Now().UTC()
	return nil
}

// ActivateSandbox 沙箱激活。
func (gate *PromotionGate) ActivateSandbox(id string) error {
	item, found := gate.items[id]
	if !found {
		return fmt.Errorf("candidate %q not found", id)
	}
	if item.Stage != StagePromoted && item.Stage != StageEvaluated {
		return fmt.Errorf("candidate %q cannot activate from %s", id, item.Stage)
	}
	item.Stage = StageSandboxActive
	item.UpdatedAt = time.Now().UTC()
	return nil
}

// StartTrial 试运行。
func (gate *PromotionGate) StartTrial(id string) error {
	item, found := gate.items[id]
	if !found {
		return fmt.Errorf("candidate %q not found", id)
	}
	if item.Stage != StageSandboxActive {
		return fmt.Errorf("candidate %q cannot trial from %s", id, item.Stage)
	}
	item.Stage = StageTrial
	item.UpdatedAt = time.Now().UTC()
	return nil
}

// Rollback 回滚候选。
func (gate *PromotionGate) Rollback(id, reason string) error {
	item, found := gate.items[id]
	if !found {
		return fmt.Errorf("candidate %q not found", id)
	}
	if item.Stage == StageProduction {
		return fmt.Errorf("production candidate %q requires explicit manual rollback", id)
	}
	item.Stage = StageRolledBack
	item.Impact = reason
	item.UpdatedAt = time.Now().UTC()
	return nil
}

// EnableProduction 显式启用生产默认；未授权时失败关闭。
func (gate *PromotionGate) EnableProduction(id string) error {
	item, found := gate.items[id]
	if !found {
		return fmt.Errorf("candidate %q not found", id)
	}
	if !gate.allowProduction {
		return fmt.Errorf("production promotion is not authorized")
	}
	if item.Stage != StageTrial && item.Stage != StagePromoted {
		return fmt.Errorf("candidate %q cannot enter production from %s", id, item.Stage)
	}
	item.Stage = StageProduction
	item.UpdatedAt = time.Now().UTC()
	return nil
}

// Get 返回候选快照。
func (gate *PromotionGate) Get(id string) (PromotionCandidate, bool) {
	item, found := gate.items[id]
	if !found {
		return PromotionCandidate{}, false
	}
	return *item, true
}
