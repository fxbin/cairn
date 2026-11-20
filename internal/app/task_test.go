// task_test.go 回归任务创建时的验收检查身份可用性门禁。
//
// Author: fxbin
package app

import (
	"context"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
)

func TestCreateTaskRejectsUnconfiguredCheckID(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Verification.Commands = []config.VerificationCommand{{ID: "build", Argv: []string{"/usr/bin/true"}}}
	scope := TaskScope{Workspace: t.TempDir(), StateDir: t.TempDir(), Config: configuration}
	criteria := []contract.AcceptanceCriterion{{ID: "compile", Statement: "项目可编译", Required: true, Checks: []string{"test"}}}
	_, err := (Service{}).CreateTask(context.Background(), scope, TaskDefinition{Goal: "修复构建", Criteria: criteria})
	if err == nil || !strings.Contains(err.Error(), "id \"test\"") {
		t.Fatalf("expected actionable error about missing id, got %v", err)
	}
}

func TestCreateTaskAcceptsConfiguredCheckID(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Verification.Commands = []config.VerificationCommand{{ID: "build", Argv: []string{"/usr/bin/true"}}}
	scope := TaskScope{Workspace: t.TempDir(), StateDir: t.TempDir(), Config: configuration}
	criteria := []contract.AcceptanceCriterion{{ID: "compile", Statement: "项目可编译", Required: true, Checks: []string{"build"}}}
	created, err := (Service{}).CreateTask(context.Background(), scope, TaskDefinition{Goal: "修复构建", Criteria: criteria})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if created.Definition.AcceptanceDigest == "" {
		t.Fatal("acceptance contract was not frozen")
	}
}
