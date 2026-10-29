package workspace_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// scenarioEnv 是一次对照场景的环境：后端、交付目标版本与元数据位置。
type scenarioEnv struct {
	// backend 是被测后端。
	backend branchBackend
	// target 是各分支发布回去的交付目标版本。
	target ports.WorkspaceVersion
	// isolated 表示该后端是否提供分支级文件隔离。
	isolated bool
	// metadataRoot 是后端的 Git 元数据目录，用于统计对象库占用。
	metadataRoot string
	// cleanup 释放场景占用的资源。
	cleanup func()
}

// TestWorkspaceBackendContract 校验两种后端都满足窄合同的关键失败语义：
// 未授权、无验证证据、目标漂移与疑似机密都必须显式失败，丢弃后基线恢复。
func TestWorkspaceBackendContract(t *testing.T) {
	t.Run("shared_directory", func(t *testing.T) {
		env := setupSharedScenario(t)
		defer env.cleanup()
		assertBackendContract(t, env.backend, false)
	})
	t.Run("git_worktree", func(t *testing.T) {
		env := setupWorktreeScenario(t)
		defer env.cleanup()
		assertBackendContract(t, env.backend, true)
	})
}

// assertBackendContract 校验一个后端是否满足窄合同的关键语义。
func assertBackendContract(t *testing.T, backend branchBackend, isolated bool) {
	t.Helper()
	ctx, cancel := contextWithTimeout(t, verifierTimeout)
	defer cancel()

	base, err := backend.Prepare(ctx, ownerRequest("base"))
	if err != nil {
		t.Fatalf("prepare base version: %v", err)
	}
	if base.Isolated != isolated {
		t.Fatalf("base isolation = %v, want %v", base.Isolated, isolated)
	}
	branch, err := backend.Fork(ctx, base.Version, ownerRequest("branch"))
	if err != nil {
		t.Fatalf("fork branch version: %v", err)
	}
	handle, err := branchHandleOf(backend, branch.Version)
	if err != nil {
		t.Fatalf("resolve branch root: %v", err)
	}
	if err := handle.write("contract.txt", "contract content\n"); err != nil {
		t.Fatalf("write branch file: %v", err)
	}
	observed, err := backend.Observe(ctx, branch.Version)
	if err != nil {
		t.Fatalf("observe branch version: %v", err)
	}
	if observed.Digest == branch.Digest {
		t.Fatalf("digest did not change after write")
	}
	diff, err := backend.Diff(ctx, base.Version, branch.Version)
	if err != nil {
		t.Fatalf("diff versions: %v", err)
	}
	if !containsString(diff.ChangedPaths, "contract.txt") {
		t.Fatalf("changed paths %v missing written file", diff.ChangedPaths)
	}
	missing := ports.WorkspaceVersion{Backend: base.Version.Backend, ID: "missing-version"}
	if _, err := backend.Observe(ctx, missing); !errors.Is(err, ports.ErrWorkspaceVersionUnknown) {
		t.Fatalf("observe unknown version error = %v, want ErrWorkspaceVersionUnknown", err)
	}
	// 发布必须绑定到调用方刚观察到的目标状态：共享目录的目标与源同一物理
	// 空间，worktree 方案的目标是独立检出目录，两者都用摘要绑定。
	targetDigest := func() ports.WorkspaceDigest {
		t.Helper()
		state, err := backend.Observe(ctx, base.Version)
		if err != nil {
			t.Fatalf("observe base version: %v", err)
		}
		return state.Digest
	}
	publish := func(authorized bool, verified bool, expected ports.WorkspaceDigest) error {
		_, publishErr := backend.Publish(ctx, ports.WorkspacePublishRequest{
			Source:               branch.Version,
			Target:               base.Version,
			ExpectedTargetDigest: expected,
			Authorized:           authorized,
			VerifierPassed:       verified,
		})
		return publishErr
	}
	if err := publish(false, true, targetDigest()); !errors.Is(err, ports.ErrWorkspaceNotAuthorized) {
		t.Fatalf("publish without authorization error = %v, want ErrWorkspaceNotAuthorized", err)
	}
	if err := publish(true, false, targetDigest()); !errors.Is(err, ports.ErrWorkspaceVerificationMissing) {
		t.Fatalf("publish without verifier evidence error = %v, want ErrWorkspaceVerificationMissing", err)
	}
	if err := publish(true, true, ports.WorkspaceDigest("sha256:"+strings.Repeat("0", 64))); !errors.Is(err, ports.ErrWorkspaceDrift) {
		t.Fatalf("publish with stale digest error = %v, want ErrWorkspaceDrift", err)
	}
	if err := handle.write(secretFilePath, "token="+fakeSecret+"\n"); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	if err := publish(true, true, targetDigest()); !errors.Is(err, ports.ErrWorkspaceSecretLeak) {
		t.Fatalf("publish with secret error = %v, want ErrWorkspaceSecretLeak", err)
	}
	if err := backend.Discard(ctx, branch.Version); err != nil {
		t.Fatalf("discard branch version: %v", err)
	}
	restored, err := backend.Observe(ctx, base.Version)
	if err != nil {
		t.Fatalf("observe base version after discard: %v", err)
	}
	if restored.Digest != base.Digest {
		t.Fatalf("base digest after discard = %s, want %s", restored.Digest, base.Digest)
	}
}

