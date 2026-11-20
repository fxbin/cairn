// verification_scope.go 定义领域无关的验证范围：冻结对象身份、内容摘要、
// 所属验收标准与适用规则。范围在验证开始前冻结，执行方不得在运行中自行缩小。
//
// Author: fxbin
package contract

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/canonicaljson"
)

const (
	// VerificationScopeVersion 是验证范围合同的当前格式版本。
	VerificationScopeVersion = 1
	// MaxVerificationObjects 限制单次验证登记的对象数量。
	MaxVerificationObjects = 256
	// verificationScopeDigestPrefix 是范围摘要的版本化域前缀。
	verificationScopeDigestPrefix = "cairn-verification-scope-v1"
	// MaxVerificationVersionBytes 限制对象版本/摘要的字节数。
	MaxVerificationVersionBytes = 512
)

var verificationToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// VerificationObject 是一个被验证对象。ID/Kind/Ruleset 是不透明标识，不含
// 语言、Git 或测试框架语义；Version 是内容摘要或可核对的版本，必须冻结。
type VerificationObject struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Version  string   `json:"version"`
	Criteria []string `json:"criteria"`
	// Required 在范围冻结时由所属标准推导：对象服务任一必需标准即为必需。
	// 它参与摘要，后续不能在不破坏摘要的情况下被改写。
	Required       bool   `json:"required"`
	Ruleset        string `json:"ruleset"`
	RulesetVersion string `json:"ruleset_version"`
}

// VerificationScope 是冻结的验证范围。Digest 覆盖版本、合同摘要与全部对象，
// 读取方必须重算并比对，不能信任外部携带的摘要。
type VerificationScope struct {
	Version          int                  `json:"version"`
	AcceptanceDigest string               `json:"acceptance_digest"`
	Objects          []VerificationObject `json:"objects"`
	Digest           string               `json:"digest"`
}

// verificationScopeContent 是摘要输入；Digest 字段自身不参与计算。
type verificationScopeContent struct {
	Version          int                  `json:"version"`
	AcceptanceDigest string               `json:"acceptance_digest"`
	Objects          []VerificationObject `json:"objects"`
}

// NewVerificationScope 校验对象并计算稳定摘要。输入被深拷贝，调用方后续修改
// 不会改变已冻结范围。对象必须归属合同中的标准，否则拒绝登记。
func NewVerificationScope(acceptance AcceptanceContract, objects []VerificationObject) (VerificationScope, error) {
	if err := acceptance.Validate(); err != nil {
		return VerificationScope{}, fmt.Errorf("acceptance contract: %w", err)
	}
	cloned := CloneVerificationObjects(objects)
	requiredCriteria := make(map[string]bool)
	for _, criterion := range acceptance.Criteria {
		if criterion.Required {
			requiredCriteria[criterion.ID] = true
		}
	}
	for index := range cloned {
		for _, criterion := range cloned[index].Criteria {
			if requiredCriteria[criterion] {
				cloned[index].Required = true
				break
			}
		}
	}
	if err := validateVerificationObjects(cloned, acceptance); err != nil {
		return VerificationScope{}, err
	}
	digest, err := verificationScopeDigest(acceptance.Digest, cloned)
	if err != nil {
		return VerificationScope{}, err
	}
	scope := VerificationScope{Version: VerificationScopeVersion, AcceptanceDigest: acceptance.Digest, Objects: cloned, Digest: digest}
	// 必需标准必须有对象服务，否则范围无法覆盖合同，必须失败关闭。
	if err := ValidateAcceptanceCoverage(acceptance, scope); err != nil {
		return VerificationScope{}, err
	}
	return scope, nil
}

// Validate 复核版本、对象结构与摘要；任何不一致都失败关闭。
func (scope VerificationScope) Validate() error {
	if scope.Version != VerificationScopeVersion {
		return fmt.Errorf("verification scope version %d is unsupported", scope.Version)
	}
	if scope.AcceptanceDigest == "" {
		return errors.New("verification scope has no acceptance contract digest")
	}
	if err := validateVerificationObjects(scope.Objects, AcceptanceContract{}); err != nil {
		return err
	}
	digest, err := verificationScopeDigest(scope.AcceptanceDigest, scope.Objects)
	if err != nil {
		return err
	}
	if scope.Digest != digest {
		return errors.New("verification scope digest does not match its objects")
	}
	return nil
}

