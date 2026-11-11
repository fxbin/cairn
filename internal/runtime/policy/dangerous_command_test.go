// dangerous_command_test 锁定危险命令识别与 bypass 拒绝。
//
// Author: fxbin
package policy

import "testing"

// TestDetectDangerousCommand 锁定危险模式识别。
func TestDetectDangerousCommand(t *testing.T) {
	if DetectDangerousCommand("exec_command", "ls -la") != nil {
		t.Fatal("ls 不应判定危险")
	}
	dangerous := DetectDangerousCommand("exec_command", "rm -rf /")
	if dangerous == nil || dangerous.Reason == "" || dangerous.RewriteHint == "" {
		t.Fatalf("rm -rf / 应判定危险: %+v", dangerous)
	}
	if DetectDangerousCommand("exec_command", "shutdown now") == nil {
		t.Fatal("shutdown 应判定危险")
	}
}

// TestGuardDangerousCommandBlocksBypass 锁定 bypass 模式仍拒绝。
func TestGuardDangerousCommandBlocksBypass(t *testing.T) {
	_, allowed := GuardDangerousCommand("exec_command", "rm -rf /", "allow", "allow")
	if allowed {
		t.Fatal("bypass 模式也应拒绝危险命令")
	}
	_, allowed = GuardDangerousCommand("exec_command", "ls -la", "allow", "allow")
	if !allowed {
		t.Fatal("安全命令应放行")
	}
}
