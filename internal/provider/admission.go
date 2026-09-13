// 本文件实现 ProviderAdmissionDecision 合同、分区链解析与准入门评估：
// 决定按 request_profile_fingerprint 分区成链，supersedes 只能引用同分区
// 决定，断链、环或多链头一律失败关闭；时间戳仅审计，链结构是唯一顺序
// 事实源。门是门不是源：admission 决定不覆盖配置，只决定能否启动。
//
// Author: fxbin
package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/contract"
)

// admissionDecisionSchemaV1 是 ProviderAdmissionDecision 的当前合同版本。
const admissionDecisionSchemaV1 = 1

// admissionFingerprintPrefix 是准入决定内容指纹的版本化域前缀。
const admissionFingerprintPrefix = "cairn-provider-admission-decision-v1"

// maxAdmissionReasonBytes 限制准入理由的 UTF-8 字节数，避免权威记录被无界自由文本放大。
const maxAdmissionReasonBytes = 1024

// 决定与权威词表（闭合集合）。
const (
	DecisionAdmitted = "admitted"
	DecisionRejected = "rejected"

	AuthorityMaintainer         = "maintainer"
	AuthorityInteractiveConfirm = "interactive_confirm"
)

// AdmissionBinding 绑定一条准入决定的有效性判据：有效性只看
// request_profile_fingerprint；config_fingerprint 仅作审计信息，不参与门
// 判定（D11）。
type AdmissionBinding struct {
	RequestProfileFingerprint string `json:"request_profile_fingerprint"`
	ConfigFingerprint         string `json:"config_fingerprint"`
}

// ProviderAdmissionDecision 是维护者对一份在档证据的准入结论：引用证据指
// 纹、绑定请求合同指纹、携带权威与脱敏理由，并可经 supersedes 取代同分区
// 的既有决定。追加式吊销：rejected 或被取代，旧记录永不删改。
type ProviderAdmissionDecision struct {
	SchemaVersion                 int              `json:"schema_version"`
	EvidenceFingerprint           string           `json:"evidence_fingerprint"`
	Binding                       AdmissionBinding `json:"binding"`
	Decision                      string           `json:"decision"`
	DecidedAt                     time.Time        `json:"decided_at"`
	Authority                     string           `json:"authority"`
	Reason                        string           `json:"reason"`
	SupersedesDecisionFingerprint string           `json:"supersedes_decision_fingerprint,omitempty"`
	DecisionFingerprint           string           `json:"decision_fingerprint"`
}

// decisionFingerprintView 是参与内容指纹的投影：排除指纹字段自身。
type decisionFingerprintView struct {
	SchemaVersion                 int              `json:"schema_version"`
	EvidenceFingerprint           string           `json:"evidence_fingerprint"`
	Binding                       AdmissionBinding `json:"binding"`
	Decision                      string           `json:"decision"`
	DecidedAt                     time.Time        `json:"decided_at"`
	Authority                     string           `json:"authority"`
	Reason                        string           `json:"reason"`
	SupersedesDecisionFingerprint string           `json:"supersedes_decision_fingerprint,omitempty"`
}

// ComputeFingerprint 计算决定的内容指纹（decision_fingerprint）：由调用方在
// 落盘前调用并回填；文件名即该值。
func (decision ProviderAdmissionDecision) ComputeFingerprint() (string, error) {
	if err := decision.Validate(); err != nil {
		return "", err
	}
	view := decisionFingerprintView{
		SchemaVersion: decision.SchemaVersion, EvidenceFingerprint: decision.EvidenceFingerprint,
		Binding: decision.Binding, Decision: decision.Decision, DecidedAt: decision.DecidedAt,
		Authority: decision.Authority, Reason: decision.Reason,
		SupersedesDecisionFingerprint: decision.SupersedesDecisionFingerprint,
	}
	return canonicaljson.Fingerprint(admissionFingerprintPrefix, view)
}

