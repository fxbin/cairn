package benchmark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// mirrorRoot 返回本地 git 对象库根目录。它派生自显式给出的 workspaces 根，
// 与任务套件目录同级持久存在，跨套件复用；本身仍是 Git 忽略的本地资产。
func mirrorRoot(workspacesRoot string) string {
	return filepath.Join(workspacesRoot, "_mirror")
}

// mirrorDirectory 返回单个来源仓库的本地镜像路径；以仓库规范 URL 的稳定
// 摘要做后缀，避免同名仓库冲突，也避免把远程字符串直接当目录名。
func mirrorDirectory(task Task, workspacesRoot string) string {
	digest := sha256.Sum256([]byte(task.Repository.URL))
	suffix := hex.EncodeToString(digest[:6])
	return filepath.Join(mirrorRoot(workspacesRoot), safePathComponent(task.Repository.Name)+"-"+suffix+".git")
}

// ensureMirrorAtBaseline 保证本地镜像已包含任务基线提交：基线已在本地时不
// 发起任何网络请求（本地优先）；缺失时才从远程镜像克隆或增量 fetch（缺则
// 远程），并经既有的受限环境与显式代理通道执行。
func ensureMirrorAtBaseline(ctx context.Context, mirror, remoteURL, baselineCommit, prepareProxy string) error {
	if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
		return fmt.Errorf("create mirror root: %w", err)
	}
	info, err := os.Stat(mirror)
	if err == nil && info.IsDir() {
		if mirrorHasCommit(ctx, mirror, baselineCommit) {
			return nil
		}
		if err := retryTransient(func() error {
			return runProcessWithProxy(ctx, mirror, []string{"git", "fetch", "origin"}, prepareProxy)
		}); err != nil {
			return fmt.Errorf("mirror fetch %s: %w", remoteURL, err)
		}
		if !mirrorHasCommit(ctx, mirror, baselineCommit) {
			return fmt.Errorf("baseline %s missing from mirror after fetch", baselineCommit)
		}
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect mirror: %w", err)
	}
	if err := retryTransient(func() error {
		return runProcessWithProxy(ctx, "", []string{"git", "clone", "--mirror", remoteURL, mirror}, prepareProxy)
	}); err != nil {
		// 镜像克隆失败的半成品不留存，下次从零开始，避免坏缓存阻塞。
		_ = os.RemoveAll(mirror)
		return fmt.Errorf("mirror clone %s: %w", remoteURL, err)
	}
	if !mirrorHasCommit(ctx, mirror, baselineCommit) {
		return fmt.Errorf("baseline %s missing from fresh mirror of %s", baselineCommit, remoteURL)
	}
	return nil
}

// mirrorHasCommit 只读探测本地镜像是否包含指定提交对象。
func mirrorHasCommit(ctx context.Context, mirror, commit string) bool {
	return runProcess(ctx, mirror, []string{"git", "cat-file", "-e", commit + "^{commit}"}) == nil
}

// cloneFromMirror 在全新工作区目录中从本地镜像执行免网络克隆并检出不可变
// 基线；本地克隆默认走硬链接，秒级完成。工作区与镜像互不写入，镜像只读。
func cloneFromMirror(ctx context.Context, mirror, workspace, baselineCommit string) error {
	if err := retryTransient(func() error {
		return runProcess(ctx, "", []string{"git", "clone", "--no-checkout", mirror, workspace})
	}); err != nil {
		return fmt.Errorf("clone from local mirror: %w", err)
	}
	if err := retryTransient(func() error {
		return runProcess(ctx, workspace, []string{"git", "checkout", "--detach", baselineCommit})
	}); err != nil {
		return fmt.Errorf("checkout baseline %s: %w", baselineCommit, err)
	}
	return nil
}

// mirrorCloneAtBaseline 是基线克隆的本地优先实现：先确保镜像含基线，再从
// 镜像克隆全新工作区并检出基线。调用方在克隆后仍以 git rev-parse 校验
// HEAD == baseline_commit，内容完整性由 git 对象哈希保证。
func mirrorCloneAtBaseline(ctx context.Context, task Task, workspace, prepareProxy, workspacesRoot string) error {
	if task.Repository.BaselineCommit == "" {
		return errors.New("baseline commit is required for mirror clone")
	}
	mirror := mirrorDirectory(task, workspacesRoot)
	if err := ensureMirrorAtBaseline(ctx, mirror, task.Repository.URL, task.Repository.BaselineCommit, prepareProxy); err != nil {
		return err
	}
	return cloneFromMirror(ctx, mirror, workspace, task.Repository.BaselineCommit)
}
