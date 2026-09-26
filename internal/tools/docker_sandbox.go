// docker_sandbox.go 定义 tools 与 benchmark 共用的 Docker 沙箱 argv 骨架，
// 保证“无网络、只读根、最小能力、失败关闭”的安全姿态只有单一 owner。
//
// Author: fxbin
package tools

import "strconv"

// DockerSandboxBaseArgs 返回 Docker 沙箱共用骨架 argv：禁止隐式拉取、无网络、
// 只读根文件系统、丢弃全部 capabilities、no-new-privileges、PID 上限、仅工
// 作区 bind mount、受限 /tmp 以及 HOME/TMPDIR 指向容器内临时目录。
//
// 调用方必须在其前补充 `run --rm`（及可选 `--name`/`--user`），在其后追加
// 白名单 `--env` 与镜像名；禁止删改本函数返回的安全旗标，否则评测与执行
// 工具的安全姿态将再次分叉。
func DockerSandboxBaseArgs(pidsLimit int, workspaceRoot, containerCWD string) []string {
	return []string{
		"--pull", "never",
		"--network", "none",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(pidsLimit),
		"--mount", "type=bind,src=" + workspaceRoot + ",dst=/workspace",
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=64m",
		"--workdir", containerCWD,
		"--env", "HOME=/tmp",
		"--env", "TMPDIR=/tmp",
	}
}
