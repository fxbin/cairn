// [INPUT]: 依赖父运行配置、委派合同与已持久化的子运行 Journal 事件。
// [OUTPUT]: 构造只读 Verifier 子运行配置，并把子 Run 终态解析为待核验 Finding。
// [POS]: Verifier 是独立只读子运行；结构化输出是待核验发现，不等于确认缺陷。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件连接独立 Verifier 子运行与结构化 Finding 输出。
// Author: fxbin
package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

const (
	// verifierMaxFindings 限制单次独立验证输出的发现数量，防止模型洪泛。
	verifierMaxFindings = 32
	// verifierOutputMaxBytes 限制结构化输出正文体积。
	verifierOutputMaxBytes = 64 << 10
)

var (
	// ErrVerifierOutput 表示 Verifier 结构化输出无法解析或不可信。
	ErrVerifierOutput = errors.New("verifier structured output is invalid")
	// ErrVerifierConfig 表示只读 Verifier 配置无法安全构造。
	ErrVerifierConfig = errors.New("verifier configuration is unsafe")
)

// VerifierRequest 是应用层主动发起独立验证的输入。TargetRunID 指向待验收的
// Worker Run，CriterionIDs 是冻结验收标准；Verifier 不继承 Worker 推理历史。
type VerifierRequest struct {
	TargetRunID  domain.RunID
	Workspace    string
	StateDir     string
	Config       config.Config
	ParentRunID  domain.RunID
	ParentTools  []string
	CriterionIDs []string
	Acceptance   contract.AcceptanceContract
}

// verifierConfig 从父配置派生只读 Verifier 配置。它复用 workflow 的只读派生
// 口径：清空验证命令、应用 read-only preset（write/exec deny、工具面收窄），
// 并强制关闭委派与可选能力，保证 Verifier 不能修改待验收产物或扩大权限。
func verifierConfig(base config.Config) (config.Config, error) {
	child := base
	// read-only preset 禁止验证命令，必须先清空（见 config_resolve.go:116）。
	child.Verification.Commands = nil
	child.Verification.AutoAfterPatch = false
	// 子运行不得再委派，也不得携带依赖外部写权限的可选能力。
	child.Tools.DelegateAsync = false
	child.Skills = nil
	child.MCP = nil
	if err := config.ApplyPreset(&child, config.PresetReadOnly); err != nil {
		return config.Config{}, fmt.Errorf("%w: %w", ErrVerifierConfig, err)
	}
	// 失败关闭：确认只读约束已真正生效。
	if child.Preset != config.PresetReadOnly || child.Policy.Write != "deny" || child.Policy.Exec != "deny" ||
		len(child.Verification.Commands) != 0 {
		return config.Config{}, fmt.Errorf("%w: read-only constraints not enforced", ErrVerifierConfig)
	}
	return child, nil
}

// verifierTaskPrompt 构造冻结的独立验证提示：只包含目标、冻结标准与输出合同，
// 不携带 Worker 推理正文或自我总结。
func verifierTaskPrompt(request VerifierRequest) string {
	criteria := append([]string(nil), request.CriterionIDs...)
	if len(criteria) == 0 {
		for _, criterion := range request.Acceptance.Criteria {
			criteria = append(criteria, criterion.ID)
		}
	}
	sort.Strings(criteria)
	var builder strings.Builder
	builder.WriteString("你是独立只读验证器。只读取实际产物与验收标准，不修改任何文件。\n")
	builder.WriteString("目标运行：" + string(request.TargetRunID) + "\n")
	builder.WriteString("冻结验收合同摘要：" + request.Acceptance.Digest + "\n")
	builder.WriteString("验收标准：" + strings.Join(criteria, ", ") + "\n")
	builder.WriteString("仅输出一个 JSON 对象 {\"findings\":[...]}。每条 finding 字段：id,target_id,category,severity,description,status,criterion_id,location,evidence_refs。")
	builder.WriteString("没有发现问题输出 {\"findings\":[]}。无法定论用 status=inconclusive，不得省略或当作没问题。")
	return builder.String()
}

