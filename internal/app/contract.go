package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// readFrozenRunConfig 只读加载 run_created 引用的冻结配置制品，并校验内容
// 摘要一致后返回其非敏感内容。没有 config_ref 的历史 Run 保持兼容地返回
// nil；一旦存在引用，缺失、格式错误或内容篡改都必须失败关闭，不能把
// 不完整的冻结配置伪装成合法的空投影。
func readFrozenRunConfig(events []domain.Event, stateDir, workspaceHash string) (json.RawMessage, error) {
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return nil, nil
	}
	var payload struct {
		ConfigRef *artifact.Ref `json:"config_ref"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode run_created config reference: %w", err)
	}
	if payload.ConfigRef == nil {
		return nil, nil
	}
	root := manifestArtifactRoot(stateDir, workspaceHash)
	if err := checkArtifactReference(root, *payload.ConfigRef); err != nil {
		return nil, fmt.Errorf("verify frozen run config: %w", err)
	}
	digest := strings.TrimPrefix(payload.ConfigRef.SHA256, "sha256:")
	content, err := os.ReadFile(filepath.Join(root, "sha256", digest[:2], digest))
	if err != nil {
		return nil, fmt.Errorf("read frozen run config: %w", err)
	}
	return content, nil
}

// runManifest 投影一次普通运行的产品合同 manifest：任务身份来自 run_created
// 的任务全文摘要，配置侧字段来自冻结配置制品。任何解码失败都视为状态损坏。
func runManifest(events []domain.Event, projection domain.RunProjection, stateDir, workspaceHash string) (contract.RunManifest, error) {
	frozenConfig, err := readFrozenRunConfig(events, stateDir, workspaceHash)
	if err != nil {
		return contract.RunManifest{}, err
	}
	return contract.ManifestFromRun(events, contract.FreeformTaskSpec(projection.Task), frozenConfig)
}

// projectRunResult 在运行结束后从日志投影完整的 contract.Result；ctx 使用
// 调用方给出的上下文（失败路径应传入 context.Background 以避免读取被取消）。
func projectRunResult(ctx context.Context, journal *sqlite.Journal, stateDir, workspaceHash string, runID domain.RunID) (contract.Result, error) {
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return contract.Result{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return contract.Result{}, err
	}
	frozenConfig, err := readFrozenRunConfig(events, stateDir, workspaceHash)
	if err != nil {
		return contract.Result{}, err
	}
	return contract.ResultFromRun(events, projection, contract.FreeformTaskSpec(projection.Task), frozenConfig)
}
