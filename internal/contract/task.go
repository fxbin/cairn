// Package contract 定义普通 Run 与 benchmark 共用的版本化产品合同：
// TaskSpec、RunManifest 与 Result。全部类型都是事件日志、冻结配置制品与
// 调用方任务身份的只读投影，不构成第二事实源；任何字段都不得携带密钥、
// 私有任务正文、完整本地工作区路径或 verifier 内容。
package contract

import (
	"crypto/sha256"
	"encoding/hex"
)

// taskSchemaV1 是 TaskSpec 的当前产品合同版本；演进必须升版本并保留解码兼容。
const taskSchemaV1 = 1

// TaskKindFreeform 表示由 CLI 直接提交的自由任务文本。
const TaskKindFreeform = "freeform"

// TaskKindBenchmark 表示来自冻结 TaskPack 的基准任务。
const TaskKindBenchmark = "benchmark_task"

// TaskSpec 是一次运行所执行任务的版本化、非敏感身份。它只携带摘要与标识，
// 不携带任务正文（正文保存在事件与私有本地资产中，由各自 owner 负责）。
type TaskSpec struct {
	SchemaVersion int               `json:"schema_version"`
	Kind          string            `json:"kind"`
	Source        string            `json:"source,omitempty"`
	PromptDigest  string            `json:"prompt_digest"`
	Benchmark     *BenchmarkTaskRef `json:"benchmark,omitempty"`
}

// BenchmarkTaskRef 定位一个冻结基准任务的非敏感事实：任务与套件标识、
// TaskPack 摘要、来源仓库与基线、私有 verifier 摘要和任务镜像。它由
// benchmark runner 从 TaskPack 与准备事实组装，是 TaskPack 的单向投影。
type BenchmarkTaskRef struct {
	TaskID              string `json:"task_id"`
	SuiteID             string `json:"suite_id,omitempty"`
	TaskPackFingerprint string `json:"task_pack_fingerprint"`
	RepositoryName      string `json:"repository_name,omitempty"`
	RepositoryURL       string `json:"repository_url,omitempty"`
	BaselineCommit      string `json:"baseline_commit,omitempty"`
	SourceRevision      string `json:"source_revision,omitempty"`
	VerifierFingerprint string `json:"verifier_fingerprint,omitempty"`
	DockerImage         string `json:"docker_image,omitempty"`
}

// FreeformTaskSpec 为普通 CLI 运行构建任务身份；digest 只依赖任务全文，
// 保证同一任务文本在任意工作区得到同一身份。
func FreeformTaskSpec(task string) TaskSpec {
	digest := sha256.Sum256([]byte(task))
	return TaskSpec{
		SchemaVersion: taskSchemaV1,
		Kind:          TaskKindFreeform,
		Source:        "cli",
		PromptDigest:  "sha256:" + hex.EncodeToString(digest[:]),
	}
}

// BenchmarkTaskSpec 为基准尝试构建任务身份；输入全部来自冻结 TaskPack、
// 套件运行标识与准备阶段事实，不读取也不复制任务正文或 verifier 内容。
func BenchmarkTaskSpec(prompt string, reference BenchmarkTaskRef) TaskSpec {
	digest := sha256.Sum256([]byte(prompt))
	return TaskSpec{
		SchemaVersion: taskSchemaV1,
		Kind:          TaskKindBenchmark,
		Source:        "benchmark",
		PromptDigest:  "sha256:" + hex.EncodeToString(digest[:]),
		Benchmark:     &reference,
	}
}
