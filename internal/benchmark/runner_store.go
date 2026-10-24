// 指标观测与 JSON 提交。
// Author: fxbin
package benchmark

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fxbin/cairn/internal/domain"
)

func observationsFromEvents(events []domain.Event) AttemptMetrics {
	var observations AttemptMetrics
	currentTurn := 0
	turnsBudgetExhausted := false
	seenReadSearch := make(map[string]struct{})
	// 当前轮是否已出现成功 apply_patch；轮末结算 streak。
	turnHadSuccessfulPatch := false
	// 是否已见过 apply_patch 提议；之前记 PrePatchExplorationTurns，
	// 之后才累计 MaxNoSuccessfulPatchStreak。
	seenPatchProposal := false
	noSuccessfulPatchStreak := 0

	endTurn := func() {
		if !seenPatchProposal {
			observations.PrePatchExplorationTurns++
			return
		}
		if turnHadSuccessfulPatch {
			noSuccessfulPatchStreak = 0
			return
		}
		noSuccessfulPatchStreak++
		if noSuccessfulPatchStreak > observations.MaxNoSuccessfulPatchStreak {
			observations.MaxNoSuccessfulPatchStreak = noSuccessfulPatchStreak
		}
	}

	for _, event := range events {
		switch event.Type {
		case domain.EventTurnOpened:
			if currentTurn > 0 {
				endTurn()
			}
			currentTurn++
			turnHadSuccessfulPatch = false
		case domain.EventToolCallProposed:
			var payload struct {
				ToolName  string          `json:"tool_name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			if payload.ToolName == "apply_patch" {
				if observations.FirstPatchTurn == 0 {
					observations.FirstPatchTurn = currentTurn
				}
				seenPatchProposal = true
			}
			if payload.ToolName == "read_file" || payload.ToolName == "search_text" {
				if arguments, valid := canonicalJSONObject(payload.Arguments); valid {
					key := payload.ToolName + "\x00" + arguments
					if _, repeated := seenReadSearch[key]; repeated {
						observations.RepeatedReadSearchCalls++
					} else {
						seenReadSearch[key] = struct{}{}
					}
				}
			}
		case domain.EventOperationSucceeded:
			var payload struct {
				ToolName string `json:"tool_name"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			if payload.ToolName == "apply_patch" {
				seenPatchProposal = true
				turnHadSuccessfulPatch = true
				if observations.FirstSuccessfulPatchTurn == 0 {
					observations.FirstSuccessfulPatchTurn = currentTurn
				}
			}
		case domain.EventVerificationStarted:
			if observations.FirstVerificationTurn == 0 {
				observations.FirstVerificationTurn = currentTurn
			}
		case domain.EventRunFailed:
			var payload struct {
				BudgetDimension string `json:"budget_dimension"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.BudgetDimension == "turns" {
				turnsBudgetExhausted = true
			}
		}
	}
	// 收尾：未显式关闭的最后一轮仍计入 streak。
	if currentTurn > 0 {
		endTurn()
	}
	observations.ZeroPatchTurnsBudgetExhausted = turnsBudgetExhausted && observations.FirstPatchTurn == 0
	return observations
}

// canonicalJSONObject 对工具参数递归按 key 排序，确保 JSON 对象成员顺序不影响
// 重复探索指标；无效、缺失或非对象参数返回 false 而不会进入重复集合。
func canonicalJSONObject(raw json.RawMessage) (string, bool) {
	if !json.Valid(raw) {
		return "", false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return "", false
	}
	encoded, err := marshalCanonicalJSON(object)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// marshalCanonicalJSON 递归生成稳定 JSON：对象按 key 排序，数组保留调用参数的
// 原始顺序，标量保持 encoding/json 的标准数值与字符串语义。
func marshalCanonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := appendCanonicalJSON(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// appendCanonicalJSON 将单个 JSON 值追加为稳定编码；该函数仅服务评测观测，
// 不会改写事件、工具参数或模型请求。
func appendCanonicalJSON(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return err
			}
			buffer.Write(encodedKey)
			buffer.WriteByte(':')
			if err := appendCanonicalJSON(buffer, typed[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
		return nil
	case []any:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := appendCanonicalJSON(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
		return nil
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
		return nil
	}
}

// writeJSON 通过唯一临时文件原子替换结果 JSON，避免并发写者共享固定临时
// 路径，也避免被中断的评测留下半个可读取文件。
func writeJSON(path string, value any) error {
	return writeJSONAtomic(path, value)
}

// writeJSONAtomic 以原子替换语义提交可更新 JSON，并在发布成功后同步目标
// 目录，使文件内容与目录项在支持目录同步的平台上共同持久化。
func writeJSONAtomic(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return commitJSON(path, filepath.Dir(path), append(encoded, '\n'), false)
}

// writeJSONExclusive 以不覆盖语义提交不可变 JSON；临时文件位于调用方指定
// 的同文件系统目录，硬链接提交确保目标一旦存在就失败关闭。
func writeJSONExclusive(path, temporaryDirectory string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(temporaryDirectory, 0o700); err != nil {
		return fmt.Errorf("create JSON temporary directory: %w", err)
	}
	return commitJSON(path, temporaryDirectory, append(encoded, '\n'), true)
}

// commitJSON 把完整同步的临时文件提交到目标路径；exclusive 使用同文件系统
// 硬链接作为原子 create-if-absent，普通模式保留既有原子替换语义。
func commitJSON(path, temporaryDirectory string, content []byte, exclusive bool) error {
	temporary, err := os.CreateTemp(temporaryDirectory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create JSON temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict JSON temporary file: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		return fmt.Errorf("write JSON temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync JSON temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close JSON temporary file: %w", err)
	}
	if err := commitJSONFile(temporaryPath, path, exclusive); err != nil {
		action := "replace"
		if exclusive {
			action = "commit immutable"
		}
		return fmt.Errorf("%s JSON %q: %w", action, filepath.Base(path), err)
	}
	return nil
}

// resultFileName 将本地任务 ID 映射为不会逃出结果目录的稳定文件名。
func resultFileName(taskID string, attempt int) string {
	return safePathComponent(taskID) + fmt.Sprintf("-attempt-%02d.json", attempt)
}

// qualificationFileName 为环境资格记录使用与正式尝试不冲突的稳定文件名。
func qualificationFileName(taskID string, attempt int) string {
	return safePathComponent(taskID) + fmt.Sprintf("-qualification-%02d.json", attempt)
}

// safePathComponent 将任务 ID 规整为本地目录名，保留字母、数字、点、下划线和连字符。
func safePathComponent(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "task"
	}
	return builder.String()
}

// allAttemptsPassed 判断套件是否只包含成功的实际 Agent 尝试。
func allAttemptsPassed(results []AttemptResult) bool {
	for _, result := range results {
		if result.Outcome != "passed" {
			return false
		}
	}
	return true
}