// verifierOutput 是 Verifier 结构化输出的信封。
type verifierOutput struct {
	Findings []contract.Finding `json:"findings"`
}

// parseVerifierOutput 从子 Run 终态文本解析结构化 Finding。解析失败、空身份、
// 超量或结构非法都失败关闭，不把无法解析的输出当作"没有问题"。
func parseVerifierOutput(text string, request VerifierRequest) ([]contract.Finding, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty verifier output", ErrVerifierOutput)
	}
	if len(trimmed) > verifierOutputMaxBytes {
		return nil, fmt.Errorf("%w: verifier output exceeds size limit", ErrVerifierOutput)
	}
	// 允许模型包裹在 JSON 代码块中；只取首个完整 JSON 对象。
	payload := extractJSONObject(trimmed)
	if payload == "" {
		return nil, fmt.Errorf("%w: no JSON object found", ErrVerifierOutput)
	}
	var output verifierOutput
	if err := json.Unmarshal([]byte(payload), &output); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrVerifierOutput, err)
	}
	if len(output.Findings) > verifierMaxFindings {
		return nil, fmt.Errorf("%w: too many findings", ErrVerifierOutput)
	}
	// 允许的标准的派生必须与 verifierTaskPrompt 一致：CriterionIDs 为空时
	// 回退到 Acceptance.Criteria，否则空集合会跳过绑定检查造成越界。
	criterionIDs := append([]string(nil), request.CriterionIDs...)
	if len(criterionIDs) == 0 {
		for _, criterion := range request.Acceptance.Criteria {
			criterionIDs = append(criterionIDs, criterion.ID)
		}
	}
	if len(criterionIDs) == 0 {
		return nil, fmt.Errorf("%w: no acceptance criteria available for binding", ErrVerifierOutput)
	}
	allowed := make(map[string]struct{}, len(criterionIDs))
	for _, id := range criterionIDs {
		allowed[id] = struct{}{}
	}
	for index := range output.Findings {
		finding := &output.Findings[index]
		finding.Version = contract.FindingVersion
		// Verifier 提出的发现一律是待核验 Proposed；模型自报的
		// Confirmed/Rejected 不被信任，必须经过 Finding Validation 才能推进。
		finding.Status = contract.FindingProposed
		finding.Reviews = nil
		// target_id 强制绑定到本次验证的目标运行，不接受模型自报的其它目标。
		finding.TargetID = string(request.TargetRunID)
		if finding.CriterionID != "" {
			if _, ok := allowed[finding.CriterionID]; !ok {
				return nil, fmt.Errorf("%w: finding references unbound criterion %q", ErrVerifierOutput, finding.CriterionID)
			}
		}
		if err := finding.Validate(); err != nil {
			return nil, fmt.Errorf("%w: finding %d: %v", ErrVerifierOutput, index, err)
		}
	}
	sort.Slice(output.Findings, func(i, j int) bool { return output.Findings[i].ID < output.Findings[j].ID })
	return output.Findings, nil
}

// verifierFinalText 从子 Run Journal 取最终文本：优先 run_completed.final_response，
// 其次最近的 assistant_message_appended。两者都没有时返回空，由 parse 失败关闭。
func verifierFinalText(events []domain.Event) string {
	text := ""
	for _, event := range events {
		switch event.Type {
		case domain.EventRunCompleted:
			var payload domain.RunCompletedPayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.FinalResponse != "" {
				text = payload.FinalResponse
			}
		case domain.EventAssistantMessageAppended:
			if text != "" {
				continue
			}
			var payload domain.AssistantMessagePayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Text != "" {
				text = payload.Text
			}
		}
	}
	return text
}

// extractJSONObject 返回文本中首个顶层 JSON 对象子串；没有则返回空。
func extractJSONObject(text string) string {
	start := strings.IndexByte(text, '{')
	if start < 0 {
		return ""
	}
	depth, inString, escaped := 0, false, false
	for index := start; index < len(text); index++ {
		character := text[index]
		if inString {
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[start : index+1]
			}
		}
	}
	return ""
}
