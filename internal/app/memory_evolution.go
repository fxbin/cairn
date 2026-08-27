// 本文件补充 M4 的演化候选用例：创建前执行与 doctor 同等的全库验证和
// 来源派生版本复核；变体参数由人工显式提供，绝不自动生成候选。
//
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	memorysqlite "github.com/fxbin/cairn/internal/adapters/memory/sqlite"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
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
func (service Service) InspectEvolutionCandidate(ctx context.Context, options MemoryEvolutionInspectOptions) (memorysqlite.EvolutionInspectResult, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memorysqlite.EvolutionInspectResult{}, err
	}
	store, err := openReadOnlyMemory(options.StateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return memorysqlite.EvolutionInspectResult{}, memory.ErrNotFound
		}
		return memorysqlite.EvolutionInspectResult{}, fmt.Errorf("%w: open memory store: %v", ErrStateCorrupt, err)
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memorysqlite.EvolutionInspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectEvolutionCandidate(ctx, options.CandidateID)
	if err != nil {
		return memorysqlite.EvolutionInspectResult{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memorysqlite.EvolutionInspectResult{}, memory.ErrNotFound
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
