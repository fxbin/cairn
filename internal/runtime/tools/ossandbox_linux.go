//go:build linux

// ossandbox_linux.go 将 host 收紧映射为 bubblewrap：宿主根按只读视图整体
// 映射，写权限只放开工作区与临时目录根，与 macOS seatbelt 的
// 「默认拒绝写、读不收紧」保持同一合同。
// Author: fxbin
package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// OSSandboxExecutable 是 Linux bubblewrap 入口。
const OSSandboxExecutable = "bwrap"

// sandboxRootSkipEntries 是不按普通只读绑定处理的根级条目：/dev 与 /proc 由
// bwrap 专用挂载重建，/sys 不进入沙箱，/tmp 由临时写规则改为可写绑定，
// /workspace 是工作区挂载点（随后单独绑定，避免与宿主同名目录冲突）。
var sandboxRootSkipEntries = []string{"dev", "proc", "sys", "tmp", "workspace"}

// sandboxFixedTempRoots 是平台固定的系统临时目录根：编译器缓存与验证子进程的临时写
// 落在这些根之下。与 macOS seatbelt 一致，临时写范围不得被写拒绝规则覆盖，否则会连带
// 打断依赖临时目录的构建与验证。当前进程使用其他临时目录时，由 sandboxTempRoots 追加
// 其规范路径。
var sandboxFixedTempRoots = []string{"/tmp", "/var/tmp"}

