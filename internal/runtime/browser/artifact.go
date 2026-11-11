// Package browser 的截图工件与交互回放：工件与 run/操作关联、脱敏有界；
// 回放默认只读，重执行需新的授权与当前状态检查。
// Author: fxbin
package browser

import (
	"fmt"
	"strings"
	"time"
)

// maxArtifactRetentionDays 限制截图/日志保留窗口。
const maxArtifactRetentionDays = 30

// Artifact 是与 run/操作关联的截图或日志引用。
type Artifact struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	RunID     string    `json:"run_id,omitempty"`
	ActionID  string    `json:"action_id,omitempty"`
	Ref       string    `json:"ref"`
	SHA256    string    `json:"sha256,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ReplayMode 区分只读回放与需授权的重执行。
type ReplayMode string

const (
	// ReplayReadOnly 仅展示历史，不产生副作用。
	ReplayReadOnly ReplayMode = "read_only"
	// ReplayReexecute 需要新的授权与当前状态检查。
	ReplayReexecute ReplayMode = "reexecute"
)

// ReplayRequest 是一次回放请求。
type ReplayRequest struct {
	ArtifactID string     `json:"artifact_id"`
	Mode       ReplayMode `json:"mode"`
	RunID      string     `json:"run_id,omitempty"`
}

// NewArtifact 创建有界工件记录；空引用失败关闭。
func NewArtifact(id, kind, runID, actionID, ref, sha string, at time.Time) (Artifact, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(ref) == "" {
		return Artifact{}, fmt.Errorf("id and ref are required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return Artifact{
		ID: id, Kind: kind, RunID: runID, ActionID: actionID,
		Ref: ref, SHA256: sha, CreatedAt: at.UTC(),
	}, nil
}

// Expired 判断工件是否超过保留窗口。
func (artifact Artifact) Expired(now time.Time) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.Sub(artifact.CreatedAt) > maxArtifactRetentionDays*24*time.Hour
}

// ValidateReplay 校验回放请求；重执行必须携带 run 身份。
func ValidateReplay(request ReplayRequest) error {
	if strings.TrimSpace(request.ArtifactID) == "" {
		return fmt.Errorf("artifact_id is required")
	}
	switch request.Mode {
	case ReplayReadOnly:
		return nil
	case ReplayReexecute:
		if strings.TrimSpace(request.RunID) == "" {
			return fmt.Errorf("reexecute requires run_id and fresh authorization")
		}
		return nil
	default:
		return fmt.Errorf("unknown replay mode %q", request.Mode)
	}
}
