package benchmark

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// LocalConfig 描述一套仅在开发者本机使用的评测资产位置与预备参数；它不承载模型密钥。
type LocalConfig struct {
	Version   int                  `yaml:"version"`
	Paths     LocalConfigPaths     `yaml:"paths"`
	Execution LocalConfigExecution `yaml:"execution"`
}

// LocalConfigPaths 保存任务合同、冻结 Agent 配置、私有 verifier 与可丢弃输出的绝对路径。
type LocalConfigPaths struct {
	Manifest      string `yaml:"manifest"`
	AgentConfig   string `yaml:"agent_config"`
	PrivateDir    string `yaml:"private_dir"`
	ResultsDir    string `yaml:"results_dir"`
	WorkspacesDir string `yaml:"workspaces_dir"`
	StateDir      string `yaml:"state_dir"`
}

// LocalConfigExecution 保存 clone/bootstrap 阶段可复跑且无凭据的本机运行参数。
type LocalConfigExecution struct {
	PrepareTimeout string `yaml:"prepare_timeout"`
	PrepareProxy   string `yaml:"prepare_proxy"`
}

// LoadLocalConfig 严格加载本地评测配置，并把相对路径固定到该配置文件所在目录。
func LoadLocalConfig(path string) (LocalConfig, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return LocalConfig{}, fmt.Errorf("resolve local benchmark config: %w", err)
	}
	contents, err := os.ReadFile(absolutePath)
	if err != nil {
		return LocalConfig{}, fmt.Errorf("read local benchmark config: %w", err)
	}
	var configuration LocalConfig
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(&configuration); err != nil {
		return LocalConfig{}, fmt.Errorf("parse local benchmark config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return LocalConfig{}, errors.New("local benchmark config must contain exactly one YAML document")
		}
		return LocalConfig{}, fmt.Errorf("parse local benchmark config: %w", err)
	}
	if err := configuration.resolvePaths(filepath.Dir(absolutePath)); err != nil {
		return LocalConfig{}, err
	}
	if err := configuration.Validate(); err != nil {
		return LocalConfig{}, err
	}
	return configuration, nil
}

// Validate 检查本地配置的版本、完整路径和预备超时，避免 benchmark runner 依赖隐式目录约定。
func (configuration LocalConfig) Validate() error {
	if configuration.Version != 1 {
		return fmt.Errorf("local benchmark config requires version 1, got %d", configuration.Version)
	}
	for name, value := range map[string]string{
		"paths.manifest":       configuration.Paths.Manifest,
		"paths.agent_config":   configuration.Paths.AgentConfig,
		"paths.private_dir":    configuration.Paths.PrivateDir,
		"paths.results_dir":    configuration.Paths.ResultsDir,
		"paths.workspaces_dir": configuration.Paths.WorkspacesDir,
		"paths.state_dir":      configuration.Paths.StateDir,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("local benchmark config requires %s", name)
		}
	}
	if configuration.Execution.PrepareTimeout == "" {
		return errors.New("local benchmark config requires execution.prepare_timeout")
	}
	if duration, err := time.ParseDuration(configuration.Execution.PrepareTimeout); err != nil || duration <= 0 {
		return fmt.Errorf("execution.prepare_timeout must be a positive duration")
	}
	return nil
}

// AssetPaths 返回本地评测配置引用的全部资产路径，供调用方实施 Git 忽略边界检查。
func (configuration LocalConfig) AssetPaths() []string {
	return []string{
		configuration.Paths.Manifest,
		configuration.Paths.AgentConfig,
		configuration.Paths.PrivateDir,
		configuration.Paths.ResultsDir,
		configuration.Paths.WorkspacesDir,
		configuration.Paths.StateDir,
	}
}

// resolvePaths 将配置中的每个相对路径转换为配置文件所在目录的绝对路径。
func (configuration *LocalConfig) resolvePaths(root string) error {
	paths := []*string{
		&configuration.Paths.Manifest,
		&configuration.Paths.AgentConfig,
		&configuration.Paths.PrivateDir,
		&configuration.Paths.ResultsDir,
		&configuration.Paths.WorkspacesDir,
		&configuration.Paths.StateDir,
	}
	for _, value := range paths {
		if strings.TrimSpace(*value) == "" {
			continue
		}
		resolved, err := resolveLocalConfigPath(root, *value)
		if err != nil {
			return err
		}
		*value = resolved
	}
	return nil
}

// resolveLocalConfigPath 规范化本地配置中的单一路径，不接受包含 NUL 的文件系统输入。
func resolveLocalConfigPath(root, value string) (string, error) {
	if strings.ContainsRune(value, '\x00') {
		return "", errors.New("local benchmark path contains NUL")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(root, value)
	}
	resolved, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve local benchmark path: %w", err)
	}
	return resolved, nil
}

// EnsureLocalAssetsIgnored 拒绝 Cairn 仓库内已跟踪或未被忽略的评测资产，防止运行配置和任务资料误入提交。
func EnsureLocalAssetsIgnored(cairnRoot string, assets ...string) error {
	root, err := canonicalCairnRoot(cairnRoot)
	if err != nil {
		return fmt.Errorf("resolve Cairn root: %w", err)
	}
	seen := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		if strings.TrimSpace(asset) == "" {
			continue
		}
		absoluteAsset, err := canonicalLocalAssetPath(asset)
		if err != nil {
			return fmt.Errorf("resolve local benchmark asset: %w", err)
		}
		if _, exists := seen[absoluteAsset]; exists {
			continue
		}
		seen[absoluteAsset] = struct{}{}
		relativeAsset, inside := pathInside(root, absoluteAsset)
		if !inside {
			continue
		}
		if err := requireGitIgnored(root, relativeAsset); err != nil {
			return fmt.Errorf("local benchmark asset %q: %w", relativeAsset, err)
		}
	}
	return nil
}