// TestWorkspaceBackendComparison 对照共享目录与 Git worktree 两种后端，在
// 串行与并行分支、同文件冲突、目标漂移、疑似机密、失败回滚与交付六个维度
// 上采集实测数据。报告默认打印到测试日志，设置了 CAIRN_WORKSPACE_REPORT 时
// 额外写入 JSON 文件。
func TestWorkspaceBackendComparison(t *testing.T) {
	report := comparisonReport{Environment: runtimeInfo()}
	report.Environment.GitVersion = gitVersionDescription(t)

	shared := runBackendComparison(t, setupSharedScenario)
	worktree := runBackendComparison(t, setupWorktreeScenario)
	report.Backends = append(report.Backends, shared, worktree)
	report.Notes = []string{
		"复跑命令：go test ./test/workspace -run TestWorkspaceBackendComparison -count=1 -v",
		"对照仓库建在测试临时目录，不进入仓库；每个场景使用独立的临时仓库。",
		"启动时间只计后端 Prepare 与 Fork 的调用耗时，不含进程启动。",
		"磁盘按工作区内容字节统计，Git 元数据单列；依赖副本数指 deps 目录在磁盘上的份数。",
		"验证命令固定为 go build ./...，仓库只依赖标准库，可离线复跑。",
	}
	writeComparisonReport(t, report)
}

// runBackendComparison 对单个后端跑完全部场景。
func runBackendComparison(t *testing.T, setup func(t *testing.T) scenarioEnv) backendReport {
	t.Helper()
	result := backendReport{}

	serial := setup(t)
	result.Backend = serial.target.Backend
	result.Isolated = serial.isolated
	result.Serial = runSerialScenario(t, serial)
	serial.cleanup()

	parallel := setup(t)
	result.Parallel = runParallelScenario(t, parallel)
	parallel.cleanup()

	disk := setup(t)
	result.Disk = measureDisk(t, disk)
	disk.cleanup()

	conflict := setup(t)
	result.Conflict = runConflictScenario(t, conflict)
	conflict.cleanup()

	drift := setup(t)
	result.Drift = runDriftScenario(t, drift)
	drift.cleanup()

	secret := setup(t)
	result.Secret = runSecretScenario(t, secret)
	secret.cleanup()

	recovery := setup(t)
	result.Recovery = runRecoveryScenario(t, recovery)
	recovery.cleanup()

	delivery := setup(t)
	result.Delivery = runDeliveryScenario(t, delivery)
	delivery.cleanup()

	return result
}

