// tool_policy_contract.go 定义策略门禁判定所需的工具数据合同：命令参数解析、
// 会话校验、补丁错误类型与恢复反馈、MCP 授权投影及工作区信任判定。这些是
// 纯数据与纯函数，不含任何执行能力；策略层与工具层共享同一组定义，保证审批
// 绑定的指纹与实际执行的参数永不分叉。JSON 字段顺序与错误文案绑定既有审批
// 指纹与恢复反馈，迁移时必须逐位保留。
// Author: fxbin
package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ExecArguments 是 exec_command 的受限执行参数；策略层在审批前解析同一结构，
// 确保审批指纹绑定的就是将要实际执行的命令。
type ExecArguments struct {
	Argv           []string          `json:"argv"`
	CWD            string            `json:"cwd"`
	TimeoutMS      int               `json:"timeout_ms"`
	MaxOutputBytes int               `json:"max_output_bytes"`
	Env            map[string]string `json:"env"`
	// EnvRefs 将进程环境变量映射到凭证名（而非明文）；执行时经 EnvResolver
	// 取值，Journal 只见引用名，降低密钥进事件面的风险。
	EnvRefs map[string]string `json:"env_refs,omitempty"`
}

// exec_command 默认与上限是工具合同的硬边界：策略层与执行器共享同一组语义常量。
const (
	// ExecDefaultTimeoutMS 是未指定 timeout_ms 时的默认时限。
	ExecDefaultTimeoutMS = 120_000
	// ExecMaxTimeoutMS 是 timeout_ms 允许的最大值。
	ExecMaxTimeoutMS = 3_600_000
	// ExecDefaultMaxOutputBytes 是未指定 max_output_bytes 时的默认输出上限。
	ExecDefaultMaxOutputBytes = 256 * 1024
	// ExecMaxOutputBytesLimit 是 max_output_bytes 允许的最大值。
	ExecMaxOutputBytesLimit = 1024 * 1024
)

// SessionAction 是 exec_session 的会话动作类型。
type SessionAction string

const (
	// SessionOpen 创建受控 tmux 会话。
	SessionOpen SessionAction = "open"
	// SessionWrite 向会话注入输入。
	SessionWrite SessionAction = "write"
	// SessionRead 读取自游标起的输出。
	SessionRead SessionAction = "read"
	// SessionClose 关闭会话并回收进程组。
	SessionClose SessionAction = "close"
)

// SessionRequest 是 exec_session 的统一入参。
type SessionRequest struct {
	Action SessionAction `json:"action"`
	// SessionID 是会话身份；open 时可省略由运行时分配。
	SessionID string `json:"session_id,omitempty"`
	// Content 是 write 的输入文本（摘要入 Journal，不记原文）。
	Content string `json:"content,omitempty"`
	// CWD 是会话工作目录，必须在工作区内。
	CWD string `json:"cwd,omitempty"`
}

// ExecSessionMaxWriteBytes 限制单次会话写入的字节数。
const ExecSessionMaxWriteBytes = 4 * 1024

// DecodeToolArguments 严格解码单个 JSON 参数对象并拒绝未知字段。
func DecodeToolArguments(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if decoder.More() {
		return fmt.Errorf("invalid arguments: multiple JSON values")
	}
	return nil
}

// ParseExecArguments 校验命令参数并填充与执行器相同的默认值；策略层在审批前
// 调用它，确保审批始终绑定到将被实际执行的命令。
func ParseExecArguments(raw json.RawMessage) (ExecArguments, error) {
	arguments := ExecArguments{CWD: ".", TimeoutMS: ExecDefaultTimeoutMS, MaxOutputBytes: ExecDefaultMaxOutputBytes}
	if err := DecodeToolArguments(raw, &arguments); err != nil {
		return ExecArguments{}, err
	}
	if len(arguments.Argv) == 0 || arguments.Argv[0] == "" {
		return ExecArguments{}, errors.New("argv requires a non-empty command")
	}
	for _, argument := range arguments.Argv {
		if argument == "" {
			return ExecArguments{}, errors.New("argv cannot contain empty values")
		}
	}
	if arguments.TimeoutMS < 1 || arguments.TimeoutMS > ExecMaxTimeoutMS || arguments.MaxOutputBytes < 1 || arguments.MaxOutputBytes > ExecMaxOutputBytesLimit {
		return ExecArguments{}, errors.New("timeout_ms or max_output_bytes is outside its allowed range")
	}
	arguments.CWD = NormalizeWorkspaceAlias(arguments.CWD)
	return arguments, nil
}

