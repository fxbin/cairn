// observe.go 实现 BRW-03 页面观察与元素选择：观察结果绑定时间与目标；
// 截图不是实时真相，过期目标必须重观察后才能操作。
// Author: fxbin
package browser

import (
	"fmt"
	"strings"
	"time"
)

// ObservationKind 区分观察载荷类型。
type ObservationKind string

const (
	// ObsScreenshot 是截图字节引用（不含图像本体）。
	ObsScreenshot ObservationKind = "screenshot"
	// ObsAccessibilityTree 是可访问树摘要。
	ObsAccessibilityTree ObservationKind = "a11y_tree"
	// ObsElementRef 是元素引用。
	ObsElementRef ObservationKind = "element_ref"
)

// Observation 是一次绑定目标与时间的观察事实。
type Observation struct {
	ID        string          `json:"id"`
	TabID     string          `json:"tab_id"`
	Kind      ObservationKind `json:"kind"`
	ElementID string          `json:"element_id,omitempty"`
	// CapturedAt 是观察时刻；后续操作须核对未过期。
	CapturedAt time.Time `json:"captured_at"`
	// ContentDigest 是载荷摘要，用于证明引用未被替换。
	ContentDigest string `json:"content_digest,omitempty"`
	// ExpiresAt 是观察有效期；零值表示仅受 maxAge 约束。
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// ObservationStore 保存最近观察；不保存图像本体。
type ObservationStore struct {
	items map[string]*Observation
}

// NewObservationStore 创建空观察库。
func NewObservationStore() *ObservationStore {
	return &ObservationStore{items: make(map[string]*Observation)}
}

// Record 登记一次观察。
func (store *ObservationStore) Record(observation Observation) error {
	if strings.TrimSpace(observation.ID) == "" || strings.TrimSpace(observation.TabID) == "" {
		return fmt.Errorf("observation id and tab id are required")
	}
	if observation.CapturedAt.IsZero() {
		observation.CapturedAt = time.Now().UTC()
	}
	copied := observation
	store.items[observation.ID] = &copied
	return nil
}

// Get 返回观察快照。
func (store *ObservationStore) Get(observationID string) (Observation, bool) {
	item, found := store.items[observationID]
	if !found {
		return Observation{}, false
	}
	return *item, true
}

// IsFresh 判断观察在给定时刻是否仍有效。
// 截图/引用过期后不得作为操作依据。
func IsFresh(observation Observation, now time.Time, maxAge time.Duration) bool {
	if observation.CapturedAt.IsZero() {
		return false
	}
	if !observation.ExpiresAt.IsZero() && now.After(observation.ExpiresAt) {
		return false
	}
	if maxAge > 0 && now.Sub(observation.CapturedAt) > maxAge {
		return false
	}
	return true
}

// RequireFresh 目标过期时返回重观察错误。
func (store *ObservationStore) RequireFresh(observationID string, now time.Time, maxAge time.Duration) (Observation, error) {
	observation, found := store.Get(observationID)
	if !found {
		return Observation{}, fmt.Errorf("observation %q not found", observationID)
	}
	if !IsFresh(observation, now, maxAge) {
		return Observation{}, fmt.Errorf("observation %q is stale; re-observe the target", observationID)
	}
	return observation, nil
}

// ElementTarget 是绑定观察的元素选择结果。
type ElementTarget struct {
	ElementID     string `json:"element_id"`
	ObservationID string `json:"observation_id"`
	TabID         string `json:"tab_id"`
	Role          string `json:"role,omitempty"`
	Name          string `json:"name,omitempty"`
}

// BindElement 将元素引用绑定到具体观察，拒绝跨标签绑定。
func BindElement(store *ObservationStore, observationID, elementID string) (ElementTarget, error) {
	observation, found := store.Get(observationID)
	if !found {
		return ElementTarget{}, fmt.Errorf("observation %q not found", observationID)
	}
	if observation.Kind != ObsElementRef && observation.Kind != ObsAccessibilityTree {
		return ElementTarget{}, fmt.Errorf("observation %q cannot host element refs", observationID)
	}
	if strings.TrimSpace(elementID) == "" {
		return ElementTarget{}, fmt.Errorf("element id is required")
	}
	return ElementTarget{
		ElementID:     elementID,
		ObservationID: observation.ID,
		TabID:         observation.TabID,
	}, nil
}
