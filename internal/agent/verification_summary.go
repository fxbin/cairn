// verification_summary.go 提供验证失败的确定性结构化摘要，供反馈事件与 UI 证据投影使用。
package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/tools"
)

// verificationFailureTailBytes 限制反馈中的诊断尾部长度，避免失败日志撑爆上下文。
const verificationFailureTailBytes = 512

// verificationFailureFacts 从工具 Result 解析出的可展示失败事实；字段缺失时省略。
type verificationFailureFacts struct {
	Code       string `json:"code"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Signal     string `json:"signal,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`
	StdoutTail string `json:"stdout_tail,omitempty"`
	Preview    string `json:"preview,omitempty"`
}

// parseVerificationFailure 从 Result.Data 或 Preview 提取失败事实，不信任非 JSON 载荷。
func parseVerificationFailure(result tools.Result) verificationFailureFacts {
	facts := verificationFailureFacts{Code: result.Code}
	if len(result.Data) > 0 {
		var data struct {
			ExitCode int    `json:"exit_code"`
			Signal   string `json:"signal"`
			Stderr   string `json:"stderr"`
			Stdout   string `json:"stdout"`
		}
		if err := json.Unmarshal(result.Data, &data); err == nil {
			code := data.ExitCode
			facts.ExitCode = &code
			facts.Signal = data.Signal
			facts.StderrTail = tailText(data.Stderr, verificationFailureTailBytes)
			facts.StdoutTail = tailText(data.Stdout, verificationFailureTailBytes)
		}
	}
	if facts.StderrTail == "" && facts.StdoutTail == "" {
		facts.Preview = tailText(result.Preview, verificationFailureTailBytes)
	}
	return facts
}

// verificationFailureFeedback 组装稳定可读的失败反馈；格式固定，便于 UI 与恢复逻辑引用。
func verificationFailureFeedback(commandID string, result tools.Result) string {
	facts := parseVerificationFailure(result)
	var b strings.Builder
	fmt.Fprintf(&b, "Verification command %s failed (code=%s", commandID, facts.Code)
	if facts.ExitCode != nil {
		fmt.Fprintf(&b, ", exit=%d", *facts.ExitCode)
	}
	if facts.Signal != "" {
		fmt.Fprintf(&b, ", signal=%s", facts.Signal)
	}
	b.WriteString(").")
	if facts.StderrTail != "" {
		b.WriteString("\nstderr_tail: ")
		b.WriteString(string(artifact.Redact([]byte(facts.StderrTail))))
	}
	if facts.StdoutTail != "" {
		b.WriteString("\nstdout_tail: ")
		b.WriteString(string(artifact.Redact([]byte(facts.StdoutTail))))
	}
	if facts.Preview != "" {
		b.WriteString("\npreview: ")
		b.WriteString(string(artifact.Redact([]byte(facts.Preview))))
	}
	b.WriteString("\nnext: Inspect the persisted result, repair the workspace, and run the configured verification again.")
	return b.String()
}

// tailText 返回去掉首尾空白后的末尾片段，空输入为空串。
func tailText(content string, limit int) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	if limit > 0 && len(content) > limit {
		content = content[len(content)-limit:]
		if idx := strings.IndexByte(content, '\n'); idx >= 0 && idx < len(content)-1 {
			content = content[idx+1:]
		}
	}
	return content
}