// NormalizeWorkspaceAlias 将模型常用的 /workspace 绝对别名收敛为受限的相对工作目录。
func NormalizeWorkspaceAlias(cwd string) string {
	if cwd == "/workspace" {
		return "."
	}
	if strings.HasPrefix(cwd, "/workspace/") {
		return strings.TrimPrefix(cwd, "/workspace/")
	}
	return cwd
}

// ValidateSessionRequest 校验会话动作；不执行任何外部调用。
func ValidateSessionRequest(request SessionRequest) error {
	switch request.Action {
	case SessionOpen:
		if strings.TrimSpace(request.CWD) == "" {
			return fmt.Errorf("cwd is required for open")
		}
	case SessionWrite:
		if strings.TrimSpace(request.SessionID) == "" {
			return fmt.Errorf("session_id is required for write")
		}
		if strings.TrimSpace(request.Content) == "" {
			return fmt.Errorf("content is required for write")
		}
		if !utf8.ValidString(request.Content) {
			return fmt.Errorf("content must be valid utf-8")
		}
		if len(request.Content) > ExecSessionMaxWriteBytes {
			return fmt.Errorf("content exceeds %d bytes", ExecSessionMaxWriteBytes)
		}
	case SessionRead, SessionClose:
		if strings.TrimSpace(request.SessionID) == "" {
			return fmt.Errorf("session_id is required for %s", request.Action)
		}
	default:
		return fmt.Errorf("unknown session action %q", request.Action)
	}
	return nil
}

// sensitiveSessionVerbs 是 write 需二次确认的敏感动词（大小写不敏感）。
var sensitiveSessionVerbs = []string{"sudo", "rm -rf", "curl", "wget", "chmod 777", "shutdown", "reboot", "mkfs", "dd if="}

// IsSensitiveSessionInput 判断会话输入是否触发二次确认。
func IsSensitiveSessionInput(content string) bool {
	lower := strings.ToLower(content)
	for _, verb := range sensitiveSessionVerbs {
		if strings.Contains(lower, verb) {
			return true
		}
	}
	return false
}

// MCPProposal 是不产生连接或调用的本地授权投影；身份来自已持久化目录。
type MCPProposal struct {
	Server            string          `json:"server"`
	Identity          string          `json:"identity"`
	EnvironmentDigest string          `json:"environment_digest,omitempty"`
	CatalogDigest     string          `json:"catalog_digest,omitempty"`
	ToolDigest        string          `json:"tool_digest,omitempty"`
	ToolName          string          `json:"tool_name"`
	Arguments         json.RawMessage `json:"arguments"`
	Effect            string          `json:"effect"`
	Stdio             bool            `json:"stdio"`
	Local             bool            `json:"local"`
	Summary           string          `json:"summary"`
}

// PatchArgumentsError 描述可由模型重建调用形状修复的参数错误；Mode 仅用于
// 选择已知模式的恢复提示，Message 不包含文件正文。
type PatchArgumentsError struct {
	Mode    string
	Message string
}

// Error 保留原参数拒绝说明，使既有错误前缀仍可被诊断与日志识别。
func (err *PatchArgumentsError) Error() string {
	return err.Message
}

// PatchPreconditionError 描述 update 或 delete 的已给定摘要与当前文件不一致；
// 只携带相对路径和摘要，绝不携带文件内容，供上层生成可重放的恢复反馈。
type PatchPreconditionError struct {
	Path           string
	ExpectedSHA256 string
	ActualSHA256   string
}

// Error 返回不含文件内容的稳定前置条件失败说明。
func (err *PatchPreconditionError) Error() string {
	return "expected_sha256 does not match current file; read_file is required before retrying apply_patch"
}

// PatchTextMatchError 描述已通过摘要前置条件后发生的精确匹配失配；
// ReplacementIndex 从零开始，Path 由 Plan 填充为已解析的相对路径，不携带文本。
type PatchTextMatchError struct {
	Path                string
	ReplacementIndex    int
	ActualOccurrences   int
	ExpectedOccurrences int
}

