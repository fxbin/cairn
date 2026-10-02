// 本文件补充 M4 的演化候选用例：创建前执行与 doctor 同等的全库验证和
// 来源派生版本复核；变体参数由人工显式提供，绝不自动生成候选。
//
// Author: fxbin
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	memstoresqlite "github.com/fxbin/cairn/internal/adapters/memstore/sqlite"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

// MemoryEvolutionCreateOptions 描述创建演化候选的输入边界：闭合变体参数、
// 精确来源版本复合键与基线指纹；不接受任务正文或自由文本。提供评测集指纹
// 时候选以 ready 形态建档（评测集随创建显式冻结），缺省仍为 draft。
type MemoryEvolutionCreateOptions struct {
	Workspace             string
	StateDir              string
	Config                config.Config
	SourceDerivedID       string
	SourceConsolidationID string
	BaselineFingerprint   string
	EvalSetFingerprint    string
	NoteDigest            string
	Variant               memory.CompactionParamsVariant
	TTL                   time.Duration
	Cooldown              time.Duration
}

// MemoryEvolutionActionOptions 描述拒绝/作废等迁移动作的封闭输入。
type MemoryEvolutionActionOptions struct {
	CandidateID string
	Reason      string
	Workspace   string
	StateDir    string
	Config      config.Config
	Cooldown    time.Duration
}

// MemoryEvolutionInspectOptions 描述演化候选的只读解释输入。
type MemoryEvolutionInspectOptions struct {
	CandidateID string
	Workspace   string
	StateDir    string
	Config      config.Config
}

