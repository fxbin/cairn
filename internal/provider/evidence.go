// 本文件实现 ProviderProbeEvidence 合同与其内容寻址存储：探测观测的三态
// 词表、工具回路六态词表、记录指纹与原子写。存储位于
// <stateDir>/workspaces/<workspaceHash>/provider/evidence/，只增不改，目录
// 0700、文件 0600；文件名即记录指纹，读回时校验内容指纹与文件名一致。
//
// Author: fxbin
package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/contract"
)

// probeEvidenceSchemaV1 是 ProviderProbeEvidence 的当前合同版本。
const probeEvidenceSchemaV1 = 1

// probeContractVersion 标识探测行为的合同代数：行为语义（两程工具回路、
// usage/stop reason 核对口径）发生变更即递增，使不同代数的观测不可直接
// 比较。当前代数覆盖两程工具回路判定。
const probeContractVersion = 1

// CurrentProbeContractVersion 导出当前探测行为代数，供探测命令构造证据
// 时引用，杜绝 CLI 侧硬编码漂移。
const CurrentProbeContractVersion = probeContractVersion

// evidenceFingerprintPrefix 是探测证据内容指纹的版本化域前缀。
const evidenceFingerprintPrefix = "cairn-provider-probe-evidence-v1"

// 观测三态与结果词表（D12）：not_observed 表示本次探测未检验该维度，绝不
// 与"实测为假"混淆。
const (
	ObservationNotObserved   = "not_observed"
	ObservationObservedTrue  = "observed_true"
	ObservationObservedFalse = "observed_false"
)

// 状态观测词表：值本身缺席（未观测到停止原因/模型修订）时用 not_observed
// 表达，观测到才进入 observed 并携带值。
const (
	StateNotObserved = "not_observed"
	StateObserved    = "observed"
)

// 工具回路六态闭合词表（D12）：not_observed=未探测工具维度；skipped=未启用
// 探测；ok=两程全通；not_invoked=模型未调用工具；failed=第一程失败；
// second_leg_failed=第一程成功但第二程失败。
const (
	RoundtripNotObserved     = "not_observed"
	RoundtripSkipped         = "skipped"
	RoundtripOK              = "ok"
	RoundtripNotInvoked      = "not_invoked"
	RoundtripFailed          = "failed"
	RoundtripSecondLegFailed = "second_leg_failed"
)

// 探测结局词表：completed 表示探测流程闭合完成（含 inconclusive 结论）；
// failed 表示探测过程本身失败（网络、合同拒绝等）。
const (
	EvidenceOutcomeCompleted = "completed"
	EvidenceOutcomeFailed    = "failed"
)

// EvidenceObservations 承载一次探测的维度观测：任何维度的缺席都必须显式
// 用 not_observed 表达，不允许以零值伪装成已观测。
type EvidenceObservations struct {
	UsageStreamed      string   `json:"usage_streamed"`
	StopReasonsState   string   `json:"stop_reasons_state"`
	StopReasons        []string `json:"stop_reasons,omitempty"`
	ToolRoundtrip      string   `json:"tool_roundtrip"`
	ModelRevisionState string   `json:"model_revision_state"`
	ModelRevision      string   `json:"model_revision"`
}

// ProviderProbeEvidence 是版本化、内容寻址的探测观测合同：身份选择、探测
// 行为代数、维度观测与脱敏结论。禁止进入：请求/响应正文、密钥、端点明文、
// reasoning 内容与任务正文。
type ProviderProbeEvidence struct {
	SchemaVersion        int                        `json:"schema_version"`
	Selection            contract.ProviderSelection `json:"selection"`
	ProbeContractVersion int                        `json:"probe_contract_version"`
	Observations         EvidenceObservations       `json:"observations"`
	Outcome              string                     `json:"outcome"`
	ErrorClass           string                     `json:"error_class,omitempty"`
	StatusCode           int                        `json:"status_code,omitempty"`
	ProbedAt             time.Time                  `json:"probed_at"`
	EvidenceFingerprint  string                     `json:"evidence_fingerprint"`
}

// evidenceFingerprintView 是参与内容指纹的投影：排除指纹字段自身，其余字段
// 全量进入，保证同内容恒同指纹、异内容必异指纹。
type evidenceFingerprintView struct {
	SchemaVersion        int                        `json:"schema_version"`
	Selection            contract.ProviderSelection `json:"selection"`
	ProbeContractVersion int                        `json:"probe_contract_version"`
	Observations         EvidenceObservations       `json:"observations"`
	Outcome              string                     `json:"outcome"`
	ErrorClass           string                     `json:"error_class,omitempty"`
	StatusCode           int                        `json:"status_code,omitempty"`
	ProbedAt             time.Time                  `json:"probed_at"`
}

// ComputeFingerprint 计算证据的内容指纹（evidence_fingerprint）：由调用方在
// 落盘前调用并回填；文件名即该值，读回校验两者一致即完整性校验。
func (evidence ProviderProbeEvidence) ComputeFingerprint() (string, error) {
	if err := evidence.Validate(); err != nil {
		return "", err
	}
	view := evidenceFingerprintView{
		SchemaVersion: evidence.SchemaVersion, Selection: evidence.Selection,
		ProbeContractVersion: evidence.ProbeContractVersion, Observations: evidence.Observations,
		Outcome: evidence.Outcome, ErrorClass: evidence.ErrorClass, StatusCode: evidence.StatusCode,
		ProbedAt: evidence.ProbedAt,
	}
	return canonicaljson.Fingerprint(evidenceFingerprintPrefix, view)
}