// Error 报告失败位置与次数，并要求保留读取原文的空白和换行，不提供猜测替换。
func (err *PatchTextMatchError) Error() string {
	return fmt.Sprintf("replacement old text occurs %d time(s), want %d at replacements[%d]; copy old exactly from read_file including whitespace and newlines; target=%s", err.ActualOccurrences, err.ExpectedOccurrences, err.ReplacementIndex, err.Path)
}

// PatchFailureResult 将非空规划错误转换为无副作用的模型恢复反馈；已知参数与
// 文本失配只附带固定动作和校验元数据，其余错误保留原 invalid 语义。
func PatchFailureResult(cause error) ToolResult {
	var argumentError *PatchArgumentsError
	if errors.As(cause, &argumentError) {
		mode := argumentError.Mode
		required := []string{"path", "mode", "expected_sha256"}
		omit := []string{}
		next := "Choose create, update, or delete and rebuild the complete request with path, mode, and expected_sha256. Use content only for create and replacements only for update. Do not repeat unchanged invalid arguments."
		switch mode {
		case "create":
			required = append(required, "content")
			omit = append(omit, "replacements")
			next = "Rebuild a create request with path, mode=create, expected_sha256=absent, and content only; omit replacements."
		case "update":
			required = append(required, "replacements")
			omit = append(omit, "content")
			next = "Rebuild an update request with path, mode=update, expected_sha256 from read_file, and replacements only; omit content. Copy old exactly from the returned source, including whitespace and newlines. Do not repeat unchanged invalid arguments."
		case "delete":
			omit = append(omit, "content", "replacements")
			next = "Rebuild a delete request with path, mode=delete, and expected_sha256 from read_file; omit both content and replacements."
		default:
			mode = ""
		}
		data, err := json.Marshal(struct {
			Mode           string   `json:"mode,omitempty"`
			RequiredFields []string `json:"required_fields"`
			OmitFields     []string `json:"omit_fields"`
			NextAction     string   `json:"next_action"`
		}{Mode: mode, RequiredFields: required, OmitFields: omit, NextAction: next})
		if err != nil {
			return ToolFailure("invalid", "patch recovery feedback could not be encoded")
		}
		result := ToolFailure(CodeInvalidPatchArguments, cause.Error())
		result.Data = data
		return result
	}
	var matchError *PatchTextMatchError
	if errors.As(cause, &matchError) {
		data, err := json.Marshal(struct {
			Path                string `json:"path"`
			ReplacementIndex    int    `json:"replacement_index"`
			ActualOccurrences   int    `json:"actual_occurrences"`
			ExpectedOccurrences int    `json:"expected_occurrences"`
			NextAction          string `json:"next_action"`
		}{Path: matchError.Path, ReplacementIndex: matchError.ReplacementIndex, ActualOccurrences: matchError.ActualOccurrences, ExpectedOccurrences: matchError.ExpectedOccurrences,
			NextAction: "Use the exact original block returned by read_file, preserving whitespace and newlines. If it is no longer available, read only the needed target range. Rebuild a smaller exact replacement with its current sha256; omit content and do not normalize, guess, or repeat the unmatched old text."})
		if err != nil {
			return ToolFailure("invalid", "patch recovery feedback could not be encoded")
		}
		result := ToolFailure(CodePatchTextMismatch, cause.Error())
		result.Data = data
		return result
	}
	return ToolFailure("invalid", cause.Error())
}

// TrustMarkerFile 是工作区信任标记文件名。
const TrustMarkerFile = ".cairn-trusted"

// UntrustedMarkerFile 显式标记工作区为不信任；无标记视为信任（向后兼容）。
const UntrustedMarkerFile = ".cairn-untrusted"

// IsExplicitlyUntrustedWorkspace 检查是否存在显式拒绝标记。
func IsExplicitlyUntrustedWorkspace(root string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(root, UntrustedMarkerFile))
	return err == nil && info.Mode().IsRegular()
}

// GuardWorkspaceWrite 在显式拒绝标记下拒绝写；无标记视为信任（向后兼容）。
func GuardWorkspaceWrite(root, operation string) error {
	if IsExplicitlyUntrustedWorkspace(root) {
		return fmt.Errorf("workspace %q is explicitly untrusted (remove %s to enable %s)", root, UntrustedMarkerFile, operation)
	}
	return nil
}

// sha256Reference 计算字节切片的十六进制 SHA-256 引用；空内容返回空串。
func SHA256Reference(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