// CreateEvolutionCandidate 在工作区锁内创建一条演化候选：先做全库跨库验证，
// 再复核来源派生版本存在、active、未过期且属于当前工作区，最后由变体参数确
// 定性派生指纹并写入登记事务。携带评测集指纹时以 ready 形态建档（评测集随
// 创建显式冻结），否则为 draft。提交后再运行与 Doctor 一致的只读验证；缺少
// 记忆库或来源失效即失败关闭。
func (service Service) CreateEvolutionCandidate(ctx context.Context, options MemoryEvolutionCreateOptions) (memory.EvolutionCandidate, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	source := memory.EvolutionSourceRef{DerivedID: options.SourceDerivedID, ConsolidationID: options.SourceConsolidationID}
	derivedInspect, err := store.InspectDerived(ctx, source.DerivedID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if derivedInspect.Candidate.ConsolidationID != source.ConsolidationID || derivedInspect.Candidate.WorkspaceHash != workspaceHash {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	if derivedInspect.Candidate.Status != memory.DerivedStatusActive {
		return memory.EvolutionCandidate{}, errors.New("evolution source derived version is not active")
	}
	now := time.Now().UTC()
	if !derivedInspect.Candidate.ExpiresAt.After(now) {
		return memory.EvolutionCandidate{}, errors.New("evolution source derived version is expired")
	}
	variantFingerprint, err := memory.VariantFingerprint(memory.TargetKindContextCompactionParams, options.Variant)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	variantDigest, err := memory.VariantSpecDigest(options.Variant)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	status := memory.CandidateStateDraft
	if options.EvalSetFingerprint != "" {
		if !memory.DigestHexShape(options.EvalSetFingerprint) {
			return memory.EvolutionCandidate{}, errors.New("eval set fingerprint must be a sha256 digest")
		}
		status = memory.CandidateStateReady
	}
	ttl, err := memory.NormalizeCandidateTTL(options.TTL)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	candidate := memory.EvolutionCandidate{
		ID:                  memory.EvolutionCandidateID(workspaceHash, source, memory.TargetKindContextCompactionParams, variantFingerprint),
		SchemaVersion:       memory.EvolutionSchemaVersion,
		WorkspaceHash:       workspaceHash,
		Source:              source,
		TargetKind:          memory.TargetKindContextCompactionParams,
		VariantSpecSHA256:   variantDigest,
		VariantFingerprint:  variantFingerprint,
		BaselineFingerprint: options.BaselineFingerprint,
		EvalSetFingerprint:  options.EvalSetFingerprint,
		CreatedAt:           now,
		ExpiresAt:           now.Add(ttl),
		Status:              status,
		NoteDigest:          options.NoteDigest,
	}
	created, err := store.CreateEvolutionCandidate(ctx, candidate, now)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return created, nil
}

// RejectEvolutionCandidate 在工作区锁内以封闭理由拒绝候选并落入冷却；
// 全库验证先行，跨工作区标识失败关闭；提交后再次完整验证，不向调用方返回
// 不能由 Doctor 接受的持久状态。
func (service Service) RejectEvolutionCandidate(ctx context.Context, options MemoryEvolutionActionOptions) (memory.EvolutionCandidate, error) {
	if !memory.ValidEvolutionRejectionReason(options.Reason) {
		return memory.EvolutionCandidate{}, errors.New("evolution rejection reason is invalid")
	}
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectEvolutionCandidate(ctx, options.CandidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	rejected, err := store.RejectEvolutionCandidate(ctx, options.CandidateID, options.Reason, time.Now().UTC(), options.Cooldown)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return rejected, nil
}

// InvalidateEvolutionCandidate 在工作区锁内执行受限原子作废：来源被合法替代
// 后全库校验会失败关闭，因此本用例不以前置全库 Check 阻断自身——它走存储层
// 的资格受限路径（同事务证明该候选来源确已失效），提交后再执行完整跨库验证
// 断言一致性已恢复；断言失败说明存在本路径未覆盖的其他损坏，原样上报。
func (service Service) InvalidateEvolutionCandidate(ctx context.Context, options MemoryEvolutionActionOptions) (memory.EvolutionCandidate, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer store.Close()
	inspected, err := store.InspectEvolutionCandidate(ctx, options.CandidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	candidate, err := store.InvalidateEvolutionCandidateForInactiveSource(ctx, options.CandidateID, time.Now().UTC())
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return candidate, nil
}

// ExpireEvolutionCandidate 在工作区锁内把保留期耗尽的候选显式置为 expired。
// 语义不对称说明：来源派生版本先于候选自然过期时，本用例的前置全库校验会
// 因"来源过期"失败关闭——该场景的唯一修复路径是 InvalidateEvolutionCandidate
// 的受限作废（其资格条件覆盖来源过期），本用例不复制该例外。提交后再次
// 完整验证，确保显式到期不会返回失真的持久状态。
func (service Service) ExpireEvolutionCandidate(ctx context.Context, options MemoryEvolutionActionOptions) (memory.EvolutionCandidate, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectEvolutionCandidate(ctx, options.CandidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	expired, err := store.ExpireEvolutionCandidate(ctx, options.CandidateID, time.Now().UTC())
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return expired, nil
}

// InspectEvolutionCandidate 只读返回候选与审计历史；跨工作区失败关闭。
func (service Service) InspectEvolutionCandidate(ctx context.Context, options MemoryEvolutionInspectOptions) (memstoresqlite.EvolutionInspectResult, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memstoresqlite.EvolutionInspectResult{}, err
	}
	store, err := openReadOnlyMemory(options.StateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return memstoresqlite.EvolutionInspectResult{}, memory.ErrNotFound
		}
		return memstoresqlite.EvolutionInspectResult{}, fmt.Errorf("%w: open memory store: %v", ErrStateCorrupt, err)
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memstoresqlite.EvolutionInspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectEvolutionCandidate(ctx, options.CandidateID)
	if err != nil {
		return memstoresqlite.EvolutionInspectResult{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memstoresqlite.EvolutionInspectResult{}, memory.ErrNotFound
	}
	return inspected, nil
}

// MemoryEvaluationStartOptions 描述为演化候选启动评测的输入：评测身份由
// memory.EvaluationID 从冻结五元组派生，携带的冻结字段必须与候选登记一致。
type MemoryEvaluationStartOptions struct {
	Evaluation memory.Evaluation
	Binding    memory.EvaluationBinding
	Workspace  string
	StateDir   string
	Config     config.Config
}

// StartEvolutionEvaluation 在工作区锁内执行评测启动事务：来源活性、评测集
// 指纹与候选登记一致性与身份派生复核都在该事务内完成；租约授权属于启动前
// 的编排层职责（先于本调用）。事务前后各执行一次全库只读验证，保证无隐藏
// 破坏被带过边界。
func (service Service) StartEvolutionEvaluation(ctx context.Context, options MemoryEvaluationStartOptions) (memory.Evaluation, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.Evaluation{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Evaluation{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Evaluation{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.Evaluation{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectEvolutionCandidate(ctx, options.Evaluation.CandidateID)
	if err != nil {
		return memory.Evaluation{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memory.Evaluation{}, memory.ErrNotFound
	}
	started, err := store.StartEvaluation(ctx, options.Evaluation.CandidateID, options.Evaluation, options.Binding, time.Now().UTC())
	if err != nil {
		return memory.Evaluation{}, err
	}
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.Evaluation{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return started, nil
}

// FinishEvolutionEvaluation 在工作区锁内执行评测闭合事务：结论四态映射到
// 候选终态并写入证据摘要引用，事务前后全库只读验证确保零残留。
func (service Service) FinishEvolutionEvaluation(ctx context.Context, evaluationID string, workspace, stateDir string, configuration config.Config, status string, suiteResultDigest string) (memory.Evaluation, error) {
	if !memory.ValidEvaluationStatus(status) || status == memory.EvaluationStatusRunning {
		return memory.Evaluation{}, errors.New("evaluation conclusion must be one of passed, failed, inconclusive or invalid")
	}
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		return memory.Evaluation{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(stateDir, workspaceHash)
	if err != nil {
		return memory.Evaluation{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(stateDir, workspaceHash)
	if err != nil {
		return memory.Evaluation{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, stateDir, workspaceHash); err != nil {
		return memory.Evaluation{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	concluded, err := store.FinishEvaluation(ctx, evaluationID, status, suiteResultDigest, time.Now().UTC())
	if err != nil {
		return memory.Evaluation{}, err
	}
	if err := verifyReadOnlyMemoryStore(ctx, store, stateDir, workspaceHash); err != nil {
		return memory.Evaluation{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return concluded, nil
}

// MemoryPromotionOptions 描述一次人工晋升决策：版本先以 inactive 形式落账，
// approval fingerprint 必须与完整冻结元组精确匹配。指标摘要由调用方从字节
// 摘要校验过的套件结果正文提取；服务层还会独立核对套件与记录版本。
type MemoryPromotionOptions struct {
	Promotion           memory.Promotion
	ApprovalFingerprint string
	SuiteStateRoot      string
	Workspace           string
	StateDir            string
	Config              config.Config
}

// trustedPromotionSuite 只解码晋升服务需要的套件身份与成功语义版本；原始
// 字节摘要仍与评测账本精确绑定，其他指标继续由 CLI 的完整套件投影提取。
type trustedPromotionSuite struct {
	SchemaVersion int    `json:"schema_version"`
	EvaluationID  string `json:"evaluation_id"`
	CandidateID   string `json:"candidate_id"`
	Status        string `json:"status"`
	Records       []struct {
		SchemaVersion int `json:"schema_version"`
	} `json:"records"`
}

// verifyTrustedPromotionSuite 在服务写入前读取冻结套件正文，要求摘要、
// 身份、结论和每条记录均满足当前可信成功语义。缺失目录不能降级为旧摘要。
func verifyTrustedPromotionSuite(options MemoryPromotionOptions) error {
	if options.SuiteStateRoot == "" || !memory.DigestHexShape(options.Promotion.EvaluationID) {
		return errors.New("promotion requires a suite state root and digest-shaped evaluation identity")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(options.Promotion.EvaluationID, "sha256:")); err != nil {
		return errors.New("promotion evaluation identity is not a hex digest")
	}
	resultPath := filepath.Join(options.SuiteStateRoot, "evaluations", strings.ReplaceAll(options.Promotion.EvaluationID, ":", "_"), "suite-result.json")
	content, err := os.ReadFile(resultPath)
	if err != nil {
		return fmt.Errorf("read promotion suite result: %w", err)
	}
	digest := sha256.Sum256(content)
	if "sha256:"+hex.EncodeToString(digest[:]) != options.Promotion.SuiteResultDigest {
		return errors.New("promotion suite result bytes diverge from the evaluated digest")
	}
	var result trustedPromotionSuite
	if err := json.Unmarshal(content, &result); err != nil {
		return fmt.Errorf("decode promotion suite result: %w", err)
	}
	if result.EvaluationID != options.Promotion.EvaluationID || result.CandidateID != options.Promotion.CandidateID || result.Status != memory.EvaluationStatusPassed || result.SchemaVersion != sandboxeval.TrustedResultSchemaVersion || len(result.Records) == 0 {
		return errors.New("promotion suite does not carry a passed trusted-evidence result for this evaluation")
	}
	for _, record := range result.Records {
		if record.SchemaVersion != sandboxeval.TrustedResultSchemaVersion {
			return errors.New("promotion suite contains a legacy run success record")
		}
	}
	return nil
}

// PromoteEvolutionCandidate 在工作区锁内写入人工决策与 inactive 版本：前后
// 双全库校验，绝不推进 sandbox 激活指针。审批指纹由存储层与全部冻结输入
// 重算比对，任何漂移整体回滚。
func (service Service) PromoteEvolutionCandidate(ctx context.Context, options MemoryPromotionOptions) (memory.Promotion, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.Promotion{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Promotion{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Promotion{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.Promotion{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectEvolutionCandidate(ctx, options.Promotion.CandidateID)
	if err != nil {
		return memory.Promotion{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memory.Promotion{}, memory.ErrNotFound
	}
	if err := verifyTrustedPromotionSuite(options); err != nil {
		return memory.Promotion{}, err
	}
	promoted, err := store.PromoteCandidate(ctx, options.Promotion, options.ApprovalFingerprint, time.Now().UTC())
	if err != nil {
		return memory.Promotion{}, err
	}
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.Promotion{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return promoted, nil
}

// MemoryPromotionActivationOptions 描述显式 sandbox 激活的输入。环境在 M4
// 固定为 sandbox，调用方不能传入生产或其他环境。
type MemoryPromotionActivationOptions struct {
	TargetKind  string
	PromotionID string
	Workspace   string
	StateDir    string
	Config      config.Config
}

// ActivateEvolutionPromotion 在工作区锁内把已批准的 inactive 版本显式激活
// 到 sandbox；前后全库校验确保该动作不越过预定回退上下文。
func (service Service) ActivateEvolutionPromotion(ctx context.Context, options MemoryPromotionActivationOptions) (memory.Promotion, memory.Activation, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	promotion, activation, err := store.ActivatePromotion(ctx, options.TargetKind, options.PromotionID, time.Now().UTC())
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return promotion, activation, nil
}

// MemoryPromotionRollbackOptions 描述一次 sandbox 激活回退的输入：目标类型
// 与必须非空的封闭理由。
type MemoryPromotionRollbackOptions struct {
	TargetKind string
	Reason     string
	Workspace  string
	StateDir   string
	Config     config.Config
}

// RollbackEvolutionPromotion 在工作区锁内执行 sandbox 回退事务：把激活指针
// 移回决策固定的 previous version（首版回产品基线）并把版本置 revoked。
func (service Service) RollbackEvolutionPromotion(ctx context.Context, options MemoryPromotionRollbackOptions) (memory.Promotion, memory.Activation, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	promotion, activation, err := store.RollbackPromotion(ctx, options.TargetKind, options.Reason, time.Now().UTC())
	if err != nil {
		return memory.Promotion{}, memory.Activation{}, err
	}
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.Promotion{}, memory.Activation{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return promotion, activation, nil
}

// InspectEvolutionPromotions 只读返回目标类型的版本史与当前激活指针。
func (service Service) InspectEvolutionPromotions(ctx context.Context, targetKind, workspace, stateDir string, configuration config.Config) ([]memory.Promotion, memory.Activation, bool, error) {
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		return nil, memory.Activation{}, false, err
	}
	store, err := openReadOnlyMemory(stateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, memory.Activation{}, false, memory.ErrNotFound
		}
		return nil, memory.Activation{}, false, fmt.Errorf("%w: open memory store: %v", ErrStateCorrupt, err)
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, stateDir, workspaceHash); err != nil {
		return nil, memory.Activation{}, false, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	promotions, err := store.ListPromotions(targetKind)
	if err != nil {
		return nil, memory.Activation{}, false, err
	}
	activation, found, err := store.ActivationFor(targetKind)
	if err != nil {
		return nil, memory.Activation{}, false, err
	}
	return promotions, activation, found, nil
}

// InspectMemoryTelemetry 只读返回记忆体系聚合遥测快照：跨表只读聚合，零
// 正文零路径零密钥。库缺失映射 ErrNotFound，一致性破坏映射 ErrStateCorrupt。
func (service Service) InspectMemoryTelemetry(ctx context.Context, workspace, stateDir string, configuration config.Config) (*memstoresqlite.TelemetrySnapshot, error) {
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		return nil, err
	}
	store, err := openReadOnlyMemory(stateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, memory.ErrNotFound
		}
		return nil, fmt.Errorf("%w: open memory store: %v", ErrStateCorrupt, err)
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, stateDir, workspaceHash); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return store.TelemetrySnapshot(ctx)
}
