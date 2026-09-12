#!/usr/bin/env bash
# 构建 cairn 的可选 otelc flavor（"默认 plain、可选 otelc"双构建形态的观测探针产物）。
# 默认产物永远是 plain；本脚本产出同 revision 的额外 opt-in 二进制，并以
# go.mod/go.sum 快照恢复保证 plain 依赖树零污染。
#
# 契约要点（与既定的可选编译期观测探针决策一致）：
# - 工具链版本固定（OTELC_VERSION，默认 v1.1.0），不依赖上游自动 pin 作为
#   唯一复现手段：插桩集合以 otelc 生成的规则匹配清单摘要形式经 ldflags
#   写入二进制，并进入 BuildIdentity。
# - 插桩集合 = 固定版本工具链的内建标准插桩（依赖图实际命中的 net/http
#   client/server、database/sql、log 关联、runtime 进程指标与注入 SDK 引导）；
#   断言编译集不越出该集合：任何自定义规则或行为 hook 一律构建失败。
# - 不使用开发期 --rules、任意函数 hook 或行为插桩。
# - 运行时遥测状态由标准 OTel 环境变量决定（OTEL_EXPORTER_OTLP_ENDPOINT、
#   OTEL_SDK_DISABLED 等）；端点与认证只从环境读取。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OTELC_VERSION="${OTELC_VERSION:-v1.1.0}"
OTELC_BIN="${OTELC_BIN:-$(go env GOPATH)/bin/otelc}"
OUTPUT="${1:-${REPO_ROOT}/cairn-otelc}"
GO_PROXY="${GO_PROXY:-https://goproxy.cn,direct}"
# 内建标准插桩集合白名单（matched.json 中的规则 path 子串）；越出即失败，
# 保证探针永不升级为行为注入。
ALLOWED_TARGETS="net/http|database/sql|otelc/instrumentation/log|otelc/instrumentation/runtime|opentelemetry.io/otel/init"
BUILD_PACKAGE="./cmd/cairn"
LDFLAGS_IDENTITY="-X github.com/fxbin/cairn/internal/buildinfo.Flavor=otelc -X github.com/fxbin/cairn/internal/buildinfo.ToolchainVersion=${OTELC_VERSION}"

cd "${REPO_ROOT}"

if [ ! -x "${OTELC_BIN}" ]; then
  echo "==> 安装 otelc ${OTELC_VERSION}（GOPROXY=${GO_PROXY}）"
  GOPROXY="${GO_PROXY}" go install "go.opentelemetry.io/otelc/tool/cmd/otelc@${OTELC_VERSION}"
fi

echo "==> 快照 go.mod / go.sum"
cp go.mod go.mod.otelc-backup
cp go.sum go.sum.otelc-backup
restore_tree() {
  mv go.mod.otelc-backup go.mod
  mv go.sum.otelc-backup go.sum
  rm -rf .otelc-build
}
fail() {
  trap - EXIT
  restore_tree
  exit 1
}
trap fail EXIT

echo "==> 第一段：otelc 插桩构建（生成规则匹配清单，产物丢弃）"
GOPROXY="${GO_PROXY}" "${OTELC_BIN}" go build -o /dev/null "${BUILD_PACKAGE}"

echo "==> 断言插桩目标不越出允许清单并计算摘要"
match_file=".otelc-build/matched.json"
if [ ! -f "${match_file}" ]; then
  echo "matched.json 缺失，无法固化插桩摘要" >&2
  fail
fi
violations=$(python3 - "$match_file" "$ALLOWED_TARGETS" << 'EOF'
import json, re, sys
matched = json.load(open(sys.argv[1]))
allowed = re.compile(sys.argv[2])
seen = set()
for entry in matched:
    for rule in entry.get("file_rules", []):
        seen.add(rule.get("path", ""))
    for rules in entry.get("func_rules", {}).values():
        for rule in rules:
            seen.add(rule.get("path", ""))
unexpected = sorted(target for target in seen if target and not allowed.search(target))
print("\n".join(unexpected))
EOF
)
if [ -n "${violations}" ]; then
  echo "发现越出允许清单的插桩目标：" >&2
  echo "${violations}" >&2
  fail
fi
digest="sha256:$(shasum -a 256 "${match_file}" | cut -d' ' -f1)"
echo "==> 插桩集合摘要 ${digest}"

echo "==> 第二段：带身份 ldflags 的正式构建 ${OUTPUT}"
GOPROXY="${GO_PROXY}" "${OTELC_BIN}" go build -tags otelc \
  -ldflags "-s -w ${LDFLAGS_IDENTITY} -X github.com/fxbin/cairn/internal/buildinfo.InstrumentationDigest=${digest}" \
  -o "${OUTPUT}" "${BUILD_PACKAGE}"

echo "==> 恢复 go.mod / go.sum 并清理生成物"
trap - EXIT
restore_tree

echo "==> plain 构建健全性（go.mod / go.sum 必须零漂移）"
go build -o /dev/null ./cmd/cairn
git diff --exit-code -- go.mod go.sum

echo "==> NDJSON 纯度门（otelc 构建的 --json stdout 只允许 NDJSON）"
purity_workspace="$(mktemp -d)"
if OTEL_SDK_DISABLED=true OTEL_LOG_LEVEL=error "${OUTPUT}" doctor -json -workspace "${purity_workspace}" -state-dir "${purity_workspace}/state" 2>/dev/null \
  | python3 -c '
import json, sys
for line in sys.stdin:
    json.loads(line)
' ; then
  echo "stdout 纯度通过"
else
  echo "NDJSON 纯度门失败：stdout 混入非 NDJSON 行" >&2
  rm -rf "${purity_workspace}"
  exit 1
fi
rm -rf "${purity_workspace}"

echo "==> 完成：${OUTPUT}"
echo "运行时遥测经标准 OTel 环境变量配置；OTEL_SDK_DISABLED=true 显式关闭。"
