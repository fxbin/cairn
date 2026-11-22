// [INPUT]: 依赖跨会话任务合同、SQLite 存储与工作区排他锁。
// [OUTPUT]: 提供任务创建、查询、进度、暂停、恢复、取消与显式验收。
// [POS]: 应用门面核验关联运行或工作流报告。最小配置不打开任务库。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件装配持久任务生命周期。
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/task"
	tasksqlite "github.com/fxbin/cairn/internal/capabilities/task/sqlite"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

var (
	// ErrTaskDisabled 表示当前配置使用最小内核。
	ErrTaskDisabled = errors.New("persistent tasks are disabled in the minimal preset")
	// ErrTaskConflict 表示界面持有的版本或分配身份已过期。
	ErrTaskConflict = task.ErrConflict
	// ErrTaskTransition 表示任务当前状态不能接受请求。
	ErrTaskTransition = task.ErrTransition
	// ErrTaskAssignmentUnconfirmed 表示任务有分配，但缺少对应的 Run 创建事实。
	ErrTaskAssignmentUnconfirmed = errors.New("task assignment has no confirmed run; automatic execution is blocked")
	// ErrTaskUnverified 表示提交的 Run 不能作为任务验收依据。
	ErrTaskUnverified = errors.New("task completion requires trusted verification evidence")
)

// TaskScope 指定任务作用域与同一工作区的运行配置。
type TaskScope struct {
	Workspace string
	StateDir  string
	Config    config.Config
}

// TaskID 标识工作区内的持久任务。
type TaskID = task.ID

// TaskDefinition 指定冻结的目标和验收条件。
type TaskDefinition = task.Definition

// TaskProgress 表示用户确认的进度和剩余事项。
type TaskProgress = task.Progress

// Task 保存任务自身的持久事实。
type Task = task.Task

// TaskView 包含任务快照和从 Run Journal 派生的执行历史。
type TaskView struct {
	Task
	Assignments []TaskAssignmentView `json:"assignments"`
	Workflow    *WorkflowStatusView  `json:"workflow,omitempty"`
}

// CreateTask 在工作区内保存独立任务。创建不启动 Run。
func (Service) CreateTask(ctx context.Context, scope TaskScope, definition TaskDefinition) (Task, error) {
	scope, hash, err := scope.resolve()
	if err != nil {
		return Task{}, err
	}
	current, err := task.New(hash, definition, time.Now().UTC())
	if err != nil {
		return Task{}, err
	}
	if acceptance, found, contractErr := current.Definition.Contract(); contractErr != nil {
		return Task{}, contractErr
	} else if found {
		available := make(map[string]struct{}, len(scope.Config.Verification.Commands))
		for index, command := range scope.Config.Verification.Commands {
			id := command.ID
			if id == "" {
				id = fmt.Sprintf("verification-%d", index+1)
			}
			available[id] = struct{}{}
		}
		for _, checkID := range acceptance.RequiredCheckIDs() {
			if _, ok := available[checkID]; !ok {
				return Task{}, fmt.Errorf("acceptance check %q has no configured verification command; add one under verification.commands with id %q", checkID, checkID)
			}
		}
	}
	lock, err := acquireWorkspaceLock(scope.StateDir, hash)
	if err != nil {
		return Task{}, err
	}
	defer lock.Release()
	store, err := tasksqlite.Open(ctx, scope.databasePath(hash), false)
	if err != nil {
		return Task{}, err
	}
	defer store.Close()
	if err := store.Create(ctx, current); err != nil {
		return Task{}, err
	}
	return current, nil
}

// GetTask 只读查询持久任务，并在需要时从 Journal 校验关联运行。
func (Service) GetTask(ctx context.Context, scope TaskScope, id TaskID) (TaskView, error) {
	scope, hash, err := scope.resolve()
	if err != nil {
		return TaskView{}, err
	}
	store, err := tasksqlite.Open(ctx, scope.databasePath(hash), true)
	if err != nil {
		return TaskView{}, err
	}
	defer store.Close()
	return readTaskView(ctx, store, scope.StateDir, hash, id)
}

// UpdateTaskProgress 按版本记录用户确认的当前进度。
func (service Service) UpdateTaskProgress(ctx context.Context, scope TaskScope, id TaskID, revision uint64, progress TaskProgress) (Task, error) {
	return service.changeTask(ctx, scope, id, revision, func(view *TaskView) error {
		return view.RecordProgress(progress)
	})
}