// runSerialScenario 测量四分支串行执行的开销与发布结果。
func runSerialScenario(t *testing.T, env scenarioEnv) scenarioReport {
	t.Helper()
	ctx, cancel := contextWithTimeout(t, verifierTimeout)
	defer cancel()
	report := scenarioReport{Branches: experimentBranchCount, PublishFailureKinds: map[string]int{}}
	for index := 0; index < experimentBranchCount; index++ {
		started := time.Now()
		branch, err := env.backend.Fork(ctx, env.target, ownerRequest(fmt.Sprintf("serial-%d", index)))
		report.ForkMillis = append(report.ForkMillis, millisSince(started))
		if err != nil {
			report.PublishFailed++
			countFailure(report.PublishFailureKinds, err)
			continue
		}
		handle, err := branchHandleOf(env.backend, branch.Version)
		if err != nil {
			t.Fatalf("resolve branch root: %v", err)
		}
		if err := handle.write(fmt.Sprintf("%s-%d.txt", sharedFilePath, index), fmt.Sprintf("branch %d\n", index)); err != nil {
			t.Fatalf("write branch file: %v", err)
		}
		if err := env.backend.commit(branch.Version); err != nil && !errors.Is(err, ports.ErrWorkspaceUnsupported) {
			t.Fatalf("commit branch version: %v", err)
		}
		verified, _, err := runVerifier(handle.root)
		if err != nil {
			verified = false
		}
		if verified {
			report.VerifyPassed++
		} else {
			report.VerifyFailed++
		}
		target, err := env.backend.Observe(ctx, env.target)
		if err != nil {
			t.Fatalf("observe target version: %v", err)
		}
		_, publishErr := env.backend.Publish(ctx, ports.WorkspacePublishRequest{
			Source:               branch.Version,
			Target:               env.target,
			ExpectedTargetDigest: target.Digest,
			Authorized:           true,
			VerifierPassed:       verified,
		})
		if publishErr == nil {
			report.PublishSucceeded++
		} else {
			report.PublishFailed++
			countFailure(report.PublishFailureKinds, publishErr)
		}
		if err := env.backend.Discard(ctx, branch.Version); err != nil {
			t.Fatalf("discard branch version: %v", err)
		}
	}
	return report
}

// runParallelScenario 测量四分支并行执行时的隔离与验证可靠性。
func runParallelScenario(t *testing.T, env scenarioEnv) scenarioReport {
	t.Helper()
	ctx, cancel := contextWithTimeout(t, verifierTimeout)
	defer cancel()
	report := scenarioReport{Branches: experimentBranchCount, PublishFailureKinds: map[string]int{}}

	branches := make([]ports.WorkspaceVersion, 0, experimentBranchCount)
	for index := 0; index < experimentBranchCount; index++ {
		started := time.Now()
		branch, err := env.backend.Fork(ctx, env.target, ownerRequest(fmt.Sprintf("parallel-%d", index)))
		report.ForkMillis = append(report.ForkMillis, millisSince(started))
		if err != nil {
			t.Fatalf("fork parallel branch: %v", err)
		}
		branches = append(branches, branch.Version)
	}

	var wait sync.WaitGroup
	writeErrors := make([]error, len(branches))
	for index := range branches {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			handle, err := branchHandleOf(env.backend, branches[index])
			if err != nil {
				writeErrors[index] = err
				return
			}
			if index == experimentBranchCount-1 {
				writeErrors[index] = handle.write(
					fmt.Sprintf("%s-%d.go", sharedFilePath, index),
					fmt.Sprintf("package main\n\n// 分支 %d 故意写入无法编译的内容。\nfunc broken%d() { undefinedSymbol }\n", index, index),
				)
				return
			}
			writeErrors[index] = handle.write(fmt.Sprintf("%s-%d.txt", sharedFilePath, index), fmt.Sprintf("branch %d\n", index))
		}(index)
	}
	wait.Wait()
	for _, err := range writeErrors {
		if err != nil {
			t.Fatalf("write parallel branch file: %v", err)
		}
	}

	for index, version := range branches {
		handle, err := branchHandleOf(env.backend, version)
		if err != nil {
			t.Fatalf("resolve branch root: %v", err)
		}
		visible := 0
		for other := 0; other < experimentBranchCount; other++ {
			if other == index {
				continue
			}
			path := fmt.Sprintf("%s-%d.txt", sharedFilePath, other)
			if other == experimentBranchCount-1 {
				path = fmt.Sprintf("%s-%d.go", sharedFilePath, other)
			}
			if _, err := os.Stat(filepath.Join(handle.root, filepath.FromSlash(path))); err == nil {
				visible++
			}
		}
		if visible > report.CrossTalkFiles {
			report.CrossTalkFiles = visible
		}
		verified, _, err := runVerifier(handle.root)
		if err != nil {
			verified = false
		}
		if verified {
			report.VerifyPassed++
		} else {
			report.VerifyFailed++
			if index != experimentBranchCount-1 {
				report.ContaminatedBranches++
			}
		}
	}
	for _, version := range branches {
		if err := env.backend.commit(version); err != nil && !errors.Is(err, ports.ErrWorkspaceUnsupported) {
			t.Fatalf("commit branch version: %v", err)
		}
	}
	for _, version := range branches {
		target, err := env.backend.Observe(ctx, env.target)
		if err != nil {
			t.Fatalf("observe target version: %v", err)
		}
		verified, _, verifyErr := runVerifier(mustRoot(t, env.backend, version))
		if verifyErr != nil {
			verified = false
		}
		_, publishErr := env.backend.Publish(ctx, ports.WorkspacePublishRequest{
			Source:               version,
			Target:               env.target,
			ExpectedTargetDigest: target.Digest,
			Authorized:           true,
			VerifierPassed:       verified,
		})
		if publishErr == nil {
			report.PublishSucceeded++
		} else {
			report.PublishFailed++
			countFailure(report.PublishFailureKinds, publishErr)
		}
	}
	return report
}