// Validate 复核决定的结构自洽：schema 版本、全部指纹引用、理由边界、词表闭合与时间戳。
func (decision ProviderAdmissionDecision) Validate() error {
	if decision.SchemaVersion != admissionDecisionSchemaV1 {
		return fmt.Errorf("admission decision schema_version %d is not supported", decision.SchemaVersion)
	}
	if !sha256DigestShape(decision.EvidenceFingerprint) {
		return errors.New("admission decision requires an evidence fingerprint")
	}
	if !sha256DigestShape(decision.Binding.RequestProfileFingerprint) {
		return errors.New("admission binding requires a request profile fingerprint")
	}
	if !sha256DigestShape(decision.Binding.ConfigFingerprint) {
		return errors.New("admission binding requires a config fingerprint")
	}
	if decision.SupersedesDecisionFingerprint != "" && !sha256DigestShape(decision.SupersedesDecisionFingerprint) {
		return errors.New("admission decision carries an invalid supersedes fingerprint")
	}
	if decision.DecisionFingerprint != "" && !sha256DigestShape(decision.DecisionFingerprint) {
		return errors.New("admission decision carries an invalid decision fingerprint")
	}
	if !utf8.ValidString(decision.Reason) {
		return errors.New("admission decision reason must be valid UTF-8")
	}
	trimmedReason := strings.TrimSpace(decision.Reason)
	if trimmedReason == "" {
		return errors.New("admission decision requires a reason")
	}
	if trimmedReason != decision.Reason {
		return errors.New("admission decision reason must not carry surrounding whitespace")
	}
	if len(decision.Reason) > maxAdmissionReasonBytes {
		return fmt.Errorf("admission decision reason exceeds %d bytes", maxAdmissionReasonBytes)
	}
	switch decision.Decision {
	case DecisionAdmitted, DecisionRejected:
	default:
		return fmt.Errorf("admission decision %q is not in the closed vocabulary", decision.Decision)
	}
	switch decision.Authority {
	case AuthorityMaintainer, AuthorityInteractiveConfirm:
	default:
		return fmt.Errorf("admission authority %q is not in the closed vocabulary", decision.Authority)
	}
	if decision.DecidedAt.IsZero() {
		return errors.New("admission decision requires a timestamp")
	}
	return nil
}

