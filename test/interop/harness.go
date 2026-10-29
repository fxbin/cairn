package interop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"
)

// interopRunTimeout 是单次受控运行的整体时限；超时按中断处理。
const interopRunTimeout = 120 * time.Second

// Usage 是一次运行的 token 用量观测。Reported 区分"外壳报告了用量"与
// "外壳没有报告任何用量"——两者对 Cairn 的成本口径影响完全不同。
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	CostUSD      float64
	Reported     bool
}

// HarnessRun 是一次受控运行的输入。
type HarnessRun struct {
	// Workspace 是本次运行的工作区目录；所有外壳都必须只在此目录内产生副作用。
	Workspace string
	// SnapshotRoot 是副作用快照的根目录；通常是 Workspace 的父目录，
	// 这样越界写入（写到工作区之外）也能被观测到。留空时回退为 Workspace。
	SnapshotRoot string
	// Task 是投递给外壳的提示词正文（已含任务脚本标记）。
	Task string
	// Timeout 是本次运行的整体时限；超时按中断处理。
	Timeout time.Duration
	// InterruptAfter 是在该时长后向外壳发送中断信号，用于观测取消语义；
	// 为零表示不主动中断。
	InterruptAfter time.Duration
	// Env 是追加到子进程的环境变量，形如 KEY=VALUE。
	Env []string
	// Args 是外壳命令的额外参数。
	Args []string
	// Command 是外壳可执行文件名。
	Command string
	// Dir 是子进程工作目录；为空时使用 Workspace。
	Dir string
	// Stdin 是投递给子进程的标准输入。
	Stdin string
}

// HarnessResult 是一次受控运行的结果观测。
type HarnessResult struct {
	// ExitCode 是子进程退出码；被信号杀死时为 -1。
	ExitCode int
	// Stdout 与 Stderr 是捕获的原始输出。
	Stdout string
	Stderr string
	// SessionHandle 是外壳给出的会话或线程句柄，用于恢复观测。
	SessionHandle string
	// Usage 是外壳报告的 token 用量。
	Usage Usage
	// Events 是解析出的事件类型序列（按出现顺序）。
	Events []string
	// Interrupted 标记本次运行是否被外部信号中断。
	Interrupted bool
	// Duration 是本次运行的墙钟耗时。
	Duration time.Duration
	// Extra 保存外壳特有的补充观测（键名自解释）。
	Extra map[string]string
}

// Harness 是一个可被对照实验驱动的执行外壳。
type Harness interface {
	// Name 返回外壳标识，用于矩阵与实验结果的行键。
	Name() string
	// Version 返回外壳版本号；探测不到时返回空串。
	Version() string
	// Run 执行一次受控运行。
	Run(ctx context.Context, run HarnessRun) (HarnessResult, error)
}

// processOutcome 是 runProcess 的内部结果。
type processOutcome struct {
	ExitCode    int
	Stdout      string
	Stderr      string
	Interrupted bool
	Duration    time.Duration
}