// runConflictScenario 测量两个分支改写同一文件时的冲突检测能力。
func runConflictScenario(t *testing.T, env scenarioEnv) conflictReport {
	t.Helper()
	ctx, cancel := contextWithTimeout(t, verifierTimeout)
	defer cancel()
	result := conflictReport{}

	first, err := env.backend.Fork(ctx, env.target, ownerRequest("conflict-a"))
	if err != nil {
		t.Fatalf("fork first branch: %v", err)
	}
	second, err := env.backend.Fork(ctx, env.target, ownerRequest("conflict-b"))
	if err != nil {
		t.Fatalf("fork second branch: %v", err)
	}
	firstHandle, err := branchHandleOf(env.backend, first.Version)
	if err != nil {
		t.Fatalf("resolve first branch root: %v", err)
	}
	secondHandle, err := branchHandleOf(env.backend, second.Version)
	if err != nil {
		t.Fatalf("resolve second branch root: %v", err)
	}
	if err := firstHandle.write(conflictFilePath, "first branch content\n"); err != nil {
		t.Fatalf("write first branch file: %v", err)
	}
	if err := secondHandle.write(conflictFilePath, "second branch content\n"); err != nil {
		t.Fatalf("write second branch file: %v", err)
	}
	for _, version := range []ports.WorkspaceVersion{first.Version, second.Version} {
		if err := env.backend.commit(version); err != nil && !errors.Is(err, ports.ErrWorkspaceUnsupported) {
			t.Fatalf("commit branch version: %v", err)
		}
	}
	target, err := env.backend.Observe(ctx, env.target)
	if err != nil {
		t.Fatalf("observe target version: %v", err)
	}
	_, firstErr := env.backend.Publish(ctx, ports.WorkspacePublishRequest{
		Source:               first.Version,
		Target:               env.target,
		ExpectedTargetDigest: target.Digest,
		Authorized:           true,
		VerifierPassed:       true,
	})
	result.FirstPublishSucceeded = firstErr == nil
	target, err = env.backend.Observe(ctx, env.target)
	if err != nil {
		t.Fatalf("observe target version: %v", err)
	}
	_, secondErr := env.backend.Publish(ctx, ports.WorkspacePublishRequest{
		Source:               second.Version,
		Target:               env.target,
		ExpectedTargetDigest: target.Digest,
		Authorized:           true,
		VerifierPassed:       true,
	})
	result.SecondPublishFailed = secondErr != nil
	if secondErr != nil {
		result.FailureKind = failureKind(secondErr)
		var conflict *ports.WorkspaceConflictError
		if errors.As(secondErr, &conflict) {
			result.ConflictPaths = len(conflict.Paths)
		}
	}
	delivered, err := os.ReadFile(filepath.Join(mustRoot(t, env.backend, env.target), filepath.FromSlash(conflictFilePath)))
	if err != nil {
		t.Fatalf("read delivered conflict file: %v", err)
	}
	result.TargetOverwritten = string(delivered) == "second branch content\n"
	return result
}

