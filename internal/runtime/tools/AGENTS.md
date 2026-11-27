# internal/runtime/tools/
> L2 | Parent: ../../../AGENTS.md

工具执行与工作区基础设施。固定注册表只保存显式工具。
路径、进程、沙箱和原子发布保留基础边界。能力正文不能改变权限。

## 成员

AGENTS.md: 模块职责地图与文件成员入口。
apply_patch.go: ApplyPatch 执行绑定旧摘要的单文件原子改写。发布前同步临时文件。
apply_patch_test.go: 回归 ApplyPatch 执行绑定旧摘要的单文件原子改写。发布前同步临时文件。
archive_preview.go: ArchivePreview 有界列举归档条目。不解压到工作区。
archive_preview_test.go: 回归 ArchivePreview 有界列举归档条目。不解压到工作区。
browser_tools.go: BrowserTool 消费显式浏览器驱动。工具动作仍受 Policy 约束。
content_digest.go: ContentDigest 为文本生成稳定 SHA-256。会话与内容比较共用算法。
content_primitives_test.go: 验证内容包名称边界与通用摘要格式。
contract_aliases.go: ContractAlias 暴露既有纯数据合同。参数校验归共享合同层。
delegate_task.go: DelegateTask 提供静态工具声明。实际子运行由宿主回调启动。
delegate_task_async_test.go: 验证委派工具的显式异步参数声明。
delegate_task_test.go: 回归 DelegateTask 提供静态工具声明。实际子运行由宿主回调启动。
docker_sandbox.go: DockerSandboxBaseArgs 统一受限容器参数。基础沙箱与评测共用安全姿态。
exec_command_unix.go: ExecCommand 只接收 argv 并限制环境、输出和时限。取消须确认执行边界回收。
exec_command_unix_test.go: 回归 ExecCommand 只接收 argv 并限制环境、输出和时限。取消须确认执行边界回收。
exec_runner.go: ExecRunner 选择 host、Seatbelt 或 Docker 边界。失败不回退弱隔离。
exec_runner_test.go: 回归 ExecRunner 选择 host、Seatbelt 或 Docker 边界。失败不回退弱隔离。
exec_session.go: ExecSession 显式启用受控 tmux 生命周期。输入正文不写入 Journal。
exec_session_test.go: 回归 ExecSession 显式启用受控 tmux 生命周期。输入正文不写入 Journal。
exec_workspace_boundary_test.go: 验证进程只能写入工作区与合法临时根。
git_read.go: GitStatus 与 GitDiff 只执行只读子命令。路径通过工作区解析。
git_read_test.go: 回归 GitStatus 与 GitDiff 只执行只读子命令。路径通过工作区解析。
git_write.go: GitCommit 只暂存明确路径并创建提交。不接受任意 Git 命令。
lsp_diagnostics.go: LspDiagnostics 选择已有语言诊断器。缺少诊断器时失败关闭。
mcp_process_unix.go: MCPProcess 复用受控双向 stdio 和沙箱。关闭须确认进程与容器退出。
model_search.go: ModelSearch 本地筛选模型目录并限制结果数。不发网络请求。
model_search_test.go: 回归 ModelSearch 本地筛选模型目录并限制结果数。不发网络请求。
observation_parallel_test.go: 验证并行观察摘要稳定且覆盖工具忽略文件。
ossandbox_darwin.go: OSSandbox 在 macOS 使用 Seatbelt。写范围由宿主策略收窄；OSSandboxConfinementReason 只做推导，供诊断提前给出规则无法生成的原因码。
ossandbox_errors.go: OSSandbox 错误标记缺少原生隔离能力。调用方据此失败关闭。
ossandbox_linux.go: OSSandbox 在 Linux 使用 bubblewrap。宿主只读，临时根与工作区有界可写，保留路径下的临时根拒绝；OSSandboxConfinementReason 只做推导，供诊断提前给出规则无法生成的原因码。
ossandbox_linux_test.go: 回归 OSSandbox 在 Linux 使用 bubblewrap。宿主只读，临时根与工作区有界可写。
ossandbox_other.go: OSSandbox 在不支持的平台拒绝宿主约束请求。不伪造隔离。
ossandbox_types.go: SeatbeltOptions 表达原生沙箱的写档位。数据合同不执行进程。
package_store.go: PackageStore 校验清单与资源摘要后原子安装。不执行内容包脚本。
package_store_test.go: 回归 PackageStore 校验清单与资源摘要后原子安装。不执行内容包脚本。
package_sync.go: PackageSync 通过根句柄有界遍历临时包。目录由深到浅同步后发布。
package_sync_test.go: 验证内容包同步的根句柄与有界嵌套遍历。
parallel_readonly.go: ReadonlyToolNames 只分类固定只读工具。动态目录不自动加入并行集合。
parallel_readonly_test.go: 回归 ReadonlyToolNames 只分类固定只读工具。动态目录不自动加入并行集合。
project_instructions.go: ProjectInstructions 从受限来源读取并脱敏规约。快照由合同层绑定和校验。
project_instructions_local_material_test.go: 验证规约发现排除 Git 忽略与运行时材料。
project_instructions_test.go: 回归 ProjectInstructions 从受限来源读取并脱敏规约。快照由合同层绑定和校验。
read_only.go: ListFiles、ReadFile 和 SearchText 共享工作区边界。输出保持有界。
read_only_test.go: 回归 ListFiles、ReadFile 和 SearchText 共享工作区边界。输出保持有界。
registry.go: Registry 静态注册工具并消费通用模型视图。能力投影由工具元数据声明。
registry_model_result_test.go: 验证注册工具的模型视图认领与原始回执。
registry_test.go: 回归 Registry 静态注册工具并消费通用模型视图。能力投影由工具元数据声明。
review_regression_test.go: 验证内容发布、模板读取和禁用标记的路径边界。
run_tests.go: RunTests 通过受控执行器运行测试 argv。结果汇总保持结构化。
run_tests_diagnostics_test.go: 验证测试输出和语言诊断的结构化解析。
sandbox_confinement.go: 给沙箱规则无法生成的失败关闭错误附上稳定原因码。错误文本原样给命令结果，原因码不含路径，供只读诊断使用。
sandbox_confinement_test.go: 验证规则无法生成的错误带原因码，只读推导与真实启动同源。
sandbox_temp.go: 决定沙箱始终可写的临时根。固定系统临时根加当前进程的临时目录，过宽则失败关闭；只读档下不得覆盖工作区。
sandbox_temp_exec_test.go: 验证真实 OS 沙箱下自定义临时目录可写，过宽或只读档下覆盖工作区时启动前失败。
sandbox_temp_profile_test.go: 验证写范围只多出当前临时目录，过宽时 profile 生成失败。
sandbox_temp_test.go: 验证可写临时根的追加、保持与失败关闭规则。
seatbelt_darwin.go: SeatbeltProfile 生成默认拒绝写的原生规则。临时根含当前进程的临时目录，网络与写权限由策略决定。
seatbelt_darwin_test.go: 回归 SeatbeltProfile 生成默认拒绝写的原生规则。网络与写权限由策略决定。
seatbelt_other.go: Seatbelt 在非 macOS 返回不支持。不提供伪隔离实现。
seatbelt_profile_temp_test.go: 验证临时目录的符号链接与规范路径写规则。
seatbelt_workspace_boundary_test.go: 验证宿主写范围及临时目录不被拒绝规则覆盖。
send_message.go: SendMessage 校验输入并调用宿主投递回调。不直接写目标 Journal。
send_message_bind.go: MessageDeliverFunc 按工具实例注入。不使用全局消息通道。
send_message_test.go: 回归 SendMessage 校验输入并调用宿主投递回调。不直接写目标 Journal。
session_backend_tmux.go: TmuxSessionBackend 执行受控会话动作。环境与工作目录由宿主冻结。
template.go: TemplateStore 校验显式参数与摘要。渲染不执行 shell 插值。
template_test.go: 回归 TemplateStore 校验显式参数与摘要。渲染不执行 shell 插值。
terminal_capability.go: TerminalCapabilities 选择终端显示能力。显式覆盖优先于自动探测。
terminal_capability_test.go: 回归 TerminalCapabilities 选择终端显示能力。显式覆盖优先于自动探测。
todo.go: TodoBoard 保存运行内计划投影。计划项不能授予权限。
web_fetch.go: WebFetch 限制网络目标、大小和时限。网络副作用仍走授权链。
web_fetch_test.go: 回归 WebFetch 限制网络目标、大小和时限。网络副作用仍走授权链。
web_search.go: WebSearch 使用显式受限端点查询。结果数量与响应大小均有界。
web_search_test.go: 回归 WebSearch 使用显式受限端点查询。结果数量与响应大小均有界。
worker_pool.go: WorkerPool 有界并发执行已规划任务。返回顺序与提交顺序一致。
worker_pool_test.go: 回归 WorkerPool 有界并发执行已规划任务。返回顺序与提交顺序一致。
workspace.go: Workspace 统一路径解析和观察摘要。忽略规则不缩小 verifier 视图。
workspace_read_file.go: ReadBoundedFile 通过根句柄读取普通文件。路径、类型和容量均需校验。
workspace_read_file_test.go: 回归 ReadBoundedFile 通过根句柄读取普通文件。路径、类型和容量均需校验。
workspace_read_file_unix_test.go: 验证 FIFO 在打开前被拒绝，避免无限等待。
workspace_test.go: 回归 Workspace 统一路径解析和观察摘要。忽略规则不缩小 verifier 视图。
workspace_trust.go: WorkspaceTrust 保存显式信任标记。不可信目录禁止敏感写入。
workspace_trust_test.go: 回归 WorkspaceTrust 保存显式信任标记。不可信目录禁止敏感写入。

[PROTOCOL]: Update this header on change, then check AGENTS.md
