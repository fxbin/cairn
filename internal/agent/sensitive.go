package agent

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fxbin/cairn/internal/credential"
)

// credentialShortFlags 列出已知程序中承载口令的短标志；分离与附着两种形式
// （docker login -p x、mysql -px）都按凭据通道处理。词表外的短标志无法从
// 语法上区分口令与普通参数，不冒险泛化。
var credentialShortFlags = map[string]map[string]bool{
	"docker":     {"p": true},
	"mysql":      {"p": true},
	"ssh-keygen": {"P": true, "N": true},
	"redis-cli":  {"a": true},
}

// execFlagCredentialExemptions 列出词干命中但语义上从不内联携带口令的标志；
// --password-stdin 是各工具推荐的无泄露形式，必须保持可用。
var execFlagCredentialExemptions = map[string]bool{"password_stdin": true}

// urlUserinfoPattern 匹配 scheme://user:pass@ 形式的 URL 内嵌凭据；要求
// userinfo 同时含用户与口令（中间冒号），git@github.com 这类纯用户形式不受影响。
var urlUserinfoPattern = regexp.MustCompile(`(?i)://[^/@\s]+:[^/@\s]+@`)

// urlQueryParameterPattern 提取 URL 查询串中的参数键，供 credential.Key 判定。
var urlQueryParameterPattern = regexp.MustCompile(`[?&]([A-Za-z_][A-Za-z0-9_-]*)=`)

// authorizationHeaderPattern 匹配命令行内嵌的 Authorization 类请求头；这类头
// 无论值长短都承载凭据，不适用面向长文本的通用脱敏阈值。
var authorizationHeaderPattern = regexp.MustCompile(`(?i)(^|[\s"'-])((proxy-)?authorization)\s*:`)

// emptyPasswordLiterals 是分离式口令标志后跟的“空口令”字面量；空值没有泄露
// 语义（如 ssh-keygen -N ""），不应触发失败关闭。
var emptyPasswordLiterals = map[string]bool{"": true, `""`: true, "''": true}

// sensitiveExecArgv 判定 exec_command 的 argv 是否携带命令行式凭据。结构化
// 规则先于通用脱敏生效：短口令标志、URL userinfo、查询串凭据键、Authorization
// 头与裸环境赋值都不依赖面向长文本的长度阈值，在任何持久化之前拒绝。
func sensitiveExecArgv(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	credentialFlags := credentialShortFlags[filepath.Base(strings.TrimSuffix(argv[0], ".exe"))]
	for index, argument := range argv {
		if sensitiveCredentialShortFlag(credentialFlags, argv, index) {
			return true
		}
		if argument == "-u" || argument == "--user" {
			if next := argv[index+1:]; len(next) > 0 && !strings.HasPrefix(next[0], "-") && strings.Contains(next[0], ":") {
				return true
			}
		}
		if authorizationHeaderPattern.MatchString(argument) || urlUserinfoPattern.MatchString(argument) {
			return true
		}
		if strings.Contains(argument, "://") {
			for _, match := range urlQueryParameterPattern.FindAllStringSubmatch(argument, -1) {
				if credential.Key(match[1]) {
					return true
				}
			}
		}
		if sensitiveBareEnvAssignment(argument) {
			return true
		}
		if sensitiveCredentialLongFlag(argv, index, argument) {
			return true
		}
	}
	return false
}

// sensitiveCredentialShortFlag 判定已知程序的口令短标志：附着值形式
// （-phunter2）直接命中；分离形式要求下一个元素是非空、非标志位的值。
func sensitiveCredentialShortFlag(credentialFlags map[string]bool, argv []string, index int) bool {
	if credentialFlags == nil {
		return false
	}
	argument := argv[index]
	if len(argument) < 2 || argument[0] != '-' || argument[1] == '-' || argument[1] == '=' {
		return false
	}
	flag, attached := argument[1], len(argument) > 2
	if !credentialFlags[string(flag)] {
		return false
	}
	if attached {
		return true
	}
	if index+1 >= len(argv) {
		return false
	}
	next := argv[index+1]
	return !strings.HasPrefix(next, "-") && !emptyPasswordLiterals[next]
}

// sensitiveCredentialLongFlag 判定 --key=value / --key value 形式的长标志；
// 键名敏感性统一交给 credential.Key，豁免清单排除从不内联口令的形式。
func sensitiveCredentialLongFlag(argv []string, index int, argument string) bool {
	if !strings.HasPrefix(argument, "-") {
		return false
	}
	option := strings.TrimLeft(argument, "-")
	key, value, assigned := strings.Cut(option, "=")
	if option == argument || execFlagCredentialExemptions[strings.ReplaceAll(key, "-", "_")] || !credential.Key(key) {
		return false
	}
	if assigned && value != "" {
		return true
	}
	if index+1 >= len(argv) {
		return false
	}
	next := argv[index+1]
	return !assigned && !strings.HasPrefix(next, "-") && !emptyPasswordLiterals[next]
}

// sensitiveBareEnvAssignment 判定 argv 中的裸环境赋值（docker -e PASSWORD=x、
// env(1)）：非标志元素按第一个等号切键，键名敏感性交给 credential.Key。
func sensitiveBareEnvAssignment(argument string) bool {
	if strings.HasPrefix(argument, "-") || !strings.Contains(argument, "=") {
		return false
	}
	key, value, _ := strings.Cut(argument, "=")
	return value != "" && credential.Key(key)
}

// sensitiveToolIdentity 判定模型给出的工具名或调用 ID 是否本身携带凭据式
// 内容；这两者会原样进入 model_response_finished，不经过参数的结构化解析。
func sensitiveToolIdentity(values ...string) bool {
	for _, value := range values {
		if authorizationHeaderPattern.MatchString(value) || urlUserinfoPattern.MatchString(value) {
			return true
		}
		if sensitiveBareEnvAssignment(value) {
			return true
		}
	}
	return false
}

// isObjectArguments 判定工具参数是否为 JSON 对象。字符串化（双重编码）的参数
// 无法通过结构化敏感检查，按畸形参数失败关闭并允许模型纠正重试，而不是
// 绕过解析后把原始字符串留进事件日志。
func isObjectArguments(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil
}