// sandboxRootEntries 列出需要只读映射的宿主根级条目，按字典序返回，跳过
// sandboxRootSkipEntries。列不出根目录时失败关闭，不退化为可写或空沙箱。
func sandboxRootEntries(readDir func(string) ([]os.DirEntry, error)) ([]string, error) {
	entries, err := readDir("/")
	if err != nil {
		return nil, fmt.Errorf("bubblewrap sandbox cannot enumerate host root: %w", err)
	}
	skipped := make(map[string]struct{}, len(sandboxRootSkipEntries))
	for _, name := range sandboxRootSkipEntries {
		skipped[name] = struct{}{}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, skip := skipped[entry.Name()]; skip {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// sandboxMountArgs 构造沙箱根的挂载参数：根级条目逐一只读绑定，使工作区外的
// 只读引用（依赖、缓存、构建产物目录）仍然可用；随后重建 /dev 与 /proc，并把
// tempRoots 中存在的临时目录根改为可写绑定。pathExists 由调用方注入，便于参数层测试。
//
// 这里必须逐条绑定宿主根条目而不是只绑定 /usr、/bin 等少数目录：沙箱根是空
// tmpfs，未绑定的路径在内核里不存在，动态链接程序会因 PT_INTERP
// （ubuntu-26.04 runner 上 /usr/bin/dash 为 /lib64/ld-linux-x86-64.so.2）缺失而
// execvp 返回 ENOENT，表现为 "bwrap: execvp /bin/sh: No such file or directory"。
func sandboxMountArgs(entries []string, tempRoots []string, pathExists func(string) bool) []string {
	args := make([]string, 0, len(entries)*3+len(tempRoots)*3+4)
	for _, name := range entries {
		path := "/" + name
		args = append(args, "--ro-bind", path, path)
	}
	args = append(args, "--dev", "/dev", "--proc", "/proc")
	for _, root := range tempRoots {
		if pathExists(root) {
			args = append(args, "--bind", root, root)
		}
	}
	return args
}

// hostPathExists 报告宿主该路径是否存在（符号链接按自身计）。
func hostPathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// sandboxReservedTempRoots 是沙箱保留、无法用可写绑定表达临时根的宿主路径：/workspace 是
// 工作区挂载点，/proc 与 /sys 由专用挂载重建。/dev 不在此列，见 sandboxProvidedTempRoot。
var sandboxReservedTempRoots = []string{"/workspace", "/proc", "/sys"}

// sandboxProvidedTempRoot 是 bubblewrap 自己重建的可写 /dev。其下的临时目录（常见的是
// tmpfs 上的 /dev/shm）在沙箱里已经可用，既不需要也不应再做宿主绑定：做了反而会盖掉
// 专用挂载。把它声明为“沙箱已提供”，临时根推导就不会为它追加规则，也不会因为主目录
// 取不到或只读档而拒绝它，行为与没有这条规则时一致。
const sandboxProvidedTempRoot = "/dev"

// linuxTempScopeOf 整理 bubblewrap 路径上的写入范围：工作区根、写入档位，以及沙箱自己
// 提供的可写 /dev。
func linuxTempScopeOf(options SeatbeltOptions, input sandboxTempInput) sandboxTempScope {
	scope := sandboxTempScopeOf(options.WorkspaceRoot, options.WriteMode == SeatbeltWriteWorkspace, input)
	scope.ProvidedRoots = []string{sandboxProvidedTempRoot}
	return scope
}

// rejectReservedTempRoots 拒绝落在沙箱保留路径之下的额外临时根。这些根级条目不按普通
// 只读绑定处理，其下的宿主路径在沙箱里要么不存在，要么指向别处，可写绑定无法表达，因此
// 在启动前失败关闭。
func rejectReservedTempRoots(extra []string) error {
	for _, root := range extra {
		for _, reserved := range sandboxReservedTempRoots {
			if pathWithin(root, reserved) {
				return newSandboxConfinementError(SandboxConfinementTempDir, "sandbox temp dir %q is under %s, which the bubblewrap sandbox reserves; point TMPDIR at a directory elsewhere", root, reserved)
			}
		}
	}
	return nil
}

// OSSandboxAvailable 判断 bwrap 是否在 PATH。
func OSSandboxAvailable() bool {
	_, err := exec.LookPath(OSSandboxExecutable)
	return err == nil
}

// HostConfinementName 返回当前平台 OS 约束名。
func HostConfinementName() string { return "host+bwrap" }

// OSSandboxConfinementReason 只做推导，不启动进程也不写文件：用占位 argv 走一遍与真实
// 启动相同的参数构造，bubblewrap 的参数构造没有副作用。可以生成时返回空串，否则返回
// 稳定原因码。
func OSSandboxConfinementReason(options SeatbeltOptions) SandboxConfinementReason {
	if !OSSandboxAvailable() {
		return SandboxConfinementUnavailable
	}
	_, _, err := ConfineArgv(options, []string{"true"})
	return sandboxConfinementReasonOf(err)
}

// ConfineArgv 用 bubblewrap 包装 argv；无独立临时文件。
//
// 沙箱根的 tmpfs 保持可写直到工作区挂载点建立完成，随后整体重挂为只读：
// 工作区父目录因此不可写（越界写得到 EROFS），而工作区绑定与临时目录绑定是
// 独立挂载，写权限不受影响。
func ConfineArgv(options SeatbeltOptions, argv []string) (wrapped []string, cleanup func() error, err error) {
	if !OSSandboxAvailable() {
		return nil, nil, errOSSandboxUnavailable
	}
	if options.WorkspaceRoot == "" {
		return nil, nil, newSandboxConfinementError(SandboxConfinementWorkspaceRoot, "workspace root is required")
	}
	binary, lookErr := exec.LookPath(OSSandboxExecutable)
	if lookErr != nil {
		return nil, nil, fmt.Errorf("bubblewrap is required for host confinement: %w", lookErr)
	}
	entries, entriesErr := sandboxRootEntries(os.ReadDir)
	if entriesErr != nil {
		return nil, nil, entriesErr
	}
	tempInput := currentSandboxTempInput()
	tempRoots, tempRootsErr := sandboxTempRoots(sandboxFixedTempRoots, tempInput, linuxTempScopeOf(options, tempInput))
	if tempRootsErr != nil {
		return nil, nil, tempRootsErr
	}
	if reservedErr := rejectReservedTempRoots(tempRoots[len(sandboxFixedTempRoots):]); reservedErr != nil {
		return nil, nil, reservedErr
	}
	args := []string{
		"--unshare-all",
		"--die-with-parent",
	}
	if options.ChildPIDFd > 0 {
		args = append(args, "--info-fd", strconv.Itoa(options.ChildPIDFd))
	}
	args = append(args, sandboxMountArgs(entries, tempRoots, hostPathExists)...)
	if options.AllowNetwork {
		args = append(args, "--share-net")
	}
	args = append(args, "--bind", options.WorkspaceRoot, "/workspace")
	if options.WriteMode == SeatbeltWriteNone {
		args = append(args, "--remount-ro", "/workspace")
	}
	args = append(args, "--remount-ro", "/", "--chdir", "/workspace", "--")
	wrapped = append([]string{binary}, args...)
	return append(wrapped, argv...), nil, nil
}

// sandboxChildPIDFd 是 --info-fd 上报使用的子进程描述符编号：exec.Cmd 的
// ExtraFiles[0] 在子进程中固定映射为 fd 3。
const sandboxChildPIDFd = 3

// sandboxChildPIDReportBytes 是 --info-fd 报告的读取上限；协议只需数百字节，
// 上限防止异常输出占满内存。
const sandboxChildPIDReportBytes = 4096

// sandboxChildPIDWait 是取消时等待 --info-fd 报告的上限。正常启动后报告在
// 内部进程真正执行前就已写入；该上限只覆盖包装层在报告前失败的路径，超时即
// 回退进程组信号，不阻塞取消。
const sandboxChildPIDWait = 100 * time.Millisecond

// bwrapInfo 是 bubblewrap --info-fd 报告的最小字段集；child-pid 是内部
// workload 在调用方（宿主）PID namespace 中的 PID。
type bwrapInfo struct {
	ChildPID int `json:"child-pid"`
}

// newSandboxTermination 为 bwrap 包装命令准备子进程 PID 上报通道，并返回直接
// 作用于内部 workload 的优雅终止方案。bwrap 不转发信号且遇 SIGTERM 立即退出，
// 若按进程组投递会由 --die-with-parent 立即 SIGKILL 内部进程；因此 SIGTERM
// 必须绕过包装进程，bwrap 继续存活并承接 TerminateGrace。宽限期结束后的
// 进程组 SIGKILL 兜底不变。
func newSandboxTermination() (*sandboxTermination, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("bubblewrap child pid channel: %w", err)
	}
	reported := make(chan int, 1)
	termination := &sandboxTermination{
		childPIDFd: sandboxChildPIDFd,
		extraFiles: []*os.File{writer},
	}
	termination.afterStart = func() {
		_ = writer.Close()
		go func() {
			defer reader.Close()
			var report bwrapInfo
			if decodeErr := json.NewDecoder(io.LimitReader(reader, sandboxChildPIDReportBytes)).Decode(&report); decodeErr == nil && report.ChildPID > 0 {
				reported <- report.ChildPID
			}
		}()
	}
	termination.graceful = func() bool {
		select {
		case pid := <-reported:
			return terminateSandboxWorkload(pid)
		case <-time.After(sandboxChildPIDWait):
			return false
		}
	}
	termination.close = func() {
		_ = writer.Close()
		_ = reader.Close()
	}
	return termination, nil
}

// terminateSandboxWorkload 向 bwrap 沙箱内部的实际 workload 投递 SIGTERM。
//
// --unshare-all 创建 PID namespace 后，bwrap 会在 namespace 内安装 PID 1
// reaper（负责回收僵尸进程），--info-fd 报告的 child-pid 正是该 reaper；
// 真正执行 argv 的 workload 是它的直接子进程。取消只作用于 workload，外层
// bwrap 与 PID 1 reaper 继续存活，因此 TerminateGrace 有真实等待对象；若此刻
// 还没有直接子进程（workload 尚未 fork 或已退出），返回 false，由调用方回退
// 到原有的进程组信号。
func terminateSandboxWorkload(initPID int) bool {
	children, err := sandboxChildren(initPID)
	if err != nil || len(children) == 0 {
		return false
	}
	delivered := false
	for _, child := range children {
		if terminateSandboxProcess(child) {
			delivered = true
		}
	}
	return delivered
}

// sandboxChildren 读取指定进程的直接子进程列表。
// /proc/<pid>/task/<pid>/children 是内核维护的即时快照，避免扫描整个 /proc。
func sandboxChildren(pid int) ([]int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(data))
	children := make([]int, 0, len(fields))
	for _, field := range fields {
		child, convErr := strconv.Atoi(field)
		if convErr != nil {
			continue
		}
		children = append(children, child)
	}
	return children, nil
}

// terminateSandboxProcess 通过 pidfd 向单个沙箱内部进程投递 SIGTERM。pidfd
// 绑定进程身份，避免读取 PID 后进程退出、PID 被复用时误伤无关进程。
func terminateSandboxProcess(pid int) bool {
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return false
	}
	defer unix.Close(pidfd)
	return unix.PidfdSendSignal(pidfd, unix.SIGTERM, nil, 0) == nil
}
