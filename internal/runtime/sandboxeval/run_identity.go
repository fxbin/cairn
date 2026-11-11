// 本文件承载候选评测单次运行的身份派生合同：以版本化域前缀与规范化四元组
// 产出确定性标识，供本地记录命名、崩溃重入识别与"已闭合 Run 不得重跑"的
// 审计核对使用。
//
// Author: fxbin
package sandboxeval

import (
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/canonicaljson"
)

// sandboxRunIDPrefix 是单个 Run 身份的版本化域前缀。
const sandboxRunIDPrefix = "cairn-sandbox-run-v1"

// SandboxRunID 由评测身份与执行四元组派生单次 Run 的确定性标识：
// sha256("cairn-sandbox-run-v1\x00" || canonical_json(四字段))。同一输入恒得
// 同一 ID，本地评测记录以它命名，恢复与重入据此识别既有事实而不重跑已闭合
// Run。臂取值必须是闭合两臂之一，尝试序号从一开始计数；任一输入残缺即拒绝
// 派生，不给残缺事实生成可用身份。
func SandboxRunID(evaluationID string, arm SuiteArm, taskID string, attemptIndex int) (string, error) {
	if evaluationID == "" || taskID == "" || attemptIndex < 1 {
		return "", errors.New("sandbox run identity requires an evaluation id, a task id and a one-based attempt index")
	}
	switch arm {
	case ArmBaseline, ArmVariant:
	default:
		return "", fmt.Errorf("sandbox run arm %q is not in the closed lexicon", arm)
	}
	view := struct {
		EvaluationID string   `json:"evaluation_id"`
		Arm          SuiteArm `json:"arm"`
		TaskID       string   `json:"task_id"`
		AttemptIndex int      `json:"attempt_index"`
	}{evaluationID, arm, taskID, attemptIndex}
	return canonicaljson.Fingerprint(sandboxRunIDPrefix, view)
}
