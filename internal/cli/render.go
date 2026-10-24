// CLI 输出渲染与退出码映射。
// Author: fxbin
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

func manifestOf(result app.RunResult) *contract.RunManifest {
	if result.Result == nil {
		return nil
	}
	return &result.Result.Manifest
}

// exitFor 将领域和应用错误映射为公开稳定的 CLI 退出码。
func exitFor(err error) int {
	var waiting *ports.WaitingApprovalError
	if errors.As(err, &waiting) {
		return ExitApproval
	}
	var denied *ports.PolicyDeniedError
	if errors.As(err, &denied) {
		return ExitDenied
	}
	if errors.Is(err, app.ErrWaitingReconcile) {
		return ExitReconcile
	}
	if errors.Is(err, app.ErrConfigDrift) {
		return ExitUsage
	}
	if errors.Is(err, app.ErrStateCorrupt) {
		return ExitCorrupt
	}
	if errors.Is(err, context.Canceled) {
		return ExitCancelled
	}
	return ExitFailed
}

func writeBuildIdentity(writer io.Writer, label string, identity domain.BuildIdentity) {
	revision := identity.Revision
	if revision == "" {
		revision = "unknown"
	}
	fmt.Fprintf(writer, "%s: revision=%s dirty=%s flavor=%s fingerprint=%s\n", label, revision, identity.Dirty, identity.Flavor, identity.Fingerprint)
}

// writeRunManifest 输出运行产品合同 manifest 的非敏感身份块；缺失字段直接
// 省略，不伪造默认值，任务正文与本地路径绝不出现。
func writeRunManifest(writer io.Writer, manifest contract.RunManifest) {
	fmt.Fprintf(writer, "task: %s", manifest.Task.Kind)
	if manifest.Task.Source != "" {
		fmt.Fprintf(writer, " (%s)", manifest.Task.Source)
	}
	if manifest.Task.Benchmark != nil {
		fmt.Fprintf(writer, " id=%s", manifest.Task.Benchmark.TaskID)
	}
	fmt.Fprintf(writer, " prompt=%s\n", manifest.Task.PromptDigest)
	if provider := manifest.Provider; provider.Kind != "" || provider.BaseURL != "" || provider.Model != "" {
		parts := make([]string, 0, 3)
		for _, value := range []string{provider.Kind, provider.BaseURL, provider.Model} {
			if value != "" {
				parts = append(parts, value)
			}
		}
		fmt.Fprintf(writer, "provider: %s\n", strings.Join(parts, " "))
	}
	if manifest.Preset != "" || manifest.Profile != "" {
		fmt.Fprintf(writer, "preset: %s profile %s\n", manifest.Preset, manifest.Profile)
	}
	if manifest.Policy.Write != "" || manifest.Policy.Exec != "" {
		fmt.Fprintf(writer, "policy: write=%s exec=%s\n", manifest.Policy.Write, manifest.Policy.Exec)
	}
	if manifest.Verification.Fingerprint != "" {
		fmt.Fprintf(writer, "verification plan: %d command(s) %s\n", manifest.Verification.Commands, manifest.Verification.Fingerprint)
	}
}

// resolveTrialActivation 按显式旗标解析沙箱激活的压缩变体并返回覆盖后
// 的配置；未选择加入时原样返回。四命令共用，保证指纹与漂移守卫一致。
func writeJSONChecked(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	if _, err := fmt.Fprintln(writer, string(encoded)); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}

// writeJSONOutput 将 JSON 输出失败统一写入 stderr，并映射为公开失败退出码。
func (command Command) writeJSONOutput(label string, value any) int {
	if err := writeJSONChecked(command.Stdout, value); err != nil {
		markOutputErrorReported(command.Stdout)
		fmt.Fprintf(command.Stderr, "%s output failed: %v\n", label, err)
		return ExitFailed
	}
	return ExitOK
}

// memoryPromotion 分派版本域的固定动作：activate 显式进入 sandbox，rollback
// 回退当前 sandbox 版本，list 只读列出版本史与激活指针。
