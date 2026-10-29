package decision

// 本文件补充决策账本的上下文投影：注入上下文的只有决策陈述与证据定位，
// 并受固定 token 预算约束。投影自行按项目与主体过滤，不含任何工具、权限
// 或审批字段，项目知识因此无法覆盖上层 Policy；放不下的条目被丢弃并如实
// 标记截断。

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

const (
	// DefaultContextTokenBudget 是注入上下文的默认 token 预算。
	DefaultContextTokenBudget = 400
	// MaxContextTokenBudget 是注入上下文的硬上限，调用方不能提高。
	MaxContextTokenBudget = 800
	// ContextBytesPerToken 是 token 估算口径：每两个 UTF-8 字节约一个 token，
	// 与 Agent 侧请求估算采用同一悲观近似，保证两侧数字可比。
	ContextBytesPerToken = 2
)

// ContextEntry 是注入上下文时的一条受限投影：只有决策陈述、适用上下文与
// 证据定位，不含工具、权限、审批或运行事实字段。
type ContextEntry struct {
	// ID 是决策记录标识。
	ID string `json:"id"`
	// Kind 是决策种类，调用方据此区分权威决策、未验证观察与个人偏好。
	Kind string `json:"kind"`
	// Context 是决策适用的上下文说明。
	Context string `json:"context"`
	// Decision 是决策陈述。
	Decision string `json:"decision"`
	// Rationale 是决策理由。
	Rationale string `json:"rationale"`
	// Evidence 是支撑该结论的证据定位，不含证据正文。
	Evidence []EvidenceRef `json:"evidence"`
	// ReviewedAt 是最近一次人工复核时刻。
	ReviewedAt *time.Time `json:"reviewed_at,omitempty"`
	// ExpiresAt 是保留期截止时刻。
	ExpiresAt time.Time `json:"expires_at"`
}

// ContextProjection 是注入上下文的有界投影结果。
type ContextProjection struct {
	// Entries 是被选中的决策条目。
	Entries []ContextEntry `json:"entries"`
	// EstimatedTokens 是投影正文的估算 token 数。
	EstimatedTokens int `json:"estimated_tokens"`
	// DroppedCount 是因预算不足被丢弃的条目数。
	DroppedCount int `json:"dropped_count"`
	// Truncated 表示投影因预算限制丢弃过条目。
	Truncated bool `json:"truncated"`
}

// ProjectContext 把决策记录投影为可注入上下文的受限条目。投影自行按项目与
// 主体过滤：跨项目记录与他人私有偏好不会进入上下文，不依赖调用方先过滤。
// 候选、未确认、已撤销、已被替代与已过期的记录一律不进入投影；预算不足时
// 按固定顺序丢弃并如实计数，不静默截断。
func ProjectContext(project, principal string, records []DecisionRecord, budget int, now time.Time) (ContextProjection, error) {
	if !validIdentifier(project) {
		return ContextProjection{}, errors.New("decision context projection requires a valid project identifier")
	}
	if !validIdentifier(principal) {
		return ContextProjection{}, errors.New("decision context projection requires a valid principal identifier")
	}
	if budget == 0 {
		budget = DefaultContextTokenBudget
	}
	if budget < 0 || budget > MaxContextTokenBudget {
		return ContextProjection{}, fmt.Errorf("decision context budget must be within 1 and %d", MaxContextTokenBudget)
	}
	if now.IsZero() {
		return ContextProjection{}, errors.New("decision context projection requires an explicit time")
	}
	projection := ContextProjection{Entries: []ContextEntry{}}
	candidates := make([]DecisionRecord, 0, len(records))
	for _, record := range records {
		if record.Project != project || !record.VisibleTo(project, principal) {
			continue
		}
		if record.EffectiveStatus(now) == StatusActive {
			candidates = append(candidates, record)
		}
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		leftRank := contextKindRank(candidates[left].Kind)
		rightRank := contextKindRank(candidates[right].Kind)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		leftTime := recordSortTime(candidates[left])
		rightTime := recordSortTime(candidates[right])
		if !leftTime.Equal(rightTime) {
			return leftTime.After(rightTime)
		}
		return candidates[left].ID < candidates[right].ID
	})
	for _, record := range candidates {
		entry := ContextEntry{
			ID:         record.ID,
			Kind:       record.Kind,
			Context:    record.Context,
			Decision:   record.Decision,
			Rationale:  record.Rationale,
			Evidence:   append([]EvidenceRef(nil), record.Evidence...),
			ReviewedAt: record.ReviewedAt,
			ExpiresAt:  record.ExpiresAt,
		}
		cost, err := estimateEntryTokens(entry)
		if err != nil {
			return ContextProjection{}, err
		}
		if projection.EstimatedTokens+cost > budget {
			projection.Truncated = true
			projection.DroppedCount++
			continue
		}
		projection.Entries = append(projection.Entries, entry)
		projection.EstimatedTokens += cost
	}
	return projection, nil
}

// contextKindRank 给出决策种类在投影中的优先级：权威决策先于未验证观察，
// 个人偏好最后，使预算紧张时优先保留可作为项目规则引用的结论。
func contextKindRank(kind string) int {
	switch kind {
	case KindAuthoritative:
		return 0
	case KindObservation:
		return 1
	default:
		return 2
	}
}

// estimateEntryTokens 估算单条投影条目的 token 数：按序列化后的字节数除以
// 每 token 字节数向上取整，与 Agent 侧估算口径一致。
func estimateEntryTokens(entry ContextEntry) (int, error) {
	encoded, err := json.Marshal(entry)
	if err != nil {
		return 0, fmt.Errorf("encode decision context entry: %w", err)
	}
	return (len(encoded) + ContextBytesPerToken - 1) / ContextBytesPerToken, nil
}
