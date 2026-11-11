// project_instructions.go 定义项目规约快照的纯数据合同：类型、容量、顺序、
// 脱敏与摘要校验，以及从内容寻址制品只读回放的入口；文件系统发现留在 Runtime。
// 本合同不访问当前工作区，也不解释正文命令。
// Author: fxbin
package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 项目指令在新运行中完整冻结；容量不足时拒绝启动，禁止截断规范正文。
const (
	ProjectInstructionsVersion          = 2
	projectInstructionsLegacyVersion    = 1
	ProjectInstructionsMaxBytes         = 32 * 1024
	ProjectInstructionsMaxDocuments     = 128
	ProjectInstructionsMaxEntries       = 100_000
	ProjectInstructionsMaxSnapshotBytes = 1024 * 1024
	ProjectInstructionsFilename         = "AGENTS.md"
)

// ProjectInstructionSource 区分用户全局来源与工作区来源；空值兼容旧版项目快照。
type ProjectInstructionSource string

// ProjectInstructionUser 标记从 Cairn 用户配置目录读取、优先级低于项目规则的来源。
const ProjectInstructionUser ProjectInstructionSource = "user"

// ProjectInstruction 保存逻辑来源与目录作用域；SHA256 绑定脱敏正文，SourceSHA256 记录读取时的原文摘要。
type ProjectInstruction struct {
	Source       ProjectInstructionSource `json:"source,omitempty"`
	Path         string                   `json:"path"`
	Scope        string                   `json:"scope"`
	SHA256       string                   `json:"sha256"`
	SourceSHA256 string                   `json:"source_sha256"`
	Content      string                   `json:"content"`
}

// ProjectInstructions 是一次运行固定使用的规范，用户全局来源在先，项目来源按目录深度和路径排序。
type ProjectInstructions struct {
	Version   int                  `json:"version"`
	Documents []ProjectInstruction `json:"documents"`
}

// Validate 校验历史快照的版本、来源作用域、完整正文、容量和确定性顺序，不访问当前工作区。
func (snapshot ProjectInstructions) Validate() error {
	if snapshot.Version != ProjectInstructionsVersion && snapshot.Version != projectInstructionsLegacyVersion || len(snapshot.Documents) > ProjectInstructionsMaxDocuments {
		return errors.New("invalid project instructions snapshot version or document count")
	}
	contentBytes := 0
	for index, document := range snapshot.Documents {
		if document.Source != "" && (snapshot.Version != ProjectInstructionsVersion || document.Source != ProjectInstructionUser || document.Path != ProjectInstructionsFilename) {
			return errors.New("invalid project instructions snapshot source")
		}
		if !ValidProjectInstructionPath(document.Path) || document.Scope != path.Dir(document.Path) {
			return errors.New("invalid project instructions snapshot scope")
		}
		if index > 0 && !ProjectInstructionDocumentLess(snapshot.Documents[index-1], document) {
			return errors.New("project instructions snapshot sources are duplicated or out of order")
		}
		if !ValidProjectInstructionText(document.Content) || strings.TrimSpace(document.Content) == "" || !bytes.Equal(Redact([]byte(document.Content)), []byte(document.Content)) {
			return errors.New("invalid or unredacted project instructions snapshot content")
		}
		if document.SHA256 != ProjectInstructionDigest([]byte(document.Content)) || !validProjectInstructionDigest(document.SourceSHA256) {
			return errors.New("project instructions snapshot digest mismatch")
		}
		contentBytes += len(document.Content)
		if contentBytes > ProjectInstructionsMaxBytes {
			return errors.New("project instructions snapshot exceeds combined byte limit")
		}
	}
	return nil
}

// ProjectInstructionDocumentLess 将用户全局规则放在项目规则之前，同类来源沿用祖先顺序。
func ProjectInstructionDocumentLess(left, right ProjectInstruction) bool {
	if left.Source != right.Source {
		return left.Source == ProjectInstructionUser
	}
	return projectInstructionLess(left.Path, right.Path)
}

// ArtifactReader 是读取项目指令快照所需的窄制品能力；由具体内容寻址存储实现，
// 避免合同层绑定制品包的完整存储类型。
type ArtifactReader interface {
	OpenBounded(ref Ref, maximumBytes int64) ([]byte, error)
}

// ReadProjectInstructions 只接受 Journal 中完整的内容寻址引用；缺失、损坏或未知结构均失败关闭。
func ReadProjectInstructions(store ArtifactReader, rawReference json.RawMessage) (ProjectInstructions, error) {
	var reference Ref
	if json.Unmarshal(rawReference, &reference) != nil || !validProjectInstructionDigest(reference.SHA256) || reference.Bytes < 1 || reference.Bytes > ProjectInstructionsMaxSnapshotBytes || reference.MediaType != "application/json" || store == nil {
		return ProjectInstructions{}, errors.New("invalid project instructions artifact reference")
	}
	raw, err := store.OpenBounded(reference, ProjectInstructionsMaxSnapshotBytes)
	if err != nil {
		return ProjectInstructions{}, errors.New("project instructions artifact is missing or corrupt")
	}
	if int64(len(raw)) != reference.Bytes || len(raw) > ProjectInstructionsMaxSnapshotBytes {
		return ProjectInstructions{}, errors.New("project instructions artifact size mismatch")
	}
	var snapshot ProjectInstructions
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return ProjectInstructions{}, errors.New("decode project instructions snapshot failed")
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return ProjectInstructions{}, errors.New("project instructions snapshot contains trailing data")
	}
	if err := snapshot.Validate(); err != nil {
		return ProjectInstructions{}, err
	}
	return snapshot, nil
}

// ValidProjectInstructionPath 保持跨平台的工作区相对来源，拒绝可泄露凭据或伪造提示边界的路径文本。
func ValidProjectInstructionPath(relative string) bool {
	return relative != "" && utf8.ValidString(relative) && ValidProjectInstructionText(relative) && !strings.ContainsAny(relative, "\\\r\n\t") && !path.IsAbs(relative) && path.Clean(relative) == relative && relative != ".." && !strings.HasPrefix(relative, "../") && path.Base(relative) == ProjectInstructionsFilename && bytes.Equal(Redact([]byte(relative)), []byte(relative))
}

// ValidProjectInstructionText 允许 Markdown 常用换行与缩进，拒绝二进制内容和终端控制字符。
func ValidProjectInstructionText(content string) bool {
	return utf8.ValidString(content) && strings.IndexFunc(content, func(value rune) bool {
		return unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t'
	}) < 0
}

// projectInstructionLess 将祖先规则排在子目录规则之前，同深度来源按路径排序。
func projectInstructionLess(left, right string) bool {
	leftDepth, rightDepth := strings.Count(left, "/"), strings.Count(right, "/")
	if leftDepth != rightDepth {
		return leftDepth < rightDepth
	}
	return left < right
}

// ProjectInstructionDigest 对实际字节计算与制品引用一致格式的 SHA-256。
func ProjectInstructionDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// validProjectInstructionDigest 只接受完整的小写十六进制摘要，避免不可信引用被解释为文件路径。
func validProjectInstructionDigest(digest string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(digest, prefix))
	return err == nil && strings.ToLower(digest) == digest
}