// PauseTask 暂停新的分配。非终态 Run 继续使用原运行控制接口。
func (service Service) PauseTask(ctx context.Context, scope TaskScope, id TaskID, revision uint64) (Task, error) {
	return service.changeTask(ctx, scope, id, revision, func(view *TaskView) error {
		if err := view.requireSettledAssignments(); err != nil {
			return err
		}
		return view.Transition(task.Paused, nil)
	})
}

// ResumeTask 恢复已暂停任务的分配资格。
func (service Service) ResumeTask(ctx context.Context, scope TaskScope, id TaskID, revision uint64) (Task, error) {
	return service.changeTask(ctx, scope, id, revision, func(view *TaskView) error {
		return view.Transition(task.Active, nil)
	})
}

// CancelTask 终结任务。存在未收尾的运行时先走原运行控制或对账。
func (service Service) CancelTask(ctx context.Context, scope TaskScope, id TaskID, revision uint64) (Task, error) {
	return service.changeTask(ctx, scope, id, revision, func(view *TaskView) error {
		if err := view.requireSettledAssignments(); err != nil {
			return err
		}
		return view.Transition(task.Cancelled, nil)
	})
}

// CompleteTask 显式验收可信分配证据，或当前工作区上通过的独立审查报告。
func (service Service) CompleteTask(ctx context.Context, scope TaskScope, id TaskID, revision uint64, runID domain.RunID) (Task, error) {
	return service.changeTask(ctx, scope, id, revision, func(view *TaskView) error {
		if err := view.requireSettledAssignments(); err != nil {
			return err
		}
		if view.WorkflowBinding != nil {
			return completeTaskFromWorkflow(scope, view, runID)
		}
		if len(view.Assignments) == 0 {
			return ErrTaskUnverified
		}
		latest := view.Assignments[len(view.Assignments)-1]
		if latest.RunID != runID || !latest.Verification.Trusted || latest.Evidence == nil {
			return ErrTaskUnverified
		}
		acceptance, hasAcceptance, err := view.Definition.Contract()
		if err != nil {
			return err
		}
		if hasAcceptance {
			if latest.Acceptance == nil || latest.Evidence.AcceptanceDigest != acceptance.Digest || latest.Evidence.EvidenceRef == nil {
				return errors.New("task completion requires sufficient acceptance evidence for every required criterion")
			}
			if latest.Evaluation == nil || latest.Evaluation.Verdict != contract.VerdictPassed {
				return errors.New("task completion requires a passed acceptance evaluation")
			}
		} else if latest.Evidence.AcceptanceDigest != "" || latest.Evidence.EvidenceRef != nil {
			return ErrTaskUnverified
		}
		return view.Transition(task.Completed, latest.Evidence)
	})
}

// changeTask 在运行排他锁内核对读取版本、运行状态和证据，再提交任务事实。
func (Service) changeTask(ctx context.Context, scope TaskScope, id TaskID, revision uint64, change func(*TaskView) error) (Task, error) {
	scope, hash, err := scope.resolve()
	if err != nil {
		return Task{}, err
	}
	lock, err := acquireWorkspaceLock(scope.StateDir, hash)
	if err != nil {
		return Task{}, err
	}
	defer lock.Release()
	store, err := tasksqlite.Open(ctx, scope.databasePath(hash), false)
	if err != nil {
		return Task{}, err
	}
	defer store.Close()
	view, err := readTaskView(ctx, store, scope.StateDir, hash, id)
	if err != nil {
		return Task{}, err
	}
	if view.Revision != revision {
		return Task{}, ErrTaskConflict
	}
	if err := change(&view); err != nil {
		return Task{}, err
	}
	view.UpdatedAt = time.Now().UTC()
	if err := store.Commit(ctx, view.Task, nil); err != nil {
		return Task{}, err
	}
	view.Revision++
	return view.Task, nil
}

// resolve 规范化后端路径，并在最小配置下拒绝 Task 用例。
func (scope TaskScope) resolve() (TaskScope, string, error) {
	if scope.Config.Preset == config.PresetMinimal {
		return TaskScope{}, "", ErrTaskDisabled
	}
	workspace, hash, err := workspaceFor(scope.Workspace, scope.Config)
	if err != nil {
		return TaskScope{}, "", err
	}
	scope.Workspace = workspace.Root
	if scope.StateDir == "" {
		scope.StateDir = DefaultStateDir()
	}
	scope.StateDir, err = filepath.Abs(scope.StateDir)
	return scope, hash, err
}

// databasePath 将独立的任务库放在普通 Run 使用的工作区状态目录内。
func (scope TaskScope) databasePath(hash string) string {
	return filepath.Join(scope.StateDir, "workspaces", hash, "tasks.db")
}
