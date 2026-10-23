// artifact_test 锁定 BRW-06 工件保留与回放模式约束。
//
// Author: fxbin
package browser

import (
	"testing"
	"time"
)

// TestNewArtifactAndExpiry 锁定工件创建与保留窗口。
func TestNewArtifactAndExpiry(t *testing.T) {
	artifact, err := NewArtifact("art-1", "screenshot", "run-1", "act-1", "ref://x", "sha256:1", time.Unix(100, 0).UTC())
	if err != nil || artifact.ActionID != "act-1" {
		t.Fatalf("创建失败: %+v %v", artifact, err)
	}
	if artifact.Expired(time.Unix(100, 0).Add(24 * time.Hour).UTC()) {
		t.Fatal("一天内不应过期")
	}
	if !artifact.Expired(time.Unix(100, 0).Add(40 * 24 * time.Hour).UTC()) {
		t.Fatal("超过保留窗口应过期")
	}
	if _, err := NewArtifact("", "screenshot", "r", "a", "ref", "", time.Time{}); err == nil {
		t.Fatal("空 ID 应拒绝")
	}
}

// TestValidateReplayModes 锁定只读与重执行约束。
func TestValidateReplayModes(t *testing.T) {
	if err := ValidateReplay(ReplayRequest{ArtifactID: "art-1", Mode: ReplayReadOnly}); err != nil {
		t.Fatalf("只读回放应允许: %v", err)
	}
	if err := ValidateReplay(ReplayRequest{ArtifactID: "art-1", Mode: ReplayReexecute}); err == nil {
		t.Fatal("重执行缺 run_id 应拒绝")
	}
	if err := ValidateReplay(ReplayRequest{ArtifactID: "art-1", Mode: ReplayReexecute, RunID: "run-1"}); err != nil {
		t.Fatalf("重执行带 run_id 应允许: %v", err)
	}
	if err := ValidateReplay(ReplayRequest{Mode: ReplayReadOnly}); err == nil {
		t.Fatal("空工件 ID 应拒绝")
	}
}

func zeroTime() time.Time { return time.Time{} }