// canonicalCairnRoot 将调用方路径收敛为 Git worktree 的真实顶层，避免子目录或符号链接绕过本地资产边界检查。
func canonicalCairnRoot(cairnRoot string) (string, error) {
	candidate, err := canonicalLocalAssetPath(cairnRoot)
	if err != nil {
		return "", err
	}
	command := exec.Command("git", "-C", candidate, "rev-parse", "--show-toplevel")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("find Git worktree top-level: %w", err)
	}
	root := strings.TrimSpace(string(output))
	if root == "" {
		return "", errors.New("Git worktree top-level is empty")
	}
	return canonicalLocalAssetPath(root)
}

// canonicalLocalAssetPath 解析已存在的符号链接，并为尚未创建的输出目录保留其规范父目录。
func canonicalLocalAssetPath(asset string) (string, error) {
	absoluteAsset, err := filepath.Abs(asset)
	if err != nil {
		return "", err
	}
	missing := make([]string, 0)
	probe := absoluteAsset
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("no existing parent for %q", asset)
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
	canonicalParent, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	for index := len(missing) - 1; index >= 0; index-- {
		canonicalParent = filepath.Join(canonicalParent, missing[index])
	}
	return canonicalParent, nil
}

// pathInside 判断目标是否位于根目录中（根目录自身也属于其中），并返回供 Git 命令使用的相对路径。
func pathInside(root, target string) (string, bool) {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return relative, true
}

// requireGitIgnored 同时拒绝已跟踪和未被忽略的仓库内本地资产。
func requireGitIgnored(root, relativeAsset string) error {
	if relativeAsset == "." {
		return errors.New("must not be the Cairn repository root")
	}
	if err := runGitCheck(root, []string{"ls-files", "--error-unmatch", "--", relativeAsset}); err == nil {
		return errors.New("must not be Git-tracked")
	} else if !isExpectedGitExit(err, 1) {
		return fmt.Errorf("check tracked state: %w", err)
	}
	output, err := runGitInput(root, []string{"check-ignore", "-z", "-v", "--no-index", "--stdin"}, []byte(relativeAsset+"\x00"))
	if err != nil {
		if isExpectedGitExit(err, 1) {
			return errors.New("must be covered by .gitignore")
		}
		return fmt.Errorf("check ignore state: %w", err)
	}
	if !ignoredByRepositoryGitignore(root, relativeAsset, output) {
		return errors.New("must be covered by a repository .gitignore rule")
	}
	return nil
}

// runGitCheck 执行只读 Git 检查，并保留 stderr 以便用户修正仓库配置。
func runGitCheck(root string, arguments []string) error {
	_, err := runGitOutput(root, arguments)
	return err
}

// runGitOutput 执行只读 Git 命令并返回有限的标准输出，用于检查 Git 所声明的资产状态。
func runGitOutput(root string, arguments []string) (string, error) {
	return runGitInput(root, arguments, nil)
}

// runGitInput 执行只读 Git 命令，并在需要时通过 stdin 传入不经 shell 解释的路径。
func runGitInput(root string, arguments []string, input []byte) (string, error) {
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(output)), nil
	}
	return "", gitCheckError{err: err, output: strings.TrimSpace(string(output))}
}

// ignoredByRepositoryGitignore 只接受仓库工作树中 .gitignore 针对目标资产的匹配规则，不信任用户全局排除或 .git/info/exclude。
func ignoredByRepositoryGitignore(root, relativeAsset, output string) bool {
	fields := strings.Split(output, "\x00")
	if len(fields) != 5 || fields[0] == "" || fields[3] != relativeAsset || fields[4] != "" {
		return false
	}
	rulePath := fields[0]
	if !filepath.IsAbs(rulePath) {
		rulePath = filepath.Join(root, rulePath)
	}
	relativeRulePath, inside := pathInside(root, rulePath)
	if !inside || filepath.Base(relativeRulePath) != ".gitignore" {
		return false
	}
	return relativeRulePath == ".gitignore" || strings.HasSuffix(relativeRulePath, string(filepath.Separator)+".gitignore")
}

// gitCheckError 保存 Git 检查的退出状态和有限诊断信息。
type gitCheckError struct {
	err    error
	output string
}

// Error 返回 Git 检查失败的简要诊断，不展开本地资产内容。
func (failure gitCheckError) Error() string {
	if failure.output == "" {
		return failure.err.Error()
	}
	return failure.err.Error() + ": " + failure.output
}

// Unwrap 暴露底层退出错误，供调用方区分预期的 Git 状态码。
func (failure gitCheckError) Unwrap() error { return failure.err }

// isExpectedGitExit 判断错误是否来自指定的 Git 退出码。
func isExpectedGitExit(err error, expected int) bool {
	var exitError *exec.ExitError
	return errors.As(err, &exitError) && exitError.ExitCode() == expected
}