// sha256DigestShape 校验 "sha256:<64 位小写十六进制>" 的摘要形态；准入决定
// 只接受指纹形态的引用，杜绝以自由文本冒充链引用。
func sha256DigestShape(value string) bool {
	if len(value) != len("sha256:")+64 || value[:7] != "sha256:" {
		return false
	}
	for index := 7; index < len(value); index++ {
		c := value[index]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// DecisionStore 是准入决定的内容寻址存储：布局、原子写与幂等语义同证据
// 存储；目录为 provider/admission/。
type DecisionStore struct {
	root string
}

// decisionDirName 是决定文件在工作区 provider 目录下的子目录名。
const decisionDirName = "admission"

// OpenDecisionStore 打开（必要时创建）指定工作区的决定存储目录。
func OpenDecisionStore(stateDir, workspaceHash string) (*DecisionStore, error) {
	if stateDir == "" || workspaceHash == "" {
		return nil, errors.New("decision store requires a state dir and workspace hash")
	}
	root := filepath.Join(stateDir, "workspaces", workspaceHash, "provider", decisionDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &DecisionStore{root: root}, nil
}

// Append 校验并落盘一条决定；语义同证据存储的 append-only 幂等写入。
func (store *DecisionStore) Append(decision *ProviderAdmissionDecision) (string, error) {
	fingerprint, err := decision.ComputeFingerprint()
	if err != nil {
		return "", err
	}
	decision.DecisionFingerprint = fingerprint
	encoded, err := json.Marshal(decision)
	if err != nil {
		return "", err
	}
	path := filepath.Join(store.root, fingerprint+".json")
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != string(append(encoded, '\n')) {
			return "", fmt.Errorf("decision %s already exists with different content", fingerprint)
		}
		return fingerprint, nil
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return "", err
	}
	return fingerprint, os.Rename(temporary, path)
}

// ListPartition 返回绑定到指定 request_profile_fingerprint 的全部分区决定。
// 分区外决定（包括错误写入的其他绑定）不进入本分区，也就不参与链校验。
func (store *DecisionStore) ListPartition(requestProfileFingerprint string) ([]ProviderAdmissionDecision, error) {
	entries, err := os.ReadDir(store.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var partition []ProviderAdmissionDecision
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		encoded, err := os.ReadFile(filepath.Join(store.root, entry.Name()))
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(encoded) {
			return nil, fmt.Errorf("decode admission decision %s: invalid UTF-8", entry.Name())
		}
		var decision ProviderAdmissionDecision
		if err := json.Unmarshal(encoded, &decision); err != nil {
			return nil, fmt.Errorf("decode admission decision %s: %w", entry.Name(), err)
		}
		recomputed, err := decision.ComputeFingerprint()
		if err != nil {
			return nil, fmt.Errorf("validate admission decision %s: %w", entry.Name(), err)
		}
		fileFingerprint := entry.Name()[:len(entry.Name())-len(".json")]
		if decision.DecisionFingerprint != recomputed || recomputed != fileFingerprint {
			return nil, fmt.Errorf("decision file %s carries inconsistent fingerprints", entry.Name())
		}
		if decision.Binding.RequestProfileFingerprint == requestProfileFingerprint {
			partition = append(partition, decision)
		}
	}
	return partition, nil
}

// ResolveChainHead 在分区内解析唯一链头：链头 = 未被任何在档决定 supersedes
// 引用的决定。断链（supersedes 引用不存在的决定）、环（supersedes 成环）
// 或多链头（两个及以上未被引用的决定）时该分区失败关闭；空分区返回 nil。
func ResolveChainHead(partition []ProviderAdmissionDecision) (*ProviderAdmissionDecision, error) {
	byFingerprint := make(map[string]ProviderAdmissionDecision, len(partition))
	referenced := make(map[string]bool, len(partition))
	for _, decision := range partition {
		if _, duplicated := byFingerprint[decision.DecisionFingerprint]; duplicated {
			return nil, fmt.Errorf("admission partition carries duplicate decision %s", decision.DecisionFingerprint)
		}
		byFingerprint[decision.DecisionFingerprint] = decision
	}
	for _, decision := range partition {
		if decision.SupersedesDecisionFingerprint == "" {
			continue
		}
		if _, ok := byFingerprint[decision.SupersedesDecisionFingerprint]; !ok {
			return nil, fmt.Errorf("admission chain is broken: decision %s supersedes unknown decision", decision.DecisionFingerprint)
		}
		referenced[decision.SupersedesDecisionFingerprint] = true
	}
	for _, decision := range partition {
		visited := map[string]bool{decision.DecisionFingerprint: true}
		cursor := decision
		for cursor.SupersedesDecisionFingerprint != "" {
			next, ok := byFingerprint[cursor.SupersedesDecisionFingerprint]
			if !ok {
				break
			}
			if visited[next.DecisionFingerprint] {
				return nil, fmt.Errorf("admission chain contains a cycle at %s", next.DecisionFingerprint)
			}
			visited[next.DecisionFingerprint] = true
			cursor = next
		}
	}
	var heads []ProviderAdmissionDecision
	for _, decision := range partition {
		if !referenced[decision.DecisionFingerprint] {
			heads = append(heads, decision)
		}
	}
	if len(heads) > 1 {
		return nil, errors.New("admission partition carries multiple chain heads")
	}
	if len(heads) == 0 {
		return nil, errors.New("admission partition has no chain head")
	}
	return &heads[0], nil
}

// AdmissionOutcome 是一次门评估的闭合结论：allowed 表是否放行；head 为分
// 区链头（optional 下可为 nil 表示无任何决定）；原因供展示，不携带敏感值。
type AdmissionOutcome struct {
	Allowed bool
	Head    *ProviderAdmissionDecision
}

// EvaluateAdmissionGate 评估准入门：admission 必须是 "optional" 或
// "required"。optional 门不拦截（缺失决定即未准入，如实展示）；required 时
// 重算请求合同指纹、解析该绑定分区链头，链头 admitted 且其证据在档、证据
// 选择与解析身份一致才放行——任一不成立（rejected、无链头、断链、环、多
// 链头、证据缺失或身份不一致）都失败关闭。version 1 配置不进入本函数
// （无准入语义）。
func EvaluateAdmissionGate(decisions *DecisionStore, evidence *EvidenceStore, profile contract.ResolvedProviderProfile, admission string) (AdmissionOutcome, error) {
	switch admission {
	case "optional":
		if decisions == nil {
			return AdmissionOutcome{Allowed: true}, nil
		}
		requestFingerprint, err := profile.Fingerprint()
		if err != nil {
			return AdmissionOutcome{}, err
		}
		partition, err := decisions.ListPartition(requestFingerprint)
		if err != nil {
			return AdmissionOutcome{}, err
		}
		var head *ProviderAdmissionDecision
		if len(partition) != 0 {
			head, err = ResolveChainHead(partition)
			if err != nil {
				return AdmissionOutcome{Allowed: true, Head: head}, fmt.Errorf("admission partition is corrupt (gate not enforced): %w", err)
			}
		}
		return AdmissionOutcome{Allowed: true, Head: head}, nil
	case "required":
		requestFingerprint, err := profile.Fingerprint()
		if err != nil {
			return AdmissionOutcome{}, err
		}
		partition, err := decisions.ListPartition(requestFingerprint)
		if err != nil {
			return AdmissionOutcome{}, err
		}
		if len(partition) == 0 {
			return AdmissionOutcome{}, errors.New("provider is not admitted: no admission decision on file for this binding")
		}
		head, err := ResolveChainHead(partition)
		if err != nil {
			return AdmissionOutcome{}, fmt.Errorf("admission gate fails closed: %w", err)
		}
		if head.Decision != DecisionAdmitted {
			return AdmissionOutcome{Head: head}, errors.New("provider admission is rejected for this binding")
		}
		record, err := evidence.Get(head.EvidenceFingerprint)
		if err != nil {
			return AdmissionOutcome{Head: head}, fmt.Errorf("admission gate fails closed: %w", err)
		}
		if record.Selection != profile.Selection {
			return AdmissionOutcome{Head: head}, errors.New("admission evidence selection does not match the resolved profile")
		}
		return AdmissionOutcome{Allowed: true, Head: head}, nil
	default:
		return AdmissionOutcome{}, fmt.Errorf("admission %q is not a gate mode", admission)
	}
}