// runProcess 在给定上下文里运行一个子进程，按需在 InterruptAfter 之后
// 依次发送 SIGINT、SIGTERM、SIGKILL，并回收输出。
func runProcess(ctx context.Context, run HarnessRun) (processOutcome, error) {
	command := exec.Command(run.Command, run.Args...)
	command.Dir = run.Dir
	if command.Dir == "" {
		command.Dir = run.Workspace
	}
	command.Env = append(os.Environ(), run.Env...)
	if run.Stdin != "" {
		command.Stdin = strings.NewReader(run.Stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := command.Start(); err != nil {
		return processOutcome{}, fmt.Errorf("start %s: %w", run.Command, err)
	}
	started := time.Now()
	waited := make(chan error, 1)
	go func() {
		waited <- command.Wait()
	}()

	interrupted := false
	var timer *time.Timer
	var interrupt <-chan time.Time
	if run.InterruptAfter > 0 {
		timer = time.NewTimer(run.InterruptAfter)
		interrupt = timer.C
	}
	overall := time.NewTimer(run.Timeout)
	defer overall.Stop()

	for {
		select {
		case err := <-waited:
			if timer != nil {
				timer.Stop()
			}
			outcome := processOutcome{
				Stdout: stdout.String(), Stderr: stderr.String(),
				Duration: time.Since(started), Interrupted: interrupted,
			}
			if err == nil {
				outcome.ExitCode = 0
				return outcome, nil
			}
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				outcome.ExitCode = exitError.ExitCode()
				return outcome, nil
			}
			return outcome, fmt.Errorf("wait %s: %w", run.Command, err)
		case <-interrupt:
			interrupted = true
			interrupt = nil
			_ = signalProcessGroup(command.Process.Pid, syscall.SIGINT)
			grace := time.NewTimer(3 * time.Second)
			select {
			case err := <-waited:
				grace.Stop()
				outcome := processOutcome{
					Stdout: stdout.String(), Stderr: stderr.String(),
					Duration: time.Since(started), Interrupted: true,
				}
				var exitError *exec.ExitError
				if errors.As(err, &exitError) {
					outcome.ExitCode = exitError.ExitCode()
					return outcome, nil
				}
				return outcome, nil
			case <-grace.C:
				grace.Stop()
				_ = signalProcessGroup(command.Process.Pid, syscall.SIGTERM)
				hard := time.NewTimer(3 * time.Second)
				select {
				case err := <-waited:
					hard.Stop()
					outcome := processOutcome{
						Stdout: stdout.String(), Stderr: stderr.String(),
						Duration: time.Since(started), Interrupted: true,
					}
					var exitError *exec.ExitError
					if errors.As(err, &exitError) {
						outcome.ExitCode = exitError.ExitCode()
						return outcome, nil
					}
					return outcome, nil
				case <-hard.C:
					hard.Stop()
					_ = signalProcessGroup(command.Process.Pid, syscall.SIGKILL)
					err := <-waited
					outcome := processOutcome{
						Stdout: stdout.String(), Stderr: stderr.String(),
						Duration: time.Since(started), Interrupted: true, ExitCode: -1,
					}
					var exitError *exec.ExitError
					if errors.As(err, &exitError) {
						outcome.ExitCode = exitError.ExitCode()
					}
					return outcome, nil
				}
			}
		case <-overall.C:
			_ = signalProcessGroup(command.Process.Pid, syscall.SIGKILL)
			<-waited
			return processOutcome{
				Stdout: stdout.String(), Stderr: stderr.String(),
				Duration: time.Since(started), Interrupted: true, ExitCode: -1,
			}, nil
		}
	}
}

// signalProcessGroup 向进程组发送信号，保证子进程树一并被回收。
func signalProcessGroup(pid int, signal syscall.Signal) error {
	group := -pid
	if err := syscall.Kill(group, signal); err != nil {
		return syscall.Kill(pid, signal)
	}
	return nil
}

// FileSnapshot 是工作区在一次运行前后的文件快照。
type FileSnapshot struct {
	// Files 是相对工作区根的文件路径到内容 SHA-256 的映射。
	Files map[string]string
}

// SnapshotWorkspace 递归遍历工作区并记录每个普通文件的内容摘要。
// 忽略 .git 与隐藏的会话目录，避免把外壳自身的状态计入副作用。
func SnapshotWorkspace(root string) (FileSnapshot, error) {
	snapshot := FileSnapshot{Files: map[string]string{}}
	err := walkWorkspace(root, "", snapshot.Files)
	if err != nil {
		return FileSnapshot{}, err
	}
	return snapshot, nil
}

// walkWorkspace 递归收集工作区文件摘要。
func walkWorkspace(root string, relative string, out map[string]string) error {
	entries, err := os.ReadDir(root + "/" + relative)
	if err != nil {
		return fmt.Errorf("read workspace %s: %w", relative, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if relative == "" && (name == ".git" || name == "node_modules") {
			continue
		}
		if strings.HasPrefix(name, ".") && relative == "" {
			continue
		}
		child := name
		if relative != "" {
			child = relative + "/" + name
		}
		if entry.IsDir() {
			if err := walkWorkspace(root, child, out); err != nil {
				return err
			}
			continue
		}
		content, err := os.ReadFile(root + "/" + child)
		if err != nil {
			return fmt.Errorf("read workspace file %s: %w", child, err)
		}
		out[child] = digestOf(content)
	}
	return nil
}

// ChangedFiles 返回快照之间发生变化的文件路径（新增、修改或删除）。
func ChangedFiles(before FileSnapshot, after FileSnapshot) []string {
	changed := map[string]bool{}
	for path, digest := range after.Files {
		if previous, ok := before.Files[path]; !ok || previous != digest {
			changed[path] = true
		}
	}
	for path := range before.Files {
		if _, ok := after.Files[path]; !ok {
			changed[path] = true
		}
	}
	names := make([]string, 0, len(changed))
	for path := range changed {
		names = append(names, path)
	}
	sort.Strings(names)
	return names
}

// CountLines 统计文本行数，供输出体积对照使用。
func CountLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}
