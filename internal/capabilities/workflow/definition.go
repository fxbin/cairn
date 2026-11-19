/*
 * [INPUT]: 依赖工作流步骤种类、共享制品和凭据脱敏合同。
 * [OUTPUT]: 提供定义草案、不可变修订、节点和制品锚点。
 * [POS]: 领域层校验线性依赖与返工边界。应用层验证实际运行权限。
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 */
// 本文件定义可经宿主保存的工作流模板。
// Author: fxbin
package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

const (
	// DefinitionSchemaVersion 标识不可变定义制品的当前模式。
	DefinitionSchemaVersion = 1
	// MaxDefinitionArtifactBytes 约束持久化正文与冷恢复所读的制品大小。
	MaxDefinitionArtifactBytes = 512 * 1024
	// StepKindScript 表示由运行授权链执行固定 argv 的节点。
	StepKindScript = "script"

	maxDefinitionNodes = 32
	maxNodeAttempts    = 4
	maxNodeArguments   = 64
	maxNodeTextBytes   = 16 * 1024
	maxArgumentBytes   = 4 * 1024
	maxNameBytes       = 256
)

var definitionIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// DefinitionID 在单一工作区内标识工作流模板。
type DefinitionID string

// DefinitionDraft 是宿主收到的待校验模板。首次保存允许宿主生成标识。
type DefinitionDraft struct {
	ID    DefinitionID `json:"id,omitempty"`
	Name  string       `json:"name"`
	Nodes []Node       `json:"nodes"`
}

// Definition 是不可变模板修订。工作区哈希与版本随正文进入内容摘要。
type Definition struct {
	SchemaVersion int          `json:"schema_version"`
	WorkspaceHash string       `json:"workspace_hash"`
	ID            DefinitionID `json:"id"`
	Revision      uint64       `json:"revision"`
	Name          string       `json:"name"`
	Nodes         []Node       `json:"nodes"`
}

// DefinitionRef 锚定工作流运行采用的模板修订及其正文制品。
type DefinitionRef struct {
	ID       DefinitionID `json:"id"`
	Revision uint64       `json:"revision"`
	Artifact contract.Ref `json:"artifact"`
}

// Node 声明线性节点及其前序制品依赖。执行权限源于有效配置和显式授权。
type Node struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Prompt      string   `json:"prompt,omitempty"`
	Argv        []string `json:"argv,omitempty"`
	Model       string   `json:"model,omitempty"`
	ReadOnly    bool     `json:"read_only,omitempty"`
	MaxAttempts int      `json:"max_attempts"`
	Inputs      []string `json:"inputs,omitempty"`
	RetryFrom   string   `json:"retry_from,omitempty"`
}

// NewDefinition 固定工作区和修订，复制草案并规范化模板正文。
func NewDefinition(workspaceHash string, draft DefinitionDraft, revision uint64) (Definition, error) {
	if len(draft.Nodes) == 0 || len(draft.Nodes) > maxDefinitionNodes {
		return Definition{}, errors.New("invalid workflow node count")
	}
	if !validDefinitionText(draft.Name, maxNameBytes) {
		return Definition{}, errors.New("invalid workflow name")
	}
	if draft.ID == "" {
		draft.ID = DefinitionID(domain.NewID("wfl"))
	}
	nodes := make([]Node, len(draft.Nodes))
	for index, node := range draft.Nodes {
		if !validDefinitionText(node.Prompt, maxNodeTextBytes) {
			return Definition{}, fmt.Errorf("invalid workflow prompt at node %d", index+1)
		}
		node.Prompt = string(contract.Redact([]byte(node.Prompt)))
		node.Model = strings.TrimSpace(node.Model)
		if node.MaxAttempts == 0 {
			node.MaxAttempts = 1
		}
		nodes[index] = node
	}
	definition := Definition{
		SchemaVersion: DefinitionSchemaVersion,
		WorkspaceHash: workspaceHash,
		ID:            draft.ID,
		Revision:      revision,
		Name:          strings.TrimSpace(string(contract.Redact([]byte(draft.Name)))),
		Nodes:         nodes,
	}
	if err := definition.Validate(); err != nil {
		return Definition{}, err
	}
	for index := range definition.Nodes {
		node := &definition.Nodes[index]
		node.Argv = slices.Clone(node.Argv)
		node.Inputs = slices.Clone(node.Inputs)
	}
	return definition, nil
}

