package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// manifestArtifactRoot 返回指定工作区制品库的根目录；与 openArtifactStore
// 使用同一路径布局，保证投影读取与写入视角一致。
func manifestArtifactRoot(stateDir, workspaceHash string) string {
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	return filepath.Join(stateDir, "workspaces", workspaceHash, "artifacts")
}

// readFrozenRunConfig 只读加载 run_created 引用的冻结配置制品，并校验内容
// 摘要一致后返回其非敏感内容。旧 Run 没有 config_ref、制品缺失或被篡改时
// 返回 nil 而不是失败——manifest 投影对缺失事实保持留空，不阻塞诊断。
func readFrozenRunConfig(events []domain.Event, stateDir, workspaceHash string) json.RawMessage {
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return nil
	}
	var payload struct {
		ConfigRef *struct {
			SHA256 string `json:"sha256"`
		} `json:"config_ref"`
	}
	if json.Unmarshal(events[0].Payload, &payload) != nil || payload.ConfigRef == nil {
		return nil
	}
	digest := strings.TrimPrefix(payload.ConfigRef.SHA256, "sha256:")
	if len(digest) != sha256.Size*2 {
		return nil
	}
	content, err := os.ReadFile(filepath.Join(manifestArtifactRoot(stateDir, workspaceHash), "sha256", digest[:2], digest))
	if err != nil {
		return nil
	}
	actual := sha256.Sum256(content)
	if hex.EncodeToString(actual[:]) != digest {
		return nil
	}
	return content
}

// runManifest 投影一次普通运行的产品合同 manifest：任务身份来自 run_created
// 的任务全文摘要，配置侧字段来自冻结配置制品。任何解码失败都视为状态损坏。
func runManifest(events []domain.Event, projection domain.RunProjection, stateDir, workspaceHash string) (contract.RunManifest, error) {
	return contract.ManifestFromRun(events, contract.FreeformTaskSpec(projection.Task), readFrozenRunConfig(events, stateDir, workspaceHash))
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
	return contract.ResultFromRun(events, projection, contract.FreeformTaskSpec(projection.Task), readFrozenRunConfig(events, stateDir, workspaceHash))
}
