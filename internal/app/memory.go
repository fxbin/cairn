package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	memorysqlite "github.com/fxbin/cairn/internal/adapters/memory/sqlite"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/memory"
)

// MemoryCaptureOptions 描述从一个已校验终态 Run 生成固定 run_outcome 候选的输入。
// 它不接受任务、模型消息、工具参数、路径或任何自由文本。
type MemoryCaptureOptions struct {
	RunID        domain.RunID
	Workspace    string
	StateDir     string
	Config       config.Config
	TTL          time.Duration
	SupersedesID string
}

// MemoryRecallOptions 描述当前工作区的只读确定性 recall 边界。
type MemoryRecallOptions struct {
	Workspace string
	StateDir  string
	Config    config.Config
	Limit     int
}

// MemoryInspectOptions 描述当前工作区内单个候选的只读 inspect 边界。
type MemoryInspectOptions struct {
	CandidateID string
	Workspace   string
	StateDir    string
	Config      config.Config
}

// MemoryRevokeOptions 描述当前工作区内撤销候选的封闭输入；Reason 只能使用
// memory 包声明的有限枚举，绝不能携带操作者备注。
type MemoryRevokeOptions struct {
	CandidateID string
	Reason      string
	Workspace   string
	StateDir    string
	Config      config.Config
}

// CaptureMemory 在既有工作区锁内从 Journal 的已校验终态 Run 派生一个结构化候选。
// Journal 先于 Memory 存在，跨库失败只会使 capture 失败，绝不回写或改变 Run 事实。
func (service Service) CaptureMemory(ctx context.Context, options MemoryCaptureOptions) (memory.Candidate, error) {
	if options.RunID == "" {
		return memory.Candidate{}, errors.New("memory capture run ID is required")
	}
	ttl, err := memory.NormalizeTTL(options.TTL)
	if err != nil {
		return memory.Candidate{}, err
	}
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.Candidate{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Candidate{}, err
	}
	defer workspaceLock.Release()
	journal, err := openReadOnlyJournal(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Candidate{}, err
	}
	defer journal.Close()
	events, err := journal.Load(ctx, options.RunID)
	if err != nil {
		return memory.Candidate{}, err
	}
	if len(events) == 0 {
		return memory.Candidate{}, fmt.Errorf("run %s not found", options.RunID)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return memory.Candidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if !projection.Terminal() || projection.WorkspaceHash != workspaceHash {
		return memory.Candidate{}, errors.New("memory capture requires a terminal run in the current workspace")
	}
	if err := checkRunArtifactReferences(events, options.StateDir, workspaceHash); err != nil {
		return memory.Candidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	evidence, err := memoryEvidenceFromEvents(events, options.StateDir, workspaceHash)
	if err != nil {
		return memory.Candidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	candidate, err := memory.NewRunOutcomeCandidate(workspaceHash, string(options.RunID), contract.FreeformTaskSpec(projection.Task).PromptDigest, projection.Phase(), contract.ClassifyTerminal(events), contract.CompletionTrusted(projection.State, projection.VerificationSucceeded, projection.LastVerificationSeq, projection.LastMutationSeq), events[len(events)-1].OccurredAt, ttl, evidence, options.SupersedesID)
	if err != nil {
		return memory.Candidate{}, err
	}
	store, err := openMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Candidate{}, err
	}
	defer store.Close()
	return store.Capture(ctx, candidate)
}

// RecallMemory 只从当前 workspace 的既有 Memory Store 读取 active、未过期的
// run_outcome 候选；Memory 不存在时返回空集，且绝不创建数据库或注入 Agent Loop。
func (service Service) RecallMemory(ctx context.Context, options MemoryRecallOptions) ([]memory.Candidate, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return nil, err
	}
	store, err := openReadOnlyMemory(options.StateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []memory.Candidate{}, nil
		}
		return nil, fmt.Errorf("%w: open memory store: %v", ErrStateCorrupt, err)
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return store.Recall(ctx, memory.RecallOptions{WorkspaceHash: workspaceHash, Limit: options.Limit, Now: time.Now().UTC()})
}

// InspectMemory 只读返回当前 workspace 的候选与审计历史；跨 workspace 标识即使
// 猜中也失败关闭，防止把任意工作区记忆混入本地检索面。
func (service Service) InspectMemory(ctx context.Context, options MemoryInspectOptions) (memory.InspectResult, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.InspectResult{}, err
	}
	store, err := openReadOnlyMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.InspectResult{}, fmt.Errorf("%w: open memory store: %v", ErrStateCorrupt, err)
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.InspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	result, err := store.Inspect(ctx, options.CandidateID)
	if err != nil {
		return memory.InspectResult{}, err
	}
	if result.Candidate.WorkspaceHash != workspaceHash {
		return memory.InspectResult{}, memory.ErrNotFound
	}
	return result, nil
}

