# otelc 构建（可选编译期观测探针）

Cairn 采用"默认 plain、可选 otelc"的双构建形态：`plain` 是默认、基线和无探
针发布产物；本目录产出**同一 revision** 的可选 `otelc` flavor 二进制。二者
的事件类型、顺序与语义完全一致——探针只补充基础设施 trace（`net/http`、
`database/sql`），Event Journal 仍是唯一事实源。

## 构建

```bash
./build/otelc/build.sh [输出路径]        # 默认输出 ./cairn-otelc
```

脚本固定工具链版本（`OTELC_VERSION`，默认 v1.1.0）、镜像（`GO_PROXY`，默认
goproxy.cn），构建后把插桩集合摘要写进二进制身份（`BuildIdentity`），并恢复
`go.mod`/`go.sum` 保证 plain 依赖树零污染。

## 运行时遥测

完全遵循标准 OTel 环境变量，无 otelc 专有配置；端点与认证只从环境读取，
不进入 YAML、事件、制品或诊断明文：

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 \
OTEL_LOG_LEVEL=error \
  ./cairn-otelc run
OTEL_SDK_DISABLED=true ./cairn-otelc run   # 显式关闭
```

- **`--json` 用法必须设置 `OTEL_LOG_LEVEL=error`**：注入运行时的日志器硬编
  码写 stdout 且无 off 档（上游限制），error 级别静音全部 Info 噪音后
  stdout 保持纯 NDJSON；构建脚本的纯度门锁定该合同。
- 默认批量导出非阻塞，但 CLI 短进程退出时会丢失末批 span（上游未暴露
  flush/shutdown 钩子）；`OTEL_GO_SIMPLE_SPAN_PROCESSOR=true` 可逐条导出换
  取完整性，代价是收集器无响应时 span 结束路径会停顿——对外部收集器可用
  性有要求，两种模式已在本地验收中分别采证。
- 无收集器时导出目标是 localhost 默认端口，连接失败只降级遥测，不影响
  Agent 语义（已验证：不可达端点下运行正常完成且事件流不变）。
- `cairn doctor` 的 `telemetry.otlp` 字段如实报告
  `unavailable | disabled | otlp`（unavailable = plain 构建）。

## 边界与隐私实测（POC 阶段如实声明）

- 插桩集合以"构建后断言"固化：脚本校验 otelc 规则匹配清单不越出内建标准
  集合（net/http client/server、database/sql、log 关联、runtime 指标、注入
  SDK 引导），任何自定义规则或行为 hook 一律构建失败；摘要进入构建身份。
- 内容级隐私实测通过：导出字节中 prompt、响应体、工具参数、文件内容、
  patch、headers、`db.statement`、密钥样字符串均为 0。
- **已知隐私缺口（部署必读）**：SDK 默认 resource 的 `process.command_args`
  会导出完整命令行，其中包含任务文本与工作区路径。上游未提供去除开关，
  部署 otelc flavor 必须在收集器侧配置属性过滤（丢弃 `process.*`），否则
  不满足隐私合同；等上游提供资源裁剪能力后可移除该要求。
- plain 产物不携带任何探针；otelc 不得伪装成 plain 发布。
