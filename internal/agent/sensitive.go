package agent

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fxbin/cairn/internal/credential"
)

// credentialFlagRule 描述已知程序的口令短标志；subcommand 非空时只在
// 该子命令下生效——docker 的 -p 只在 login 中承载口令，run/compose 的
// -p 分别是端口发布与项目名，属于高频合法形态，必须保持可用。
type credentialFlagRule struct {
	subcommand string
	flags      map[string]bool
}

// credentialShortFlags 收录只承载口令语义的短标志；词表外短标志无法从
// 语法上区分口令与普通参数（tar -u、mysql -e），不冒险泛化。
var credentialShortFlags = map[string]credentialFlagRule{
	"docker":     {subcommand: "login", flags: map[string]bool{"p": true}},
	"mysql":      {flags: map[string]bool{"p": true}},
	"ssh-keygen": {flags: map[string]bool{"P": true, "N": true}},
	"redis-cli":  {flags: map[string]bool{"a": true}},
	"sshpass":    {flags: map[string]bool{"p": true}},
	"zip":        {flags: map[string]bool{"P": true}},
}

// execFlagCredentialExemptions 列出词干命中但语义上从不内联携带口令的标志；
// --password-stdin 是各工具推荐的无泄露形式，必须保持可用。
var execFlagCredentialExemptions = map[string]bool{"password_stdin": true}

// urlUserinfoPattern 匹配 scheme://user:pass@ 形式的 URL 内嵌凭据；要求
// userinfo 同时含用户与口令（中间冒号），git@github.com 这类纯用户形式不受影响。
var urlUserinfoPattern = regexp.MustCompile(`(?i)://[^/@\s]+:[^/@\s]+@`)

// urlQueryParameterPattern 提取 URL 查询串中的参数键，供凭据键判定。
var urlQueryParameterPattern = regexp.MustCompile(`[?&]([A-Za-z_][A-Za-z0-9_-]*)=`)

// bareEnvAssignmentPattern 匹配整元素形态的裸环境赋值（docker -e PASSWORD=x、
// env(1)）；行锚定保证 "fix: token=1" 这类消息文本的键切分不成立。
var bareEnvAssignmentPattern = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(\S+)$`)

// uidGidPairPattern 匹配 uid:gid 纯数字对（docker run -u 1000:1000），
// 这是权限对齐值而非口令。
var uidGidPairPattern = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

// authorizationHeaderPattern 匹配请求头取值内的 Authorization 类头；这类头
// 无论值长短都承载凭据，不适用面向长文本的通用脱敏阈值。
var authorizationHeaderPattern = regexp.MustCompile(`(?i)((proxy-)?authorization)\s*:`)

// emptyPasswordLiterals 是口令取值的“空口令”字面量；空值没有泄露语义
// （ssh-keygen -N ""、--password=），不应触发失败关闭。
var emptyPasswordLiterals = map[string]bool{"": true, `""`: true, "''": true}

// sensitiveExecArgv 判定 exec_command 的 argv 是否携带命令行式凭据。结构化
// 规则先于通用脱敏生效：短口令标志、URL userinfo、查询串凭据键、Authorization
// 头取值与裸环境赋值都不依赖面向长文本的长度阈值，在任何持久化之前拒绝。
func sensitiveExecArgv(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	program := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
	rule, known := credentialShortFlags[program]
	if known && rule.subcommand != "" && (len(argv) < 2 || argv[1] != rule.subcommand) {
		known = false
	}
	credentialFlags := map[string]bool(nil)
	if known {
		credentialFlags = rule.flags
	}
	for index, argument := range argv {
		if sensitiveCredentialShortFlag(credentialFlags, argv, index) {
			return true
		}
		if argument == "-u" || argument == "--user" {
			if next := argv[index+1:]; len(next) > 0 && sensitiveUserPasswordValue(next[0]) {
				return true
			}
		}
		if sensitiveHeaderValue(argv, index) {
			return true
		}
		if urlUserinfoPattern.MatchString(argument) {
			return true
		}
		if strings.Contains(argument, "://") {
			for _, match := range urlQueryParameterPattern.FindAllStringSubmatch(argument, -1) {
				if queryCredentialParameter(match[1]) {
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
	if option == argument || (!assigned && execFlagCredentialExemptions[strings.ReplaceAll(key, "-", "_")]) || !credential.Key(key) {
		return false
	}
	if assigned {
		return value != "" && !emptyPasswordLiterals[value]
	}
	if index+1 >= len(argv) {
		return false
	}
	next := argv[index+1]
	return !strings.HasPrefix(next, "-") && !emptyPasswordLiterals[next]
}

// sensitiveUserPasswordValue 判定 -u/--user 的取值是否为 user:pass 形态：
// 含冒号、非纯数字对（uid:gid 是权限对齐）且不含路径分隔符
// （rsync 的 host:/path 是源地址而非口令）。
func sensitiveUserPasswordValue(value string) bool {
	return !strings.HasPrefix(value, "-") && strings.Contains(value, ":") &&
		!uidGidPairPattern.MatchString(value) && !strings.Contains(value, "/")
}

// sensitiveHeaderValue 只在取值跟随 -H/--header 或附着于其上时判定
// Authorization 类头；grep 等工具把 "Authorization:" 当搜索模式时前一元素
// 不是头标志，鉴权排查类任务不受影响。
func sensitiveHeaderValue(argv []string, index int) bool {
	argument := argv[index]
	attached := strings.HasPrefix(argument, "-H") || strings.HasPrefix(argument, "--header=")
	if index == 0 || (!attached && argv[index-1] != "-H" && argv[index-1] != "--header") {
		return false
	}
	return authorizationHeaderPattern.MatchString(argument)
}

// queryCredentialParameter 判定 URL 查询键是否为凭据参数。token/secret 只在
// 精确匹配或复合限定（access_token、auth_token 等）时判定；NextToken、
// page_token 这类分页游标不是凭据，放行以保证分页 API 调测可用。
func queryCredentialParameter(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	if normalized == "token" || normalized == "secret" {
		return true
	}
	for _, marker := range []string{"password", "passwd", "passphrase", "api_key", "apikey",
		"access_key", "private_key", "client_key", "session_key", "credential",
		"access_token", "auth_token", "refresh_token", "secret_"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// sensitiveBareEnvAssignment 判定 argv 元素是否为裸环境赋值且键名敏感；
// 元素锚定匹配保证只有完整标识符形态（PASSWORD=x）参与键判定。
func sensitiveBareEnvAssignment(argument string) bool {
	if strings.HasPrefix(argument, "-") {
		return false
	}
	match := bareEnvAssignmentPattern.FindStringSubmatch(argument)
	return match != nil && credential.Key(match[1])
}

// sensitiveToolIdentity 判定模型给出的工具名或调用 ID 是否本身携带凭据式
// 内容；这两者会原样进入 model_response_finished，不经过参数的结构化解析。
func sensitiveToolIdentity(values ...string) bool {
	for _, value := range values {
		if urlUserinfoPattern.MatchString(value) {
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
// 绕过解析后把原始字符串留进事件日志；顶层 null 同样不作为对象放行。
func isObjectArguments(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}
