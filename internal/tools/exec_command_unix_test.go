//go:build darwin || linux

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestExecCommandUsesArgvAndEnforcesEnvironmentAllowlist(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: workspace, EnvAllow: map[string]struct{}{"CAIRN_TEST": {}}}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/echo","hello"],"cwd":".","env":{"CAIRN_TEST":"1"}}`))
	if !result.OK || result.Code != "ok" {
		t.Fatalf("result = %#v", result)
	}
	blocked := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/echo","hello"],"env":{"UNSAFE":"1"}}`))
	if blocked.OK || blocked.Code != "invalid" {
		t.Fatalf("blocked = %#v", blocked)
	}
}

func TestExecCommandAcceptsWorkspaceAlias(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: workspace}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/echo","hello"],"cwd":"/workspace"}`))
	if !result.OK || !strings.Contains(result.Preview, `"cwd":"."`) {
		t.Fatalf("result = %#v", result)
	}
	arguments, err := ParseExecArguments(json.RawMessage(`{"argv":["/usr/bin/true"],"cwd":"/workspace/subdir"}`))
	if err != nil || arguments.CWD != "subdir" {
		t.Fatalf("arguments = %#v, %v", arguments, err)
	}
}

func TestExecCommandDoesNotInheritUnallowlistedEnvironment(t *testing.T) {
	t.Setenv("CAIRN_SHOULD_NOT_LEAK", "private-value")
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: workspace, EnvAllow: map[string]struct{}{}}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/usr/bin/env"]}`))
	if !result.OK || strings.Contains(result.Preview, "CAIRN_SHOULD_NOT_LEAK") || strings.Contains(result.Preview, "private-value") {
		t.Fatalf("environment isolation failed: %#v", result)
	}
}
