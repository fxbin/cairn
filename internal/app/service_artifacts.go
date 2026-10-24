// 制品引用完整性检查与只读取回。
// Author: fxbin
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/tools"
)

// checkArtifactReferences 验证日志 payload 引用的每一个制品文件及其摘要。
func checkArtifactReferences(ctx context.Context, journal *sqlite.Journal, stateDir, workspaceHash string) error {
	runIDs, err := journal.RunIDs(ctx)
	if err != nil {
		return err
	}
	for _, runID := range runIDs {
		events, err := journal.Load(ctx, runID)
		if err != nil {
			return err
		}
		if runID == domain.QueueRunID {
			// 队列命名空间只校验内容制品引用，不做 run 冻结合同校验。
			if err := checkRunArtifactReferences(events, stateDir, workspaceHash); err != nil {
				return err
			}
			continue
		}
		if err := checkRunArtifactReferences(events, stateDir, workspaceHash); err != nil {
			return err
		}
		if err := checkFrozenRunContract(events, stateDir, workspaceHash); err != nil {
			return err
		}
	}
	return nil
}

// checkRunArtifactReferences 校验单次 Run 的所有制品引用。Evidence 只能读取
// 指定 Run，不能因其他 Run 的损坏而失去可用性；诊断则复用本函数遍历全部 Run。
func checkRunArtifactReferences(events []domain.Event, stateDir, workspaceHash string) error {
	root := manifestArtifactRoot(stateDir, workspaceHash)
	for _, event := range events {
		var payload struct {
			ResultRef              *artifact.Ref   `json:"result_ref"`
			PlanRef                *artifact.Ref   `json:"plan_ref"`
			ConfigRef              *artifact.Ref   `json:"config_ref"`
			EvidenceRef            *artifact.Ref   `json:"evidence_ref"`
			PlannedBeforeRef       *artifact.Ref   `json:"planned_before_ref"`
			PriorContextRef        json.RawMessage `json:"prior_context_ref"`
			ProjectInstructionsRef json.RawMessage `json:"project_instructions_ref"`
			ContentRef             json.RawMessage `json:"content_ref"`
			ProtocolBlocksRef      json.RawMessage `json:"protocol_blocks_ref"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		if len(payload.ProjectInstructionsRef) > 0 {
			if _, err := tools.ReadProjectInstructions(&artifact.Store{Root: root}, payload.ProjectInstructionsRef); err != nil {
				return fmt.Errorf("run %s event %d: %w", event.RunID, event.Seq, err)
			}
		}
		for name, raw := range map[string]json.RawMessage{
			"prior_context_ref":   payload.PriorContextRef,
			"content_ref":         payload.ContentRef,
			"protocol_blocks_ref": payload.ProtocolBlocksRef,
		} {
			if len(raw) == 0 {
				continue
			}
			var reference artifact.Ref
			if string(raw) == "null" || json.Unmarshal(raw, &reference) != nil {
				return fmt.Errorf("run %s event %d: invalid %s", event.RunID, event.Seq, name)
			}
			if err := checkArtifactReference(root, reference); err != nil {
				return fmt.Errorf("run %s event %d: %w", event.RunID, event.Seq, err)
			}
		}
		// 验证计划与冻结配置是审批绑定的信任根，与结果制品同等纳入
		// 存在性与摘要校验，防止被删除或篡改后静默通过诊断；before 快照
		// 是工作区回退的恢复源，同样必须完整在档。
		for _, reference := range []*artifact.Ref{payload.ResultRef, payload.PlanRef, payload.ConfigRef, payload.EvidenceRef, payload.PlannedBeforeRef} {
			if reference == nil {
				continue
			}
			if err := checkArtifactReference(root, *reference); err != nil {
				return fmt.Errorf("run %s event %d: %w", event.RunID, event.Seq, err)
			}
		}
	}
	return nil
}

// checkFrozenRunContract 在恢复入口产生任何新事件或执行前，严格校验
// run_created 引用的冻结配置属于当前合同，并与运行配置指纹交叉绑定。
func checkFrozenRunContract(events []domain.Event, stateDir, workspaceHash string) error {
	frozenConfig, err := readFrozenRunConfig(events, stateDir, workspaceHash)
	if err != nil {
		return err
	}
	return contract.ValidateFrozenRunConfig(events, frozenConfig)
}

// manifestArtifactRoot 返回指定工作区制品库的根目录；与 openArtifactStore
// 使用同一路径布局，保证投影读取与写入视角一致。
func manifestArtifactRoot(stateDir, workspaceHash string) string {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	return filepath.Join(stateDir, "workspaces", workspaceHash, "artifacts")
}

// checkArtifactReference 验证单个制品引用的格式、存在性和内容摘要。
func checkArtifactReference(root string, reference artifact.Ref) error {
	_, err := readArtifactContent(root, reference)
	return err
}

// readArtifactContent 读取并校验制品内容：摘要格式合法、文件在档且内容哈
// 希与引用一致；失败关闭，供诊断校验与展示层只读解引用共用。
func readArtifactContent(root string, reference artifact.Ref) ([]byte, error) {
	digest := strings.TrimPrefix(reference.SHA256, "sha256:")
	if len(digest) != sha256.Size*2 {
		return nil, fmt.Errorf("invalid artifact digest %q", reference.SHA256)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, fmt.Errorf("decode artifact digest: %w", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "sha256", digest[:2], digest))
	if err != nil {
		return nil, fmt.Errorf("read referenced artifact: %w", err)
	}
	actual := sha256.Sum256(content)
	if hex.EncodeToString(actual[:]) != digest {
		return nil, errors.New("referenced artifact hash mismatch")
	}
	return content, nil
}

// ReadRunArtifact 只读解引用当前工作区制品库中的一个制品：内容哈希必须与
