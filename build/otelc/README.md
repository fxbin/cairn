# otelc 构建（可选编译期观测探针）

Cairn 采用“默认 plain、可选 otelc”的双构建形态：`plain` 是默认、基线和无探针发布产物；本目录为同一源码 revision 生成可选 `otelc` flavor。两种形态的事件类型、顺序与语义一致，Event Journal 仍是唯一事实源。

## 构建与发布

```bash
./build/otelc/build.sh [输出路径] # 默认输出 ./cairn-otelc
```

脚本会：

1. 固定并双重验证 otelc 命令版本和 Go 模块版本（默认 `v1.1.0`）。
2. 在相同 tags 与构建参数下生成、校验并复核完整插桩规则摘要。
3. 在构建前捕获 revision/dirty、Git 可见源码指纹和 `go list` 实际编译输入指纹，将主模块输入物化到仓库同级临时目录并只在隔离镜像中运行 otelc。构建固定 `GOWORK=off`，源工作区的 `go.mod`/`go.sum` 从始至终不写入；为避免活目录漂移，构建会拒绝本地 `replace`、符号链接输入及其他越出仓库边界的主模块输入。随后执行 plain 构建门禁，并在发布前拒绝构建期间发生的源码变化。
4. 通过 Doctor NDJSON 门禁验证最终二进制的 `otelc` 构建身份。
5. 发布隐藏、按最终二进制 SHA-256 命名且权限为 `0700` 的 payload，再原子切换公开启动器。

构建使用 PID 排他锁。已有锁（包括疑似陈旧锁）一律失败关闭，需确认无构建运行后人工删除。失败不会替换已有启动器，并清理本次临时文件、规则目录和未发布 payload。历史 payload 不自动删除，以免与并发启动旧 launcher 发生竞态；确认没有旧进程或启动动作后可人工清理同目录下不再被 launcher 引用的 `.cairn-otelc-*.bin`。

自定义输出路径在任何锁或临时文件创建前校验，并在最终发布前重新校验。输出若位于仓库内，必须是未被 Git 跟踪且已由 `.gitignore` 覆盖的文件路径；脚本拒绝覆盖已跟踪文件、未忽略文件、目录和符号链接。路径规范化只解析父目录，不跟随最终输出项，最终启动器通过不跟随目标符号链接的原子替换发布。

## 默认安全的运行方式

**只能把公开启动器作为部署入口，不要直接执行隐藏 payload。** 启动器默认强制设置 `OTEL_SDK_DISABLED=true`，因此 otelc 发布物开箱不会导出遥测：

```bash
./cairn-otelc doctor
./cairn-otelc run
```

otelc 注入的 SDK 会生成包含完整命令行的 `process.command_args`。上游当前没有删除该资源属性的开关。只有 Collector 已在接收、存储和转发前删除全部 `process.*` 属性后，才可显式确认并启用：

```bash
CAIRN_OTEL_PROCESS_FILTER_CONFIRMED=true \
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 \
OTEL_LOG_LEVEL=error \
  ./cairn-otelc run
```

该确认变量只是本地安全门，不会进入配置、事件或制品。端点和认证仍只从标准 OTel 环境变量读取，不得写入 Cairn YAML。

如需再次强制关闭，即使已设置确认变量也可设置：

```bash
CAIRN_OTEL_PROCESS_FILTER_CONFIRMED=true \
OTEL_SDK_DISABLED=true ./cairn-otelc run
```

## 运行与诊断合同

- `--json` 场景必须设置 `OTEL_LOG_LEVEL=error`。注入运行时的日志器会写 stdout 且无 off 档；error 级别可保持 NDJSON 纯净，构建脚本会验证这一点。
- 默认批量导出不阻塞，但 CLI 短进程退出时可能丢失末批 span。`OTEL_GO_SIMPLE_SPAN_PROCESSOR=true` 可逐条导出，代价是收集器异常时结束 span 的路径可能停顿。
- `cairn doctor` 的 `telemetry.otlp` 聚合三个 signal：`unavailable` 表示 plain；`disabled` 表示 SDK 全局关闭或三个 exporter 均为 `none`；`otlp` 表示至少一个 OTLP exporter；`enabled` 表示仅 metrics `prometheus` 启用；`config_error` 表示 exporter 配置不受上游支持。
- exporter 名称遵循上游 autoexport 的精确语义：区分大小写且不裁剪。traces/logs 支持空值（默认 `otlp`）、`otlp`、`none`；metrics 还支持 `prometheus`。
- 无收集器时默认目标是 localhost 标准端口；连接失败只降级遥测，不改变 Agent 事件语义。

## 插桩与隐私边界

- 脚本按 `module_path`、`package_name`、规则 `target` 和规则来源路径校验 otelc 的完整 `matched.json`；未知字段、未知包、自定义规则或行为 hook 均失败关闭。
- 规则 JSON 规范化后整体计算 SHA-256，正式构建后再次计算并要求完全一致；摘要写入 `BuildIdentity`，身份指纹可由事件回放复算。
- 已验证的观测内容不包含 prompt、响应体、工具参数、文件内容、patch、headers、`db.statement` 或密钥样字符串。
- `process.command_args` 是已知上游隐私缺口，因此公开启动器默认禁用 SDK。未完成 Collector 过滤或未显式确认时，不得启用遥测。
- plain 产物不携带探针；otelc 构建身份无效时 Run/Rewind 在持久化前失败关闭，Doctor 同时报告 `build_valid=false`。
