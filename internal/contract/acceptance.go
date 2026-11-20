// acceptance.go 定义领域无关的验收合同：标准身份、必需性、检查绑定与稳定摘要。
// 合同只描述“什么算完成”，不执行检查，也不授予权限。
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
	// AcceptanceContractVersion 是当前验收合同格式版本。
	AcceptanceContractVersion = 1
	// MaxAcceptanceCriteria 限制单份合同的标准数量。
	MaxAcceptanceCriteria = 32
	// MaxAcceptanceChecks 限制单条标准绑定的检查数量。
	MaxAcceptanceChecks = 32
	// MaxAcceptanceStatementBytes 限制单条标准正文的字节数。
	MaxAcceptanceStatementBytes = 2 << 10
	// acceptanceDigestPrefix 是合同摘要的版本化域前缀。
	acceptanceDigestPrefix = "cairn-acceptance-contract-v1"
)

var acceptanceIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// AcceptanceCriterion 是一条可独立评估的验收标准。ID 在合同内稳定，Checks
// 引用验证计划中的不透明检查标识；合同本身不包含命令、语言或工具语义。
type AcceptanceCriterion struct {
	ID        string   `json:"id"`
	Statement string   `json:"statement"`
	Required  bool     `json:"required"`
	Checks    []string `json:"checks"`
}

// AcceptanceContract 是冻结的验收合同。Digest 覆盖版本与全部标准，读取方必须
// 重新计算并比对，不能信任外部携带的摘要。
type AcceptanceContract struct {
	Version  int                   `json:"version"`
	Criteria []AcceptanceCriterion `json:"criteria"`
	Digest   string                `json:"digest"`
}

// acceptanceContractContent 是摘要输入；Digest 字段自身不参与计算，避免自引用。
type acceptanceContractContent struct {
	Version  int                   `json:"version"`
	Criteria []AcceptanceCriterion `json:"criteria"`
}

// NewAcceptanceContract 校验标准并计算稳定摘要。输入被深拷贝，调用方后续修改
// 不会改变合同事实。
func NewAcceptanceContract(criteria []AcceptanceCriterion) (AcceptanceContract, error) {
	cloned := CloneAcceptanceCriteria(criteria)
	if err := validateAcceptanceCriteria(cloned); err != nil {
		return AcceptanceContract{}, err
	}
	digest, err := acceptanceContractDigest(cloned)
	if err != nil {
		return AcceptanceContract{}, err
	}
	return AcceptanceContract{Version: AcceptanceContractVersion, Criteria: cloned, Digest: digest}, nil
}

// Validate 复核版本、标准结构和摘要；任何不一致都失败关闭。
func (acceptance AcceptanceContract) Validate() error {
	if acceptance.Version != AcceptanceContractVersion {
		return fmt.Errorf("acceptance contract version %d is unsupported", acceptance.Version)
	}
	if err := validateAcceptanceCriteria(acceptance.Criteria); err != nil {
		return err
	}
	digest, err := acceptanceContractDigest(acceptance.Criteria)
	if err != nil {
		return err
	}
	if acceptance.Digest != digest {
		return errors.New("acceptance contract digest does not match its criteria")
	}
	return nil
}

// Clone 返回合同及其标准的深拷贝。
func (acceptance AcceptanceContract) Clone() AcceptanceContract {
	cloned := acceptance
	cloned.Criteria = CloneAcceptanceCriteria(acceptance.Criteria)
	return cloned
}

// RequiredCheckIDs 返回必需标准引用的去重检查标识，按字典序稳定排列。Core 用
// 它校验冻结验证计划是否覆盖合同。
func (acceptance AcceptanceContract) RequiredCheckIDs() []string {
	seen := make(map[string]struct{})
	for _, criterion := range acceptance.Criteria {
		if !criterion.Required {
			continue
		}
		for _, check := range criterion.Checks {
			seen[check] = struct{}{}
		}
	}
	checks := make([]string, 0, len(seen))
	for check := range seen {
		checks = append(checks, check)
	}
	sort.Strings(checks)
	return checks
}

// CloneAcceptanceCriteria 深拷贝标准及其检查列表。
func CloneAcceptanceCriteria(criteria []AcceptanceCriterion) []AcceptanceCriterion {
	if criteria == nil {
		return nil
	}
	cloned := make([]AcceptanceCriterion, len(criteria))
	for index, criterion := range criteria {
		cloned[index] = criterion
		cloned[index].Checks = append([]string(nil), criterion.Checks...)
	}
	return cloned
}

// ValidAcceptanceIdentifier 判断标准或检查标识是否满足单一词元约束。
func ValidAcceptanceIdentifier(id string) bool {
	return acceptanceIdentifier.MatchString(id)
}

// validateAcceptanceCriteria 拒绝空合同、重复身份、无效正文、无检查标准以及
// 没有任何必需标准的合同。
func validateAcceptanceCriteria(criteria []AcceptanceCriterion) error {
	if len(criteria) == 0 || len(criteria) > MaxAcceptanceCriteria {
		return errors.New("invalid acceptance criterion count")
	}
	identities := make(map[string]struct{}, len(criteria))
	required := 0
	for index, criterion := range criteria {
		if !ValidAcceptanceIdentifier(criterion.ID) {
			return fmt.Errorf("acceptance criterion %d has an invalid id", index+1)
		}
		if _, duplicate := identities[criterion.ID]; duplicate {
			return fmt.Errorf("acceptance criterion id %q is duplicated", criterion.ID)
		}
		identities[criterion.ID] = struct{}{}
		if !validAcceptanceStatement(criterion.Statement) {
			return fmt.Errorf("acceptance criterion %q has an invalid statement", criterion.ID)
		}
		if criterion.Required {
			required++
		}
		if len(criterion.Checks) == 0 || len(criterion.Checks) > MaxAcceptanceChecks {
			return fmt.Errorf("acceptance criterion %q has an invalid check count", criterion.ID)
		}
		checks := make(map[string]struct{}, len(criterion.Checks))
		for _, check := range criterion.Checks {
			if !ValidAcceptanceIdentifier(check) {
				return fmt.Errorf("acceptance criterion %q has an invalid check id", criterion.ID)
			}
			if _, duplicate := checks[check]; duplicate {
				return fmt.Errorf("acceptance criterion %q repeats check %q", criterion.ID, check)
			}
			checks[check] = struct{}{}
		}
	}
	if required == 0 {
		return errors.New("acceptance contract requires at least one required criterion")
	}
	return nil
}

// validAcceptanceStatement 限制正文体积与字符集，并拒绝可识别的凭据材料。
func validAcceptanceStatement(statement string) bool {
	if statement == "" || len(statement) > MaxAcceptanceStatementBytes || !utf8.ValidString(statement) {
		return false
	}
	if strings.TrimSpace(statement) != statement {
		return false
	}
	if strings.ContainsFunc(statement, func(character rune) bool {
		return unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t'
	}) {
		return false
	}
	return string(Redact([]byte(statement))) == statement
}

// acceptanceContractDigest 用规范化 JSON 计算合同摘要；标准顺序参与摘要，调用
// 方不得在冻结后重排。
func acceptanceContractDigest(criteria []AcceptanceCriterion) (string, error) {
	return canonicaljson.Fingerprint(acceptanceDigestPrefix, acceptanceContractContent{
		Version:  AcceptanceContractVersion,
		Criteria: criteria,
	})
}
