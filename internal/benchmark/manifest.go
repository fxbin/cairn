// Package benchmark 读取本地任务合同，并为可复跑评测提供稳定的任务输入。
package benchmark

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// TaskPack 是调用方显式提供的本地 benchmark 任务合同，不包含 verifier 源码或凭据。
type TaskPack struct {
	Version     int               `yaml:"version" json:"version"`
	Suite       string            `yaml:"suite" json:"suite"`
	IntendedUse string            `yaml:"intended_use" json:"intended_use"`
	TaskCount   int               `yaml:"task_count" json:"task_count"`
	Tasks       []Task            `yaml:"tasks" json:"tasks"`
	Execution   ExecutionContract `yaml:"execution_contract" json:"execution_contract"`
}

// Task 描述一个目标仓库上的自然语言修复任务及其允许边界。
type Task struct {
	ID           string       `yaml:"id" json:"id"`
	Kind         string       `yaml:"kind" json:"kind"`
	Repository   Repository   `yaml:"repository" json:"repository"`
	Language     string       `yaml:"language" json:"language"`
	TaskPrompt   string       `yaml:"task_prompt" json:"task_prompt"`
	Scope        Scope        `yaml:"scope" json:"scope"`
	Verification Verification `yaml:"verification" json:"verification"`
	Runtime      Runtime      `yaml:"runtime" json:"runtime"`
	Preflight    Preflight    `yaml:"preflight" json:"preflight"`
}

// Repository 固定目标仓库、评测基线和仅供人工复核使用的参考提交元数据。
type Repository struct {
	Name             string `yaml:"name" json:"name"`
	URL              string `yaml:"url" json:"url"`
	BaselineCommit   string `yaml:"baseline_commit" json:"baseline_commit"`
	ReferenceCommit  string `yaml:"reference_commit" json:"reference_commit"`
	ReferenceSubject string `yaml:"reference_subject" json:"reference_subject"`
}

// Scope 限制 Agent 可修改与不得修改的路径；runner 会把它写入任务提示词。
type Scope struct {
	AllowedPaths    []string `yaml:"allowed_paths" json:"allowed_paths"`
	ProhibitedPaths []string `yaml:"prohibited_paths" json:"prohibited_paths"`
}

// Verification 保存 bootstrap 与完成前固定执行的无 shell argv 命令。
type Verification struct {
	Bootstrap     []Command `yaml:"bootstrap" json:"bootstrap"`
	Command       Command   `yaml:"command" json:"command"`
	VolatilePaths []string  `yaml:"volatile_paths,omitempty" json:"volatile_paths,omitempty"`
}

// Command 是在指定相对工作目录内运行的一条 argv 命令及其非敏感环境。
type Command struct {
	Argv []string          `yaml:"argv" json:"argv"`
	CWD  string            `yaml:"cwd" json:"cwd"`
	Env  map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// Runtime 声明 Agent 命令执行时必须使用的本地 Docker 镜像。
type Runtime struct {
	DockerImage string `yaml:"docker_image" json:"docker_image"`
}

// Preflight 保存任务定义阶段观察到的依赖条件，不参与模型提示或成败判定。
type Preflight struct {
	Status   string `yaml:"status" json:"status"`
	Observed string `yaml:"observed" json:"observed"`
}

// ExecutionContract 固定一次正式基准的重复、网络与记录要求。
type ExecutionContract struct {
	AttemptsPerTask       int      `yaml:"attempts_per_task" json:"attempts_per_task"`
	AgentWorkspace        string   `yaml:"agent_workspace" json:"agent_workspace"`
	ResetBetweenAttempts  bool     `yaml:"reset_between_attempts" json:"reset_between_attempts"`
	NetworkDuringAgentRun string   `yaml:"network_during_agent_run" json:"network_during_agent_run"`
	BootstrapTimeInBudget bool     `yaml:"bootstrap_time_in_budget" json:"bootstrap_time_in_budget"`
	Evaluation            []string `yaml:"evaluation" json:"evaluation"`
}

// LoadTaskPack 严格解析本地 YAML 合同并拒绝缺失、重复或不安全的命令字段。
func LoadTaskPack(path string) (TaskPack, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return TaskPack{}, fmt.Errorf("read task pack: %w", err)
	}
	var pack TaskPack
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(&pack); err != nil {
		return TaskPack{}, fmt.Errorf("parse task pack: %w", err)
	}
	if err := pack.Validate(); err != nil {
		return TaskPack{}, err
	}
	return pack, nil
}

