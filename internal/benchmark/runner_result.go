// 尝试/资格/准备结果辅助。
// Author: fxbin
package benchmark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"
)

func (runner Runner) captureDiff(ctx context.Context, workspace, resultDirectory string, result AttemptResult) AttemptResult {
	diff, err := runProcessOutput(ctx, workspace, []string{"git", "diff", "--binary"})
	if err != nil {
		return result
	}
	path := filepath.Join(resultDirectory, resultFileName(result.TaskID, result.Attempt)+".diff")
	if err := os.WriteFile(path, diff, 0o600); err != nil {
		return result
	}
	digest := sha256.Sum256(diff)
	result.DiffPath = filepath.Base(path)
	result.DiffSHA256 = "sha256:" + hex.EncodeToString(digest[:])
	return result
}

// fail 在不丢失已收集证据的前提下标记可汇总的失败类别。
func (result AttemptResult) fail(class string, err error) AttemptResult {
	result.Outcome = "failed"
	result.FailureClass = class
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// finish 固定尝试结束时间，避免结果写入时间混入 Agent 墙钟预算。
func (result AttemptResult) finish(finished time.Time) AttemptResult {
	result.FinishedAt = finished
	return result
}

// fail 标记资格检查的稳定失败类别；结果中只保留有限摘要，绝不持久化 clone、
// verifier 或 Docker 的底层诊断。
func (result QualificationResult) fail(class string, err error) QualificationResult {
	result.Outcome = "failed"
	result.FailureClass = class
	result.Error = preparationErrorSummary(class, err)
	return result
}

// finish 固定资格检查结束时间，使其与 Agent 墙钟和模型预算完全分离。
func (result QualificationResult) finish(finished time.Time) QualificationResult {
	result.FinishedAt = finished
	return result
}

// fail 标记正式 Agent workspace 的准备失败；它采用与 qualification 相同的
// 脱敏错误摘要，以便 suite 环境无效报告不泄露私有 verifier 或命令输出。
func (result PreparationResult) fail(class string, err error) PreparationResult {
	result.Outcome = "failed"
	result.FailureClass = class
	result.Error = preparationErrorSummary(class, err)
	return result
}

// finish 固定正式准备结束时间，使环境用时不混入 Agent 墙钟预算。
func (result PreparationResult) finish(finished time.Time) PreparationResult {
	result.FinishedAt = finished
	return result
}

// preparationErrorSummary 将底层错误收敛为固定非敏感分类；调用方仍可从
// FailureClass 判断失败阶段，但不会把 URL、路径、命令输出或凭据写入结果。
func preparationErrorSummary(class string, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "preparation timed out"
	}
	switch class {
	case "qualification_prepare", "preparation_prepare":
		return "source preparation failed"
	case "qualification_verifier", "preparation_verifier":
		return "private verifier preparation failed"
	case "qualification_bootstrap", "preparation_bootstrap":
		return "bootstrap preparation failed"
	case "qualification_sandbox", "preparation_sandbox":
		return "task sandbox preparation failed"
	case "preparation_config":
		return "task configuration preparation failed"
	default:
		return "environment preparation failed"
	}
}

// now 返回可测试的 UTC 时钟；零值 Runner 使用系统时间。