// RevokeMemory 在既有工作区锁内原子撤销当前 workspace 的 active 候选；缺失
// Memory Store 不会被 revoke 创建，避免把一次失败的撤销请求变成新的状态。
func (service Service) RevokeMemory(ctx context.Context, options MemoryRevokeOptions) (memory.Candidate, error) {
	if !memory.ValidRevocationReason(options.Reason) {
		return memory.Candidate{}, errors.New("memory revoke reason must be evidence_invalid or human_request")
	}
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.Candidate{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.Candidate{}, err
	}
	defer workspaceLock.Release()
	path := memoryDBPath(options.StateDir, workspaceHash)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return memory.Candidate{}, memory.ErrNotFound
		}
		return memory.Candidate{}, err
	}
	store, err := memorysqlite.Open(path)
	if err != nil {
		return memory.Candidate{}, err
	}
	defer store.Close()
	existing, err := store.Inspect(ctx, options.CandidateID)
	if err != nil {
		return memory.Candidate{}, err
	}
	if existing.Candidate.WorkspaceHash != workspaceHash {
		return memory.Candidate{}, memory.ErrNotFound
	}
	candidate, err := store.Revoke(ctx, options.CandidateID, options.Reason, time.Now().UTC())
	if err != nil {
		return memory.Candidate{}, err
	}
	return candidate, nil
}

// memoryEvidenceFromEvents 创建候选的不可逆证据定位。最后的终态 Event 永远被
// 记录；所有被记录的 artifact SHA-256 均必须由同一 Event 的固定字段实际声明。
func memoryEvidenceFromEvents(events []domain.Event, stateDir, workspaceHash string) ([]memory.EventEvidenceRef, error) {
	if len(events) == 0 {
		return nil, errors.New("memory evidence requires run events")
	}
	terminal := events[len(events)-1]
	references := []memory.EventEvidenceRef{{RunID: string(terminal.RunID), EventSeq: terminal.Seq, EventHash: "sha256:" + terminal.EventHash, EventType: string(terminal.Type)}}
	root := manifestArtifactRoot(stateDir, workspaceHash)
	for _, event := range events {
		artifacts, err := eventArtifactRefs(event)
		if err != nil {
			return nil, err
		}
		for _, reference := range artifacts {
			if err := checkArtifactReference(root, reference); err != nil {
				return nil, fmt.Errorf("run %s event %d artifact: %w", event.RunID, event.Seq, err)
			}
			references = append(references, memory.EventEvidenceRef{RunID: string(event.RunID), EventSeq: event.Seq, EventHash: "sha256:" + event.EventHash, EventType: string(event.Type), ArtifactSHA256: reference.SHA256})
		}
	}
	return references, nil
}

// eventArtifactRefs 从已受领域 schema 约束的 payload 提取已知 artifact.Ref 字段。
// 返回值仅在调用边界短暂保留 Ref；Memory Store 永远只收到它的 SHA-256。
func eventArtifactRefs(event domain.Event) ([]artifact.Ref, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode event artifact fields: %w", err)
	}
	keys := []string{"task_ref", "config_ref", "final_response_ref", "reason_ref", "request_ref", "response_ref", "arguments_ref", "plan_ref", "evidence_ref", "result_ref", "model_response_ref", "message_ref"}
	var references []artifact.Ref
	for _, key := range keys {
		raw, exists := payload[key]
		if !exists || string(raw) == "null" {
			continue
		}
		var reference artifact.Ref
		if err := json.Unmarshal(raw, &reference); err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		if reference.SHA256 == "" {
			return nil, fmt.Errorf("%s has no artifact digest", key)
		}
		references = append(references, reference)
	}
	return references, nil
}

// checkMemoryReferences 执行 doctor 的跨库验证。Memory 记录必须仍能在当前
// workspace 的 Journal 与 Artifact Store 中找到完全匹配的来源，否则失败关闭。
func checkMemoryReferences(ctx context.Context, journal interface {
	Load(context.Context, domain.RunID) ([]domain.Event, error)
}, stateDir, workspaceHash string) (string, bool, error) {
	store, err := openReadOnlyMemory(stateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "absent", false, nil
		}
		return "invalid", false, err
	}
	defer store.Close()
	if err := verifyMemoryStore(ctx, store, journal, stateDir, workspaceHash); err != nil {
		return "invalid", false, err
	}
	return "ok", true, nil
}