// Validate 复核证据的结构自洽：schema 版本、选择身份、词表闭合与观测一致
// 性；违反任何一条即失败关闭，防御被手工篡改或跨代混用的记录。
func (evidence ProviderProbeEvidence) Validate() error {
	if evidence.SchemaVersion != probeEvidenceSchemaV1 {
		return fmt.Errorf("probe evidence schema_version %d is not supported", evidence.SchemaVersion)
	}
	if evidence.ProbeContractVersion != probeContractVersion {
		return fmt.Errorf("probe contract version %d is not supported", evidence.ProbeContractVersion)
	}
	if evidence.Selection.SchemaVersion != 1 || evidence.Selection.ProviderID == "" || evidence.Selection.ModelRef == "" {
		return errors.New("probe evidence requires a resolved selection")
	}
	switch evidence.Observations.UsageStreamed {
	case ObservationNotObserved, ObservationObservedTrue, ObservationObservedFalse:
	default:
		return fmt.Errorf("usage_streamed observation %q is not in the closed vocabulary", evidence.Observations.UsageStreamed)
	}
	switch evidence.Observations.StopReasonsState {
	case StateNotObserved:
		if len(evidence.Observations.StopReasons) != 0 {
			return errors.New("stop reasons must be absent when not observed")
		}
	case StateObserved:
		if len(evidence.Observations.StopReasons) == 0 {
			return errors.New("stop reasons must be present when observed")
		}
	default:
		return fmt.Errorf("stop_reasons_state %q is not in the closed vocabulary", evidence.Observations.StopReasonsState)
	}
	switch evidence.Observations.ToolRoundtrip {
	case RoundtripNotObserved, RoundtripSkipped, RoundtripOK, RoundtripNotInvoked, RoundtripFailed, RoundtripSecondLegFailed:
	default:
		return fmt.Errorf("tool_roundtrip %q is not in the closed vocabulary", evidence.Observations.ToolRoundtrip)
	}
	switch evidence.Observations.ModelRevisionState {
	case StateNotObserved:
		if evidence.Observations.ModelRevision != "" {
			return errors.New("model revision must be absent when not observed")
		}
	case StateObserved:
		if evidence.Observations.ModelRevision == "" {
			return errors.New("model revision must be present when observed")
		}
	default:
		return fmt.Errorf("model_revision_state %q is not in the closed vocabulary", evidence.Observations.ModelRevisionState)
	}
	switch evidence.Outcome {
	case EvidenceOutcomeCompleted, EvidenceOutcomeFailed:
	default:
		return fmt.Errorf("probe outcome %q is not in the closed vocabulary", evidence.Outcome)
	}
	if evidence.Outcome == EvidenceOutcomeCompleted && evidence.ErrorClass != "" {
		return errors.New("completed probes must not carry an error class")
	}
	if evidence.ProbedAt.IsZero() {
		return errors.New("probe evidence requires a probe timestamp")
	}
	return nil
}

// EvidenceStore 是探测证据的内容寻址存储：append-only、原子写，文件名即
// 记录指纹。并发发布使用 no-replace 语义，读取始终校验内容指纹与文件名一致。
type EvidenceStore struct {
	root string
}

// evidenceDirName 是证据文件在工作区 provider 目录下的子目录名。
const evidenceDirName = "evidence"

// OpenEvidenceStore 打开（必要时创建）指定工作区的证据存储目录。
func OpenEvidenceStore(stateDir, workspaceHash string) (*EvidenceStore, error) {
	if stateDir == "" || workspaceHash == "" {
		return nil, errors.New("evidence store requires a state dir and workspace hash")
	}
	root := filepath.Join(stateDir, "workspaces", workspaceHash, "provider", evidenceDirName)
	if err := openContentAddressedStore(root); err != nil {
		return nil, fmt.Errorf("open evidence store: %w", err)
	}
	return &EvidenceStore{root: root}, nil
}

// Append 校验并落盘一条证据：内容指纹回填后以同目录唯一临时文件发布
// （0600）；同指纹文件已存在时校验内容一致即幂等返回，永不覆盖或删除。
func (store *EvidenceStore) Append(evidence *ProviderProbeEvidence) (string, error) {
	fingerprint, err := evidence.ComputeFingerprint()
	if err != nil {
		return "", err
	}
	evidence.EvidenceFingerprint = fingerprint
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return "", fmt.Errorf("encode evidence %s: %w", fingerprint, err)
	}
	content := append(encoded, '\n')
	if err := appendContentAddressed(store.root, fingerprint+".json", content, "evidence", fingerprint); err != nil {
		return "", err
	}
	return fingerprint, nil
}

// Get 按指纹读回一条证据并校验内容指纹与请求指纹一致；内容寻址即完整性
// 校验，不一致按不存在处理并失败关闭。
func (store *EvidenceStore) Get(fingerprint string) (ProviderProbeEvidence, error) {
	encoded, err := os.ReadFile(filepath.Join(store.root, fingerprint+".json"))
	if err != nil {
		return ProviderProbeEvidence{}, fmt.Errorf("evidence %s is not on file: %w", fingerprint, err)
	}
	var evidence ProviderProbeEvidence
	if err := json.Unmarshal(encoded, &evidence); err != nil {
		return ProviderProbeEvidence{}, fmt.Errorf("decode evidence %s: %w", fingerprint, err)
	}
	recomputed, err := evidence.ComputeFingerprint()
	if err != nil {
		return ProviderProbeEvidence{}, fmt.Errorf("validate evidence %s: %w", fingerprint, err)
	}
	if evidence.EvidenceFingerprint != recomputed || recomputed != fingerprint {
		return ProviderProbeEvidence{}, fmt.Errorf("evidence file %s carries inconsistent fingerprints", fingerprint)
	}
	return evidence, nil
}
