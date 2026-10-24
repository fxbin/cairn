package tools

import (
	"testing"
	"time"
)

// TestRunnerForSelectsStrategy 锁定策略选择与失败关闭。
func TestRunnerForSelectsStrategy(t *testing.T) {
	workspace := Workspace{Root: "/tmp"}
	if _, err := runnerFor("unknown", workspace, false, true, "img", 64); err == nil {
		t.Fatal("unknown backend must fail closed")
	}
	runner, err := runnerFor(ExecBackendHost, workspace, false, true, "", 64)
	if err != nil {
		t.Fatal(err)
	}
	if runner.ID() != ExecBackendHost {
		t.Fatalf("id = %q", runner.ID())
	}
	runner, err = runnerFor(ExecBackendHost, workspace, true, true, "", 64)
	if err != nil {
		t.Fatal(err)
	}
	if runner.ID() != ExecBackendSeatbeltHost {
		t.Fatalf("confined id = %q", runner.ID())
	}
	runner, err = runnerFor(ExecBackendDocker, workspace, false, true, "alpine:3.21", 64)
	if err != nil {
		t.Fatal(err)
	}
	if runner.ID() != ExecBackendDocker {
		t.Fatalf("docker id = %q", runner.ID())
	}
}

// TestExecRunRequestCarriesGrace 确认请求形状含宽限字段。
func TestExecRunRequestCarriesGrace(t *testing.T) {
	request := ExecRunRequest{Grace: time.Second, HostWrite: true}
	if request.Grace != time.Second || !request.HostWrite {
		t.Fatalf("request = %+v", request)
	}
}