// runDriftScenario 测量目标版本漂移时是否显式拒绝发布。
func runDriftScenario(t *testing.T, env scenarioEnv) driftReport {
	t.Helper()
	ctx, cancel := contextWithTimeout(t, verifierTimeout)
	defer cancel()
	result := driftReport{}
	branch, err := env.backend.Fork(ctx, env.target, ownerRequest("drift"))
	if err != nil {
		t.Fatalf("fork branch: %v", err)
	}
	handle, err := branchHandleOf(env.backend, branch.Version)
	if err != nil {
		t.Fatalf("resolve branch root: %v", err)
	}
	if err := handle.write("drift.txt", "drift content\n"); err != nil {
		t.Fatalf("write branch file: %v", err)
	}
	if err := env.backend.commit(branch.Version); err != nil && !errors.Is(err, ports.ErrWorkspaceUnsupported) {
		t.Fatalf("commit branch version: %v", err)
	}
	_, publishErr := env.backend.Publish(ctx, ports.WorkspacePublishRequest{
		Source:               branch.Version,
		Target:               env.target,
		ExpectedTargetDigest: ports.WorkspaceDigest("sha256:" + strings.Repeat("0", 64)),
		Authorized:           true,
		VerifierPassed:       true,
	})
	result.DriftDetected = errors.Is(publishErr, ports.ErrWorkspaceDrift)
	if publishErr != nil {
		result.FailureKind = failureKind(publishErr)
	}
	return result
}

// runSecretScenario 测量疑似机密是否被显式拦截。
func runSecretScenario(t *testing.T, env scenarioEnv) secretReport {
	t.Helper()
	ctx, cancel := contextWithTimeout(t, verifierTimeout)
	defer cancel()
	result := secretReport{}
	branch, err := env.backend.Fork(ctx, env.target, ownerRequest("secret"))
	if err != nil {
		t.Fatalf("fork branch: %v", err)
	}
	handle, err := branchHandleOf(env.backend, branch.Version)
	if err != nil {
		t.Fatalf("resolve branch root: %v", err)
	}
	if err := handle.write(secretFilePath, "api_key="+fakeSecret+"\n"); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	if err := env.backend.commit(branch.Version); err != nil && !errors.Is(err, ports.ErrWorkspaceUnsupported) {
		t.Fatalf("commit branch version: %v", err)
	}
	target, err := env.backend.Observe(ctx, env.target)
	if err != nil {
		t.Fatalf("observe target version: %v", err)
	}
	_, publishErr := env.backend.Publish(ctx, ports.WorkspacePublishRequest{
		Source:               branch.Version,
		Target:               env.target,
		ExpectedTargetDigest: target.Digest,
		Authorized:           true,
		VerifierPassed:       true,
	})
	result.LeakDetected = errors.Is(publishErr, ports.ErrWorkspaceSecretLeak)
	if publishErr != nil {
		result.FailureKind = failureKind(publishErr)
		var leak *ports.WorkspaceSecretLeakError
		if errors.As(publishErr, &leak) {
			result.ReportedPaths = len(leak.Paths)
		}
	}
	return result
}

// runRecoveryScenario 测量失败分支被丢弃后基线是否恢复、是否损及其他分支。
func runRecoveryScenario(t *testing.T, env scenarioEnv) recoveryReport {
	t.Helper()
	ctx, cancel := contextWithTimeout(t, verifierTimeout)
	defer cancel()
	result := recoveryReport{}
	base, err := env.backend.Observe(ctx, env.target)
	if err != nil {
		t.Fatalf("observe target version: %v", err)
	}
	failing, err := env.backend.Fork(ctx, env.target, ownerRequest("recovery-failing"))
	if err != nil {
		t.Fatalf("fork failing branch: %v", err)
	}
	healthy, err := env.backend.Fork(ctx, env.target, ownerRequest("recovery-healthy"))
	if err != nil {
		t.Fatalf("fork healthy branch: %v", err)
	}
	failingHandle, err := branchHandleOf(env.backend, failing.Version)
	if err != nil {
		t.Fatalf("resolve failing branch root: %v", err)
	}
	healthyHandle, err := branchHandleOf(env.backend, healthy.Version)
	if err != nil {
		t.Fatalf("resolve healthy branch root: %v", err)
	}
	if err := failingHandle.write("failing.txt", "failing content\n"); err != nil {
		t.Fatalf("write failing branch file: %v", err)
	}
	if err := healthyHandle.write("healthy.txt", "healthy content\n"); err != nil {
		t.Fatalf("write healthy branch file: %v", err)
	}
	if err := env.backend.Discard(ctx, failing.Version); err != nil {
		t.Fatalf("discard failing branch: %v", err)
	}
	restored, err := env.backend.Observe(ctx, env.target)
	if err != nil {
		t.Fatalf("observe target version: %v", err)
	}
	result.BaseRestored = restored.Digest == base.Digest
	result.RestoredDigest = shortDigest(restored.Digest)
	if _, err := os.Stat(filepath.Join(healthyHandle.root, "healthy.txt")); err != nil {
		result.CollateralDamage = true
	}
	return result
}

