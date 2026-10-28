// delegate_async.go 承载根运行异步委派的进程内运行时与恢复门禁：子完成
// 信号登记表由父 Loop 与委派 goroutine 共享，goroutine 生命周期由本运行时
// 跟踪并在父 Run 返回前统一回收；恢复入口对非终态子失败关闭并引导显式
// 恢复，不自动续跑子运行。
// Author: fxbin
package app

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/domain"
)

// asyncDelegateRuntime 汇集一次根运行的异步委派进程内运行时：子完成信号
// 登记表供父 Loop 停靠等待，runner 计数跟踪派发 goroutine。仅当配置显式
// 启用 tools.delegate_async 时由 service.Run 装配。
type asyncDelegateRuntime struct {
	children *agent.AsyncChildSet
	runners  sync.WaitGroup
}

// newAsyncDelegateRuntime 创建异步委派运行时并登记到给定 Loop。
func newAsyncDelegateRuntime(loop *agent.Loop) *asyncDelegateRuntime {
	children := agent.NewAsyncChildSet()
	loop.AsyncChildren = children
	return &asyncDelegateRuntime{children: children}
}

// recallAsyncChildren 在父 Loop 返回后回收异步子：调用方先取消父运行上下文
// （子运行沿用同一上下文，随之进入取消收尾），再等待全部派发 goroutine 退出，
// 保证子 Journal 终态与完成回写先于父 Run 返回落定，不把写入窗口留给进程退出。
func (runtime *asyncDelegateRuntime) recallAsyncChildren() {
	runtime.runners.Wait()
}

// unfinishedChildRunIDs 从父事件重建子状态并返回非终态子身份（按字典序），
// 供恢复门禁失败关闭。
func unfinishedChildRunIDs(events []domain.Event) []string {
	states := RebuildSubagentStates(events)
	pending := make([]string, 0, len(states))
	for childID, state := range states {
		if state.State == "delegated" {
			pending = append(pending, childID)
		}
	}
	sort.Strings(pending)
	return pending
}

// rejectResumeWithUnfinishedAsyncChildren 在异步委派启用时对含非终态子的父
// 运行恢复失败关闭：父 resume 不自动续跑子、不重发未知副作用，先显式恢复子
// 运行使其收束，或以 resume --abort-run 收尾后再恢复父运行。
func rejectResumeWithUnfinishedAsyncChildren(asyncEnabled bool, events []domain.Event) error {
	if !asyncEnabled {
		return nil
	}
	pending := unfinishedChildRunIDs(events)
	if len(pending) == 0 {
		return nil
	}
	return fmt.Errorf("async child run(s) %s have not finished; resume the child run to let it settle, or finish it with resume <child-run-id> --abort-run, before resuming the parent", strings.Join(pending, ", "))
}
