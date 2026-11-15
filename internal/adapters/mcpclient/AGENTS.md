# internal/adapters/mcpclient/
> L2 | Parent: ../../../AGENTS.md

MCP 协议与本机认证的叶子适配。仅由应用层显式装配。
目录接纳只消费成功回执。会话未确认关闭时不能伪造释放成功。

## 成员

AGENTS.md: 模块职责地图与文件成员入口。
catalog.go: Manager 持有惰性 MCP 会话与目录投影。发现结果先形成待持久化证据。
catalog_http_test.go: 通过本地真实 HTTP 验证发现、漂移与 Journal 冷恢复。
catalog_sync.go: CatalogSyncer 提供目录失效状态计算。Manager 未接入周期刷新。
catalog_sync_test.go: 回归CatalogSyncer 提供目录失效状态计算。Manager 未接入周期刷新。
http.go: HTTPTransport 限制目标、重定向和解压后响应。不向其他原点传播凭据。
http_test.go: 回归HTTPTransport 限制目标、重定向和解压后响应。不向其他原点传播凭据。
identity.go: AuthenticationBinding 区分本机当前身份与会话身份。摘要不包含原始密钥。
oauth_login.go: OAuthLogin 只由显式登录入口执行。授权码流程绑定 PKCE 与服务器身份。
oauth_store.go: OAuthStore 私有保存令牌并串行刷新。文件原子发布且权限受限。
oauth_test.go: 验证私有令牌、刷新并发、身份替换与显式登录。
redact.go: SecretFilter 精确移除会话实际凭据。外部结果脱敏后进入 Journal。
retry_executor.go: RetryExecutor 提供独立重试算法。Manager 未装配自动重试。
retry_executor_test.go: 回归RetryExecutor 提供独立重试算法。Manager 未装配自动重试。
retry_policy.go: RetryPolicy 表达幂等重试建议。策略对象不自动发起请求。
retry_policy_test.go: 回归RetryPolicy 表达幂等重试建议。策略对象不自动发起请求。
stdio_test.go: 验证 stdio 传输、环境白名单与进程取消回收。
transport.go: WireSession 复用官方 SDK 会话。操作预算与关闭确认独立约束。
user_directory_test.go: 验证用户根 OAuth 存储、权限与按身份退出。

[PROTOCOL]: Update this header on change, then check AGENTS.md