// Validate 检查任务合同的可复跑前提，并确保 Agent 只会收到受限的路径和命令。
func (pack TaskPack) Validate() error {
	if pack.Version != 1 || strings.TrimSpace(pack.Suite) == "" || pack.IntendedUse != "evaluation_only" {
		return fmt.Errorf("task pack requires version 1, suite, and intended_use=evaluation_only")
	}
	if pack.TaskCount != len(pack.Tasks) || len(pack.Tasks) == 0 {
		return fmt.Errorf("task_count %d does not match %d tasks", pack.TaskCount, len(pack.Tasks))
	}
	if pack.Execution.AttemptsPerTask < 1 || pack.Execution.AgentWorkspace != "disposable_clone" || !pack.Execution.ResetBetweenAttempts || pack.Execution.NetworkDuringAgentRun != "denied" {
		return fmt.Errorf("task pack execution contract must require disposable reset workspaces and denied agent network")
	}
	seen := make(map[string]struct{}, len(pack.Tasks))
	for _, task := range pack.Tasks {
		if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Repository.URL) == "" || strings.TrimSpace(task.Repository.BaselineCommit) == "" || strings.TrimSpace(task.TaskPrompt) == "" {
			return fmt.Errorf("task has incomplete repository identity or prompt")
		}
		if _, exists := seen[task.ID]; exists {
			return fmt.Errorf("duplicate task id %q", task.ID)
		}
		seen[task.ID] = struct{}{}
		if len(task.Scope.AllowedPaths) == 0 || strings.TrimSpace(task.Runtime.DockerImage) == "" || invalidDockerImage(task.Runtime.DockerImage) {
			return fmt.Errorf("task %q requires allowed paths and a valid docker image", task.ID)
		}
		if err := validateCommand(task.Verification.Command); err != nil {
			return fmt.Errorf("task %q verification command: %w", task.ID, err)
		}
		for index, command := range task.Verification.Bootstrap {
			if err := validateCommand(command); err != nil {
				return fmt.Errorf("task %q bootstrap command %d: %w", task.ID, index, err)
			}
		}
		if err := validateVolatilePaths(task.Verification.VolatilePaths); err != nil {
			return fmt.Errorf("task %q verification volatile_paths: %w", task.ID, err)
		}
		for _, path := range append(append([]string(nil), task.Scope.AllowedPaths...), task.Scope.ProhibitedPaths...) {
			if err := validateRelativePath(path); err != nil {
				return fmt.Errorf("task %q scope path %q: %w", task.ID, path, err)
			}
		}
	}
	return nil
}

// validateVolatilePaths 只接受任务 verifier 已知会写入的精确相对路径，避免
// 以宽泛目录或通配符关闭 verification_mutated_workspace 防护。
func validateVolatilePaths(paths []string) error {
	seen := make(map[string]struct{}, len(paths))
	for _, entry := range paths {
		if err := validateRelativePath(entry); err != nil {
			return err
		}
		clean := filepath.ToSlash(filepath.Clean(entry))
		if clean == "." || strings.ContainsAny(clean, "*?[") {
			return fmt.Errorf("must be an exact non-root path")
		}
		if isBuiltInExcludedPath(clean) {
			return fmt.Errorf("must not repeat a built-in excluded path")
		}
		if _, duplicate := seen[clean]; duplicate {
			return fmt.Errorf("contains duplicate path %q", clean)
		}
		seen[clean] = struct{}{}
	}
	return nil
}

// isBuiltInExcludedPath 判断任务合同是否试图重述 verifier 已固定排除的元数据
// 目录或其后代，避免合同看似放宽而实际不改变 observation 边界。
func isBuiltInExcludedPath(path string) bool {
	for _, excluded := range []string{".git", ".agent", ".cairn"} {
		if path == excluded || strings.HasPrefix(path, excluded+"/") {
			return true
		}
	}
	return false
}

// TaskByID 返回指定本地任务；未知 ID 不会默默回退到其他任务。
func (pack TaskPack) TaskByID(id string) (Task, bool) {
	for _, task := range pack.Tasks {
		if task.ID == id {
			return task, true
		}
	}
	return Task{}, false
}