// ValidateAcceptanceCoverage 核对每条必需标准都至少被一个冻结对象服务。范围
// 自身只携带摘要，完整合同须显式传入；未覆盖的必需标准是显式缺口，不得静默
// 以空分母或空 UncoveredRequired 表现为“无问题”。
func ValidateAcceptanceCoverage(acceptance AcceptanceContract, scope VerificationScope) error {
	if err := acceptance.Validate(); err != nil {
		return fmt.Errorf("acceptance contract: %w", err)
	}
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("verification scope: %w", err)
	}
	if scope.AcceptanceDigest != acceptance.Digest {
		return errors.New("verification scope acceptance digest does not match the contract")
	}
	served := make(map[string]bool)
	for _, object := range scope.Objects {
		for _, criterion := range object.Criteria {
			served[criterion] = true
		}
	}
	for _, criterion := range acceptance.Criteria {
		if criterion.Required && !served[criterion.ID] {
			return fmt.Errorf("required criterion %q has no verification object", criterion.ID)
		}
	}
	return nil
}

// Object 按身份查找冻结对象。
func (scope VerificationScope) Object(id string) (VerificationObject, bool) {
	for _, object := range scope.Objects {
		if object.ID == id {
			return object, true
		}
	}
	return VerificationObject{}, false
}

// CriterionIDs 返回范围引用的去重标准身份，按字典序稳定排列。
func (scope VerificationScope) CriterionIDs() []string {
	seen := make(map[string]struct{})
	for _, object := range scope.Objects {
		for _, criterion := range object.Criteria {
			seen[criterion] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Clone 返回范围及对象的深拷贝。
func (scope VerificationScope) Clone() VerificationScope {
	cloned := scope
	cloned.Objects = CloneVerificationObjects(scope.Objects)
	return cloned
}

// CloneVerificationObjects 深拷贝对象及其标准列表。
func CloneVerificationObjects(objects []VerificationObject) []VerificationObject {
	if objects == nil {
		return nil
	}
	cloned := make([]VerificationObject, len(objects))
	for index, object := range objects {
		cloned[index] = object
		cloned[index].Criteria = append([]string(nil), object.Criteria...)
	}
	return cloned
}

// validateVerificationObjects 校验对象集合。acceptance 非零值时额外核对每个
// 对象引用的标准确实存在于合同；零值只做结构校验（供 Validate 使用）。
func validateVerificationObjects(objects []VerificationObject, acceptance AcceptanceContract) error {
	if len(objects) == 0 || len(objects) > MaxVerificationObjects {
		return errors.New("invalid verification object count")
	}
	identities := make(map[string]struct{}, len(objects))
	knownCriteria := make(map[string]struct{})
	for _, criterion := range acceptance.Criteria {
		knownCriteria[criterion.ID] = struct{}{}
	}
	for index, object := range objects {
		if !verificationToken.MatchString(object.ID) {
			return fmt.Errorf("verification object %d has an invalid id", index+1)
		}
		if _, duplicate := identities[object.ID]; duplicate {
			return fmt.Errorf("verification object id %q is duplicated", object.ID)
		}
		identities[object.ID] = struct{}{}
		if !verificationToken.MatchString(object.Kind) || !verificationToken.MatchString(object.Ruleset) ||
			!verificationToken.MatchString(object.RulesetVersion) {
			return fmt.Errorf("verification object %q has an invalid identity or ruleset", object.ID)
		}
		if !validVerificationVersion(object.Version) {
			return fmt.Errorf("verification object %q has an invalid version", object.ID)
		}
		if len(object.Criteria) == 0 {
			return fmt.Errorf("verification object %q has no acceptance criterion", object.ID)
		}
		seen := make(map[string]struct{}, len(object.Criteria))
		for _, criterion := range object.Criteria {
			if !ValidAcceptanceIdentifier(criterion) {
				return fmt.Errorf("verification object %q has an invalid criterion id", object.ID)
			}
			if _, duplicate := seen[criterion]; duplicate {
				return fmt.Errorf("verification object %q repeats criterion %q", object.ID, criterion)
			}
			seen[criterion] = struct{}{}
			if len(knownCriteria) != 0 {
				if _, found := knownCriteria[criterion]; !found {
					return fmt.Errorf("verification object %q references unknown criterion %q", object.ID, criterion)
				}
			}
		}
	}
	return nil
}

// validVerificationVersion 要求版本/摘要非空、有界、可打印且无首尾空白。
func validVerificationVersion(version string) bool {
	if version == "" || len(version) > MaxVerificationVersionBytes || !utf8.ValidString(version) {
		return false
	}
	if strings.TrimSpace(version) != version {
		return false
	}
	return !strings.ContainsFunc(version, func(character rune) bool {
		return unicode.IsControl(character)
	})
}

// verificationScopeDigest 用规范化 JSON 计算范围摘要；对象顺序参与摘要，冻结后不得重排。
func verificationScopeDigest(acceptanceDigest string, objects []VerificationObject) (string, error) {
	return canonicaljson.Fingerprint(verificationScopeDigestPrefix, verificationScopeContent{
		Version:          VerificationScopeVersion,
		AcceptanceDigest: acceptanceDigest,
		Objects:          objects,
	})
}