// runDeliveryScenario 测量可审阅交付单元的产出成本。
func runDeliveryScenario(t *testing.T, env scenarioEnv) deliveryReport {
	t.Helper()
	ctx, cancel := contextWithTimeout(t, verifierTimeout)
	defer cancel()
	result := deliveryReport{}
	branch, err := env.backend.Fork(ctx, env.target, ownerRequest("delivery"))
	if err != nil {
		t.Fatalf("fork branch: %v", err)
	}
	handle, err := branchHandleOf(env.backend, branch.Version)
	if err != nil {
		t.Fatalf("resolve branch root: %v", err)
	}
	if err := handle.write("delivery.txt", "delivery content\n"); err != nil {
		t.Fatalf("write branch file: %v", err)
	}
	if err := env.backend.commit(branch.Version); err != nil && !errors.Is(err, ports.ErrWorkspaceUnsupported) {
		t.Fatalf("commit branch version: %v", err)
	}
	unit, content, elapsed, err := env.backend.deliveryUnit(branch.Version)
	if err != nil {
		t.Fatalf("produce delivery unit: %v", err)
	}
	result.Unit = unit
	result.Available = len(content) > 0
	result.Millis = float64(elapsed.Microseconds()) / 1000
	result.Bytes = int64(len(content))
	return result
}

// measureDisk 统计四个分支占用的工作区内容字节与 Git 元数据字节。
func measureDisk(t *testing.T, env scenarioEnv) diskReport {
	t.Helper()
	result := diskReport{}
	ctx, cancel := contextWithTimeout(t, verifierTimeout)
	defer cancel()
	for index := 0; index < experimentBranchCount; index++ {
		if _, err := env.backend.Fork(ctx, env.target, ownerRequest(fmt.Sprintf("disk-%d", index))); err != nil {
			t.Fatalf("fork disk branch: %v", err)
		}
	}
	roots, err := env.backend.physicalRoots()
	if err != nil {
		t.Fatalf("resolve physical roots: %v", err)
	}
	for _, root := range roots {
		_, _, bytes, err := contentDigest(root)
		if err != nil {
			t.Fatalf("measure branch content: %v", err)
		}
		result.ContentBytes += bytes
		info, err := os.Stat(filepath.Join(root, "deps", "blob.bin"))
		if err == nil {
			result.DependencyCopies++
			result.DependencyBytes = info.Size()
		}
	}
	result.MetadataBytes = metadataBytes(t, env)
	result.Roots = len(roots)
	return result
}

// metadataBytes 统计后端的 Git 元数据占用；worktree 方案只保留一份对象库。
func metadataBytes(t *testing.T, env scenarioEnv) int64 {
	t.Helper()
	root := env.metadataRoot
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return 0
	}
	var total int64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		total += fileInfo.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("measure git metadata: %v", err)
	}
	return total
}

// mustRoot 返回版本的物理目录，缺失时终止测试。
func mustRoot(t *testing.T, backend branchBackend, version ports.WorkspaceVersion) string {
	t.Helper()
	root, err := backend.root(version)
	if err != nil {
		t.Fatalf("resolve version root: %v", err)
	}
	return root
}

// millisSince 返回自起始时刻经过的毫秒数。
func millisSince(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000
}

// shortDigest 返回摘要的前缀，便于人工核对而不暴露完整内容。
func shortDigest(digest ports.WorkspaceDigest) string {
	value := string(digest)
	if len(value) > 19 {
		return value[:19]
	}
	return value
}

// containsString 判断字符串集合是否包含给定值。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// writeComparisonReport 输出对照报告：默认打印到测试日志，设置了环境变量
// CAIRN_WORKSPACE_REPORT 时额外写入 JSON 文件。
func writeComparisonReport(t *testing.T, report comparisonReport) {
	t.Helper()
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("encode comparison report: %v", err)
	}
	if path := os.Getenv("CAIRN_WORKSPACE_REPORT"); path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create report directory: %v", err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write comparison report: %v", err)
		}
	}
	t.Logf("对照实验报告：\n%s", string(encoded))
}
