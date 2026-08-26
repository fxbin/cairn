package sandboxeval

import (
	"fmt"
	"sort"
	"strings"

	"github.com/fxbin/cairn/internal/credential"
)

// forbiddenUserDirectoryNames 是不得进入评测子进程的用户目录变量：它们会把
// 宿主用户身份与个人配置面暴露给评测进程。
var forbiddenUserDirectoryNames = map[string]struct{}{
	"HOME": {}, "HOMEPATH": {}, "USERPROFILE": {},
}

// proxySuffix 是代理类变量的识别后缀：宿主代理可能携带内嵌凭据或改变评测
// 流量的路径，属于继承禁运品。
const proxySuffix = "_PROXY"

// BuildEvaluationEnvironment 以空环境起步构造评测子进程环境：只有白名单中的
// 变量会从可信宿主环境取值并注入，随后通过专用通道携带沙箱 Key。白名单
// 出现凭据样名称、沙箱 Key 名称混入普通白名单或 Key 为空都属启动合同破坏，
// 返回错误且不产生任何部分结果。
func BuildEvaluationEnvironment(sourceEnv func(string) (string, bool), allowlist []string, providerKey string) ([]string, error) {
	if sourceEnv == nil {
		return nil, fmt.Errorf("evaluation environment requires a trusted source lookup")
	}
	if strings.TrimSpace(providerKey) == "" {
		return nil, fmt.Errorf("sandbox provider key must be present in trusted memory before launch")
	}
	pairs := make([]string, 0, len(allowlist)+1)
	for _, name := range allowlist {
		if name == SandboxProviderKeyEnv {
			return nil, fmt.Errorf("%s is carried by the dedicated lease channel and must not appear in the allowlist", SandboxProviderKeyEnv)
		}
		if credential.Key(name) {
			return nil, fmt.Errorf("allowlist entry %q looks like a credential and is rejected", name)
		}
		if _, forbidden := forbiddenUserDirectoryNames[name]; forbidden {
			return nil, fmt.Errorf("allowlist entry %q may not expose host user directories", name)
		}
		value, present := sourceEnv(name)
		if !present {
			continue
		}
		pairs = append(pairs, name+"="+value)
	}
	sort.Strings(pairs)
	pairs = append(pairs, SandboxProviderKeyEnv+"="+providerKey)
	return pairs, nil
}

// AuditEvaluationEnvironment 检查一份已成型的子进程环境是否携带隔离破坏信
// 号：凭据样变量名（沙箱 Key 自身除外）、用户目录变量与代理变量。返回的
// 命中名单只含变量名，不含值；调用方据此把评测判为 invalid 并按安全事件
// 处理。
func AuditEvaluationEnvironment(environ []string) []string {
	violations := make([]string, 0)
	for _, entry := range environ {
		name, _, found := strings.Cut(entry, "=")
		if !found || name == "" {
			continue
		}
		if name == SandboxProviderKeyEnv {
			continue
		}
		if credential.Key(name) {
			violations = append(violations, name)
			continue
		}
		if _, forbidden := forbiddenUserDirectoryNames[name]; forbidden {
			violations = append(violations, name)
			continue
		}
		if strings.HasSuffix(strings.ToUpper(name), proxySuffix) {
			violations = append(violations, name)
		}
	}
	sort.Strings(violations)
	return violations
}

// TaskEnvironmentViolation 报告任务进程（exec_command）环境是否可见沙箱 Key：
// Provider 网络调用发生在 Cairn 评测进程，任务子进程的注入名单中出现沙箱
// Key 即为隔离破坏，必须在执行前失败关闭。
func TaskEnvironmentViolation(injected []string) error {
	for _, name := range injected {
		if name == SandboxProviderKeyEnv {
			return fmt.Errorf("task environment must not receive %s", SandboxProviderKeyEnv)
		}
	}
	return nil
}