// Validate 拒绝未知模式、重复节点以及越过前序的制品依赖或返工。
func (definition Definition) Validate() error {
	if definition.SchemaVersion != DefinitionSchemaVersion {
		return errors.New("unsupported workflow definition schema")
	}
	if !validDefinitionDigest(definition.WorkspaceHash) || !definition.ID.Valid() ||
		definition.Revision == 0 || definition.Revision > math.MaxInt64 {
		return errors.New("invalid workflow definition identity")
	}
	if strings.TrimSpace(definition.Name) == "" || !validDefinitionText(definition.Name, maxNameBytes) {
		return errors.New("invalid workflow name")
	}
	if len(definition.Nodes) == 0 || len(definition.Nodes) > maxDefinitionNodes {
		return errors.New("invalid workflow node count")
	}
	prior := make(map[string]string, len(definition.Nodes))
	for index, node := range definition.Nodes {
		if !validDefinitionID(node.ID) || prior[node.ID] != "" {
			return fmt.Errorf("invalid or duplicate workflow node identity at index %d", index+1)
		}
		if err := node.validate(prior); err != nil {
			return fmt.Errorf("workflow node %d: %w", index+1, err)
		}
		prior[node.ID] = node.Kind
	}
	return nil
}

// Valid 判断定义标识是否符合存储与界面的单一词元约束。
func (id DefinitionID) Valid() bool { return validDefinitionID(string(id)) }

// Validate 检查定义锚点的身份与有界正文承诺。读取方再核验正文摘要。
func (reference DefinitionRef) Validate() error {
	if !reference.ID.Valid() || reference.Revision == 0 || reference.Revision > math.MaxInt64 ||
		!validDefinitionDigest(reference.Artifact.SHA256) || reference.Artifact.Bytes <= 0 ||
		reference.Artifact.Bytes > MaxDefinitionArtifactBytes || reference.Artifact.MediaType != "application/json" ||
		reference.Artifact.TruncatedPreview != "" {
		return errors.New("invalid workflow definition reference")
	}
	return nil
}

// validate 按节点种类约束执行字段，并检查引用只能消费已声明的前序。
func (node Node) validate(prior map[string]string) error {
	if node.MaxAttempts < 1 || node.MaxAttempts > maxNodeAttempts {
		return errors.New("invalid workflow attempt limit")
	}
	if !validDefinitionText(node.Prompt, maxNodeTextBytes) || !validDefinitionText(node.Model, maxNameBytes) ||
		strings.ContainsFunc(node.Model, unicode.IsControl) || string(contract.Redact([]byte(node.Model))) != node.Model {
		return errors.New("invalid workflow node text")
	}
	switch node.Kind {
	case StepKindExecute:
		if strings.TrimSpace(node.Prompt) == "" || len(node.Argv) != 0 {
			return errors.New("agent node requires a prompt and accepts no argv")
		}
	case StepKindScript:
		if node.Prompt != "" || node.Model != "" || node.ReadOnly {
			return errors.New("script node accepts only fixed command arguments")
		}
		if err := validateWorkflowArgv(node.Argv); err != nil {
			return err
		}
	case StepKindVerify, StepKindAccept:
		if len(node.Argv) != 0 || node.Model != "" || node.ReadOnly {
			return errors.New("verification and human nodes accept no execution overrides")
		}
	default:
		return errors.New("unknown workflow node kind")
	}
	if node.RetryFrom != "" && prior[node.RetryFrom] != StepKindExecute && prior[node.RetryFrom] != StepKindScript {
		return errors.New("workflow rework requires an earlier agent or script node")
	}
	if len(node.Inputs) > len(prior) {
		return errors.New("invalid workflow input count")
	}
	inputs := make(map[string]bool, len(node.Inputs))
	for _, input := range node.Inputs {
		kind := prior[input]
		if kind == "" || kind == StepKindAccept || inputs[input] {
			return errors.New("workflow inputs must reference distinct earlier producer nodes")
		}
		inputs[input] = true
	}
	return nil
}

// validateWorkflowArgv 保护固定 argv 的字节语义，并拒绝可识别的凭据。
func validateWorkflowArgv(argv []string) error {
	if len(argv) == 0 || len(argv) > maxNodeArguments || strings.TrimSpace(argv[0]) == "" {
		return errors.New("script node requires executable argv within the argument limit")
	}
	for _, argument := range argv {
		flag, _, _ := strings.Cut(argument, "=")
		sensitiveFlag := strings.HasPrefix(flag, "-") && contract.SensitiveKey(strings.TrimLeft(flag, "-"))
		if argument == "" || !validDefinitionText(argument, maxArgumentBytes) || sensitiveFlag ||
			string(contract.Redact([]byte(argument))) != argument {
			return errors.New("invalid or sensitive script argument")
		}
	}
	return nil
}

// validDefinitionText 限制持久化正文，拒绝无效 UTF-8 和不可见控制字符。
func validDefinitionText(text string, maxBytes int) bool {
	return len(text) <= maxBytes && utf8.ValidString(text) && !strings.ContainsFunc(text, func(character rune) bool {
		return unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t'
	})
}

// validDefinitionID 校验单一词元并拒绝可识别的敏感值。
func validDefinitionID(id string) bool {
	return definitionIdentifier.MatchString(id) && string(contract.Redact([]byte(id))) == id
}

// validDefinitionDigest 接受制品与工作区共用的 SHA-256 摘要编码。
func validDefinitionDigest(value string) bool {
	digest, ok := strings.CutPrefix(value, "sha256:")
	if !ok || len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}
