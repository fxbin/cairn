// [INPUT]: 依赖Memory评测合同、冻结资产与受控执行入口。
// [OUTPUT]: 提供执行计划摘要的唯一公式。冻结器和执行器共用。。
// [POS]: 带外评测工具链，不进入最小Agent内核。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件承载评测冻结合同的输入摘要计算：解析计划从这里取得唯一事实源，冻
// 结工具与执行器不得各自另写公式。
//
// Author: fxbin
package benchmark

import (
	"fmt"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/config"
)

// resolvedExecutionPlanPrefix 是解析计划摘要的版本化域前缀。
const resolvedExecutionPlanPrefix = "cairn-resolved-execution-plan-v1"

// ResolvedExecutionPlanDigest 以规范化 JSON 计算解析计划摘要：全部"字段→值→
// 来源"记录参与指纹，覆盖实际生效的轮次、工具调用、输入/输出 token、墙钟与
// 成本上限及其来源链。空计划视为残缺冻结材料拒绝产出。
func ResolvedExecutionPlanDigest(entries []config.ResolutionEntry) (string, error) {
	if len(entries) == 0 {
		return "", fmt.Errorf("resolved execution plan requires recorded entries")
	}
	return canonicaljson.Fingerprint(resolvedExecutionPlanPrefix, struct {
		Entries []config.ResolutionEntry `json:"entries"`
	}{Entries: append([]config.ResolutionEntry(nil), entries...)})
}