// verifyReadOnlyMemoryStore 为 recall/inspect 提供和 doctor 同等严格的只读信任
// 校验：Memory Store 一旦存在，缺失 Journal 或任何外部证据均为状态损坏而非空集。
func verifyReadOnlyMemoryStore(ctx context.Context, store *memorysqlite.Store, stateDir, workspaceHash string) error {
	journal, err := openReadOnlyJournal(stateDir, workspaceHash)
	if err != nil {
		return fmt.Errorf("open memory source journal: %w", err)
	}
	defer journal.Close()
	return verifyMemoryStore(ctx, store, journal, stateDir, workspaceHash)
}

// verifyMemoryStore 复用给 doctor、recall 与 inspect 的内部/外部完整性验证；它
// 检查全部候选而非仅当前 recall 命中，防止把损坏记录静默过滤成成功结果。
func verifyMemoryStore(ctx context.Context, store *memorysqlite.Store, journal interface {
	Load(context.Context, domain.RunID) ([]domain.Event, error)
}, stateDir, workspaceHash string) error {
	if err := store.Check(ctx); err != nil {
		return err
	}
	candidates, err := store.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if candidate.WorkspaceHash != workspaceHash {
			return errors.New("memory candidate workspace hash mismatch")
		}
		events, err := journal.Load(ctx, domain.RunID(candidate.RunID))
		if err != nil || len(events) == 0 {
			return fmt.Errorf("memory candidate source run %s is missing", candidate.RunID)
		}
		projection, err := domain.Replay(events)
		if err != nil || !projection.Terminal() || projection.WorkspaceHash != workspaceHash {
			return fmt.Errorf("memory candidate source run %s is invalid", candidate.RunID)
		}
		if candidate.TaskPromptDigest != contract.FreeformTaskSpec(projection.Task).PromptDigest || candidate.Outcome != projection.Phase() || candidate.FailureClass != contract.ClassifyTerminal(events) || candidate.TrustedVerification != contract.CompletionTrusted(projection.State, projection.VerificationSucceeded, projection.LastVerificationSeq, projection.LastMutationSeq) {
			return fmt.Errorf("memory candidate %s does not match source run facts", candidate.ID)
		}
		if err := verifyCandidateEvidence(candidate, events, stateDir, workspaceHash); err != nil {
			return err
		}
	}
	return nil
}

// verifyCandidateEvidence 验证每个 memory EvidenceRef 的 Run、seq、hash、type
// 与可选制品摘要都仍由同一个来源事件声明并可在 CAS 中校验。
func verifyCandidateEvidence(candidate memory.Candidate, events []domain.Event, stateDir, workspaceHash string) error {
	terminalSeen := false
	root := manifestArtifactRoot(stateDir, workspaceHash)
	for _, evidence := range candidate.Evidence {
		var matched *domain.Event
		for index := range events {
			event := &events[index]
			if evidence.RunID == string(event.RunID) && evidence.EventSeq == event.Seq && evidence.EventHash == "sha256:"+event.EventHash && evidence.EventType == string(event.Type) && event.WorkspaceHash == workspaceHash {
				matched = event
				break
			}
		}
		if matched == nil {
			return fmt.Errorf("memory candidate %s has missing or mismatched event evidence", candidate.ID)
		}
		if matched.Seq == events[len(events)-1].Seq && evidence.ArtifactSHA256 == "" {
			terminalSeen = true
		}
		if evidence.ArtifactSHA256 == "" {
			continue
		}
		artifacts, err := eventArtifactRefs(*matched)
		if err != nil {
			return err
		}
		found := false
		for _, artifactRef := range artifacts {
			if artifactRef.SHA256 == evidence.ArtifactSHA256 {
				if err := checkArtifactReference(root, artifactRef); err != nil {
					return err
				}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("memory candidate %s artifact evidence is not declared by its event", candidate.ID)
		}
	}
	if !terminalSeen {
		return fmt.Errorf("memory candidate %s lacks terminal event evidence", candidate.ID)
	}
	return nil
}

// memoryDBPath 返回工作区专属的独立 Memory SQLite 路径；它与 Journal 仅共享
// 工作区目录和锁，不共享表、事务或事实所有权。
func memoryDBPath(stateDir, workspaceHash string) string {
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	return filepath.Join(stateDir, "workspaces", workspaceHash, "memory.db")
}

// openMemory 打开可写的独立 Memory Store，仅供 capture/revoke 这类明确写入用例使用。
func openMemory(stateDir, workspaceHash string) (*memorysqlite.Store, error) {
	return memorysqlite.Open(memoryDBPath(stateDir, workspaceHash))
}

// openReadOnlyMemory 打开既有 Memory Store；缺失路径返回原始 fs.ErrNotExist，
// 由 recall/doctor 根据各自的只读语义决定是空集还是完整性失败。
func openReadOnlyMemory(stateDir, workspaceHash string) (*memorysqlite.Store, error) {
	return memorysqlite.OpenReadOnly(memoryDBPath(stateDir, workspaceHash))
}
