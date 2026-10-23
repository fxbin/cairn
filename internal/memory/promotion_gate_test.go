// promotion_gate_test 锁定 MEM-06 演化候选、评测、晋升与回滚。
//
// Author: fxbin
package memory

import "testing"

// TestPromotionGateLifecycle 锁定候选→评测→晋升→沙箱→试运行。
func TestPromotionGateLifecycle(t *testing.T) {
	gate := NewPromotionGate(false)
	if err := gate.Register("c1", "bench-v1"); err != nil {
		t.Fatal(err)
	}
	if err := gate.Register("c1", "x"); err == nil {
		t.Fatal("重复 ID 应拒绝")
	}
	if err := gate.Promote("c1", "impact"); err != nil {
		t.Fatalf("有证据应可晋升: %v", err)
	}
	if err := gate.ActivateSandbox("c1"); err != nil {
		t.Fatal(err)
	}
	if err := gate.StartTrial("c1"); err != nil {
		t.Fatal(err)
	}
	// 生产默认需显式授权
	if err := gate.EnableProduction("c1"); err == nil {
		t.Fatal("未授权不得进入生产")
	}
	authorized := NewPromotionGate(true)
	_ = authorized.Register("c1", "e")
	_ = authorized.Promote("c1", "i")
	_ = authorized.ActivateSandbox("c1")
	_ = authorized.StartTrial("c1")
	if err := authorized.EnableProduction("c1"); err != nil {
		t.Fatalf("授权后应可进入生产: %v", err)
	}
}

// TestPromotionRollback 锁定回滚。
func TestPromotionRollback(t *testing.T) {
	gate := NewPromotionGate(false)
	_ = gate.Register("c2", "e")
	if err := gate.Rollback("c2", "flaky"); err != nil {
		t.Fatal(err)
	}
	got, _ := gate.Get("c2")
	if got.Stage != StageRolledBack {
		t.Fatalf("应为回滚: %+v", got)
	}
}
