#!/usr/bin/env bash
# assert-bwrap-e2e.sh 在带 bubblewrap 的 Linux 环境中实跑沙箱门禁用例，
# 并断言用例真实执行：bwrap 缺失或无法建立命名空间、输出出现 SKIP、
# 目标用例缺少 PASS 记录，任一情况都让作业失败，而不是绿着通过。
# CI 的 bwrap-e2e job 调用本脚本；本地 privileged 容器可用同一脚本复现。
# Author: fxbin
set -euo pipefail

sandbox_tests="TestExecCommandRejectsWriteToWorkspaceParent TestExecCommandAllowsWriteInsideWorkspace TestExecCommandAllowsReadOutsideWorkspace"
signal_test="TestSecondInterruptPreemptsCancellation"

# require_bwrap 断言 bwrap 可执行且能建立命名空间；只有版本号存在不代表可用。
require_bwrap() {
	command -v bwrap >/dev/null || {
		echo "bwrap 缺失：沙箱路径无法被验证" >&2
		exit 1
	}
	bwrap --version
	bwrap --unshare-all --die-with-parent --ro-bind / / -- /bin/true || {
		echo "bwrap 无法建立命名空间：环境不具备真实沙箱能力" >&2
		exit 1
	}
}

# required_tests 是本次运行必须出现 PASS 的用例名，由调用方在 run_verbose 前设置。
required_tests=""

# run_verbose 执行一条测试命令并做防空转断言：
#   1. 任何 --- SKIP 都视为失败（用例被跳过不等于通过）；
#   2. 每个 required_tests 用例都必须出现 --- PASS 记录；
#   3. 测试命令本身必须退出 0。
run_verbose() {
	local label="$1"
	shift
	local log status failed name
	log="$(mktemp)"
	set +e
	"$@" 2>&1 | tee "$log"
	status=${PIPESTATUS[0]}
	set -e
	failed=0
	if grep -q -- "--- SKIP" "$log"; then
		echo "防空转失败：${label} 出现 --- SKIP，用例未真实执行" >&2
		failed=1
	fi
	for name in $required_tests; do
		if ! grep -Eq -- "--- PASS: ${name}([[:space:]]|$)" "$log"; then
			echo "防空转失败：${label} 缺少 ${name} 的 PASS 记录" >&2
			failed=1
		fi
	done
	if [ "$status" -ne 0 ]; then
		echo "${label} 失败（exit ${status}）" >&2
		failed=1
	fi
	rm -f "$log"
	return "$failed"
}

require_bwrap

required_tests="$sandbox_tests"
run_verbose "沙箱写边界用例" go test -v ./internal/tools \
	-run "^(${sandbox_tests// /|})$" -count=1

required_tests="$signal_test"
run_verbose "第二次信号抢占用例" go test -v ./test/e2e -tags=e2e \
	-run "^${signal_test}$" -count=1