// SelectedTasks 按合同顺序返回用户选择的任务；空选择表示整个本地任务集。
func (pack TaskPack) SelectedTasks(ids []string) ([]Task, error) {
	if len(ids) == 0 {
		return append([]Task(nil), pack.Tasks...), nil
	}
	requested := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, exists := requested[id]; exists {
			return nil, fmt.Errorf("task %q was selected more than once", id)
		}
		requested[id] = struct{}{}
	}
	selected := make([]Task, 0, len(ids))
	for _, task := range pack.Tasks {
		if _, wanted := requested[task.ID]; wanted {
			selected = append(selected, task)
			delete(requested, task.ID)
		}
	}
	if len(requested) != 0 {
		unknown := make([]string, 0, len(requested))
		for id := range requested {
			unknown = append(unknown, id)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown task ids: %s", strings.Join(unknown, ", "))
	}
	return selected, nil
}

// AgentPrompt 构造提供给模型的最小任务说明，刻意不暴露参考提交或人工 oracle。
func (task Task) AgentPrompt() string {
	environment := ""
	if len(task.Verification.Command.Env) != 0 {
		keys := make([]string, 0, len(task.Verification.Command.Env))
		for key := range task.Verification.Command.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		entries := make([]string, 0, len(keys))
		for _, key := range keys {
			entries = append(entries, key+"="+task.Verification.Command.Env[key])
		}
		environment = "\n固定验证环境：" + strings.Join(entries, " ") + "。"
	}
	return fmt.Sprintf("任务：%s\n\n允许修改的路径：\n- %s\n\n禁止修改的路径：\n- %s\n\n完成后必须通过固定验证命令：%s。%s\n不要访问网络、凭据或工作区外的路径。", task.TaskPrompt, strings.Join(task.Scope.AllowedPaths, "\n- "), strings.Join(task.Scope.ProhibitedPaths, "\n- "), strings.Join(task.Verification.Command.Argv, " "), environment)
}

// Fingerprint 计算本地任务合同内容的稳定 SHA-256，用于结果可追溯性。
func (pack TaskPack) Fingerprint() string {
	encoded, err := yaml.Marshal(pack)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// validateCommand 拒绝空 argv、绝对 cwd 和上跳路径，确保 runner 不依赖 shell 解释。
func validateCommand(command Command) error {
	if len(command.Argv) == 0 || strings.TrimSpace(command.Argv[0]) == "" {
		return fmt.Errorf("argv is required")
	}
	for _, argument := range command.Argv {
		if strings.TrimSpace(argument) == "" || strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("argv contains an invalid value")
		}
	}
	if err := validateRelativePath(command.CWD); err != nil {
		return err
	}
	for key, value := range command.Env {
		if !validPublicEnvironmentKey(key) {
			return fmt.Errorf("environment contains an invalid or secret-like key %q", key)
		}
		if value == "" || len(value) > 4096 || strings.ContainsRune(value, '\x00') || strings.Contains(value, "\n") || strings.Contains(value, "\r") || strings.Contains(value, "@") || strings.HasPrefix(value, "sk-") {
			return fmt.Errorf("environment value for %q is invalid or secret-like", key)
		}
	}
	return nil
}

// validPublicEnvironmentKey 只接受普通环境变量名并拒绝常见密钥名，避免把凭据写入
// 本地任务合同；运行时实际密钥仍只能从 provider API key 环境变量读取。
func validPublicEnvironmentKey(key string) bool {
	if key == "" {
		return false
	}
	for index, character := range key {
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && character != '_' && (index == 0 || character < '0' || character > '9') {
			return false
		}
	}
	upper := strings.ToUpper(key)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "AUTHORIZATION"} {
		if strings.Contains(upper, marker) {
			return false
		}
	}
	return true
}

// validateRelativePath 约束合同中的 cwd 与 scope 路径始终位于 disposable clone 内。
func validateRelativePath(value string) error {
	if value == "" || filepath.IsAbs(value) {
		return fmt.Errorf("must be a non-empty relative path")
	}
	clean := filepath.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("must not escape the workspace")
	}
	return nil
}

// invalidDockerImage 拒绝会被 Docker CLI 解释为选项或包含空白的镜像标识。
func invalidDockerImage(image string) bool {
	return image != strings.TrimSpace(image) || strings.HasPrefix(image, "-") || strings.ContainsAny(image, " \t\r\n\x00")
}
