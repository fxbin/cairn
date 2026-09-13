#!/usr/bin/env bash
# 构建 Cairn 的可选 otelc flavor。工具链、规则集合、依赖树、构建身份和
# NDJSON 合同全部通过门禁后，才以安全启动器原子发布产物。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OTELC_VERSION="${OTELC_VERSION:-v1.1.0}"
OTELC_BIN="${OTELC_BIN:-$(go env GOPATH)/bin/otelc}"
OUTPUT="${1:-${REPO_ROOT}/cairn-otelc}"
GO_PROXY="${GO_PROXY:-https://goproxy.cn,direct}"
BUILD_PACKAGE="./cmd/cairn"
BUILD_TAGS="otelc"
BUILD_ROOT=""
MATCH_FILE=""
LOCK_DIR="${REPO_ROOT}/.otelc-build.lock"
EXPECTED_INSTRUMENTATION_DIGEST="sha256:fa4acde6011b17f550ad3fe2d113970899d18cd9e8fcbc1fa3b4b3054e724be1"
TEMP_BINARY=""
TEMP_LAUNCHER=""
PURITY_WORKSPACE=""
NEW_PAYLOAD=""
PUBLISHED=false
LOCK_HELD=false

cd "${REPO_ROOT}"

# 构建合同只接受 go.mod/go.sum；禁止 go.work 在仓库外注入未冻结的 replace。
export GOWORK=off
normalize_output() {
  python3 - "$1" <<'PY'
import pathlib
import sys

path = pathlib.Path(sys.argv[1]).expanduser()
print(path.parent.resolve(strict=False) / path.name)
PY
}

validate_output() {
  target=$1
  if [ -L "${target}" ]; then
    echo "拒绝将 otelc 启动器发布到符号链接：${target}" >&2
    return 1
  fi
  if [ -d "${target}" ]; then
    echo "otelc 输出路径不能是目录：${target}" >&2
    return 1
  fi
  if output_relative=$(python3 - "${REPO_ROOT}" "${target}" <<'PY'
import pathlib
import sys

root = pathlib.Path(sys.argv[1]).resolve()
output = pathlib.Path(sys.argv[2])
try:
    print(output.relative_to(root))
except ValueError:
    raise SystemExit(1)
PY
  ); then
    if git ls-files --error-unmatch -- "${output_relative}" >/dev/null 2>&1; then
      echo "拒绝覆盖 Git 已跟踪文件：${output_relative}" >&2
      return 1
    fi
    if ! git check-ignore -q --no-index -- "${output_relative}"; then
      echo "仓库内 otelc 输出必须被 .gitignore 覆盖：${output_relative}" >&2
      return 1
    fi
  fi
}

OUTPUT=$(normalize_output "${OUTPUT}")
validate_output "${OUTPUT}"
mkdir -p "$(dirname -- "${OUTPUT}")"


cleanup() {
  status=$?
  trap - EXIT INT TERM
  [ -z "${BUILD_ROOT}" ] || rm -rf "${BUILD_ROOT}"
  [ -z "${PURITY_WORKSPACE}" ] || rm -rf "${PURITY_WORKSPACE}"
  [ -z "${TEMP_BINARY}" ] || rm -f "${TEMP_BINARY}"
  [ -z "${TEMP_LAUNCHER}" ] || rm -f "${TEMP_LAUNCHER}"
  if [ "${PUBLISHED}" != true ] && [ -n "${NEW_PAYLOAD}" ]; then
    payload_file=$(basename "${NEW_PAYLOAD}")
    if [ ! -f "${OUTPUT}" ] || ! grep -Fq "${payload_file}" "${OUTPUT}" 2>/dev/null; then
      rm -f "${NEW_PAYLOAD}"
    fi
  fi
  if [ "${LOCK_HELD}" = true ] && [ -f "${LOCK_DIR}/pid" ] && [ "$(cat "${LOCK_DIR}/pid" 2>/dev/null || true)" = "$$" ]; then
    rm -rf "${LOCK_DIR}"
  fi
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

acquire_lock() {
  if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
    holder=$(cat "${LOCK_DIR}/pid" 2>/dev/null || true)
    if [ -n "${holder}" ]; then
      echo "otelc 构建锁已存在（PID ${holder}）；确认无构建运行后人工删除 ${LOCK_DIR}" >&2
    else
      echo "otelc 构建锁已存在；确认无构建运行后人工删除 ${LOCK_DIR}" >&2
    fi
    exit 1
  fi
  LOCK_HELD=true
  if ! printf '%s\n' "$$" > "${LOCK_DIR}/pid"; then
    rm -rf "${LOCK_DIR}"
    LOCK_HELD=false
    echo "无法初始化 otelc 构建锁" >&2
    exit 1
  fi
}
acquire_lock

if [ ! -x "${OTELC_BIN}" ]; then
  echo "==> 安装 otelc ${OTELC_VERSION}（GOPROXY=${GO_PROXY}）"
  GOPROXY="${GO_PROXY}" go install "go.opentelemetry.io/otelc/tool/cmd/otelc@${OTELC_VERSION}"
fi
actual_version=$("${OTELC_BIN}" version 2>/dev/null | awk '$1 == "otelc" && $2 == "version" && NF == 3 { print $3 }')
module_version=$(go version -m "${OTELC_BIN}" 2>/dev/null | awk '$1 == "mod" && $2 == "go.opentelemetry.io/otelc" { print $3 }')
if [ "${actual_version}" != "${OTELC_VERSION}" ] || [ "${module_version}" != "${OTELC_VERSION}" ]; then
  echo "otelc 工具链身份不匹配：期望 ${OTELC_VERSION}，命令报告 ${actual_version:-unknown}，模块报告 ${module_version:-unknown}" >&2
  exit 1
fi

source_fingerprint() {
  git ls-files -z --cached --others --exclude-standard 2>/dev/null | python3 -c '
import hashlib
import os
import sys

digest = hashlib.sha256()
for raw_path in sorted(sys.stdin.buffer.read().split(b"\0")):
    if not raw_path:
        continue
    path = os.fsdecode(raw_path)
    digest.update(len(raw_path).to_bytes(8, "big"))
    digest.update(raw_path)
    if os.path.islink(path):
        content = os.fsencode(os.readlink(path))
        kind = b"link"
    elif os.path.isfile(path):
        with open(path, "rb") as stream:
            content = stream.read()
        kind = b"file"
    else:
        raise SystemExit(f"不支持的源码条目类型：{path}")
    digest.update(kind)
    digest.update(len(content).to_bytes(8, "big"))
    digest.update(content)
print(digest.hexdigest())
'
}

build_input_fingerprint() {
  go list -deps -json -tags "${BUILD_TAGS}" "${BUILD_PACKAGE}" | python3 -c '
import hashlib
import json
import os
import sys

decoder = json.JSONDecoder()
text = sys.stdin.read()
offset = 0
files = set()
file_fields = ("GoFiles", "CgoFiles", "CFiles", "CXXFiles", "MFiles", "HFiles", "FFiles", "SFiles", "SwigFiles", "SwigCXXFiles", "SysoFiles", "EmbedFiles")
while offset < len(text):
    while offset < len(text) and text[offset].isspace():
        offset += 1
    if offset >= len(text):
        break
    package, offset = decoder.raw_decode(text, offset)
    directory = package.get("Dir", "")
    for field in file_fields:
        for name in package.get(field) or []:
            files.add(os.path.join(directory, name))
    module = package.get("Module") or {}
    if module.get("GoMod"):
        files.add(module["GoMod"])
digest = hashlib.sha256()
for path in sorted(files):
    encoded = os.fsencode(path)
    digest.update(len(encoded).to_bytes(8, "big"))
    digest.update(encoded)
    if os.path.islink(path):
        content = os.fsencode(os.readlink(path))
        kind = b"link"
    else:
        with open(path, "rb") as stream:
            content = stream.read()
        kind = b"file"
    digest.update(kind)
    digest.update(len(content).to_bytes(8, "big"))
    digest.update(content)
print(digest.hexdigest())
'
}

materialize_build_root() {
  BUILD_ROOT=$(mktemp -d "$(dirname "${REPO_ROOT}")/.cairn-otelc-work.XXXXXX")
  MATCH_FILE="${BUILD_ROOT}/.otelc-build/matched.json"
  python3 - "${REPO_ROOT}" "${BUILD_ROOT}" <<'PY'
import json
import os
import pathlib
import shutil
import subprocess
import sys

source = pathlib.Path(sys.argv[1]).resolve()
target = pathlib.Path(sys.argv[2]).resolve()
module_data = json.loads(subprocess.check_output(["go", "mod", "edit", "-json"], cwd=source, text=True))
for replacement in module_data.get("Replace") or []:
    new = replacement.get("New") or {}
    if new.get("Path") and not new.get("Version"):
        raise SystemExit("otelc 隔离构建不接受本地 replace；请先使用版本化模块依赖")
paths = set()

listed = subprocess.check_output(
    ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
    cwd=source,
)
for raw in listed.split(b"\0"):
    if raw:
        relative = pathlib.Path(os.fsdecode(raw))
        if (source / relative).is_symlink():
            raise SystemExit(f"otelc 隔离构建不接受符号链接输入：{relative}")
        paths.add(relative)

packages = subprocess.check_output(
    ["go", "list", "-deps", "-json", "-tags", "otelc", "./cmd/cairn"],
    cwd=source,
    text=True,
)
decoder = json.JSONDecoder()
offset = 0
while offset < len(packages):
    while offset < len(packages) and packages[offset].isspace():
        offset += 1
    if offset >= len(packages):
        break
    package, offset = decoder.raw_decode(packages, offset)
    directory = pathlib.Path(package.get("Dir", ""))
    if package.get("Goroot"):
        continue
    module = package.get("Module") or {}
    if module and not module.get("Main"):
        continue
    for field in (
        "GoFiles", "CgoFiles", "IgnoredGoFiles", "CFiles", "CXXFiles", "MFiles",
        "HFiles", "FFiles", "SFiles", "SwigFiles", "SwigCXXFiles", "SysoFiles",
        "EmbedFiles",
    ):
        for name in package.get(field) or []:
            candidate = directory / name
            if candidate.is_symlink():
                raise SystemExit(f"otelc 隔离构建不接受符号链接输入：{candidate}")
            try:
                paths.add(candidate.resolve().relative_to(source))
            except ValueError:
                raise SystemExit(f"otelc 构建输入越出仓库边界：{candidate}")
    gomod = module.get("GoMod")
    if gomod:
        candidate = pathlib.Path(gomod)
        if candidate.is_symlink():
            raise SystemExit(f"otelc 隔离构建不接受符号链接模块文件：{candidate}")
        try:
            paths.add(candidate.resolve().relative_to(source))
        except ValueError:
            raise SystemExit(f"otelc 模块文件越出仓库边界：{candidate}")

for relative in sorted(paths, key=os.fspath):
    src = source / relative
    dst = target / relative
    if not src.exists() and not src.is_symlink():
        continue
    if src.is_symlink():
        raise SystemExit(f"otelc 隔离构建不接受符号链接输入：{relative}")
    dst.parent.mkdir(parents=True, exist_ok=True)
    if src.is_file():
        shutil.copy2(src, dst)
PY
}
revision=$(git rev-parse HEAD 2>/dev/null || true)
dirty=unknown
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if [ -z "$(git status --porcelain --untracked-files=normal --ignore-submodules 2>/dev/null)" ]; then dirty=false; else dirty=true; fi
fi
source_digest=$(source_fingerprint)
build_input_digest=$(build_input_fingerprint)

echo "==> 创建只读源的隔离构建镜像"
materialize_build_root
TEMP_BINARY=$(mktemp "$(dirname "${OUTPUT}")/.cairn-otelc-bin.XXXXXX")
TEMP_LAUNCHER=$(mktemp "$(dirname "${OUTPUT}")/.cairn-otelc-launcher.XXXXXX")
rm -f "${TEMP_BINARY}"
canonical_digest() {
  python3 - "${MATCH_FILE}" "${BUILD_ROOT}" <<'PY'
import hashlib
import json
import pathlib
import sys

build_root = pathlib.Path(sys.argv[2]).resolve()
with open(sys.argv[1], encoding="utf-8") as stream:
    matched = json.load(stream)
if not isinstance(matched, list) or not matched:
    raise SystemExit("matched.json 必须是非空数组")

rule_fields = {
    "raw_rules", "func_rules", "struct_rules", "call_rules", "lit_rules",
    "directive_rules", "decl_rules", "file_rules",
}
entry_fields = rule_fields | {"package_name", "module_path", "cgo_file_map"}
allowed_entries = {
    "main": "main",
    "database/sql": "sql",
    "net/http": "http",
    "log": "log",
    "runtime": "runtime",
}
allowed_rule_paths = {
    "go.opentelemetry.io/otelc/instrumentation/go.opentelemetry.io/otel/init",
    "go.opentelemetry.io/otelc/instrumentation/net/http/client",
    "go.opentelemetry.io/otelc/instrumentation/net/http/server",
    "go.opentelemetry.io/otelc/instrumentation/database/sql",
    "go.opentelemetry.io/otelc/instrumentation/log",
    "go.opentelemetry.io/otelc/instrumentation/runtime",
}
allowed_rule_fields = {
    "raw_rules": {"func", "name", "raw", "recv", "target"},
    "func_rules": {"after", "before", "func", "name", "path", "recv", "resolved_path", "target"},
    "struct_rules": {"name", "new_field", "struct", "target"},
    "file_rules": {"file", "name", "path", "resolved_path", "target"},
    "call_rules": set(), "lit_rules": set(), "directive_rules": set(), "decl_rules": set(),
}
canonical_entries = []
rule_count = 0
for entry in matched:
    if not isinstance(entry, dict):
        raise SystemExit("matched.json 条目必须是对象")
    unknown_fields = sorted(set(entry) - entry_fields)
    if unknown_fields:
        raise SystemExit("发现未知清单字段：" + ", ".join(unknown_fields))
    module = entry.get("module_path")
    package = entry.get("package_name")
    if allowed_entries.get(module) != package:
        raise SystemExit(f"发现越出允许清单的插桩包：module={module!r} package={package!r}")
    if entry.get("cgo_file_map") not in (None, {}):
        raise SystemExit("不允许非空 cgo_file_map")
    canonical_entry = {"module_path": module, "package_name": package, "rules": {}}
    for field in sorted(rule_fields):
        value = entry.get(field, {})
        groups = value.values() if isinstance(value, dict) else [value]
        canonical_rules = []
        for group in groups:
            if group is None:
                continue
            if not isinstance(group, list):
                raise SystemExit(f"规则类别 {field} 的结构无效")
            for rule in group:
                if not isinstance(rule, dict):
                    raise SystemExit(f"规则类别 {field} 含非对象条目")
                unknown_rule_fields = sorted(set(rule) - allowed_rule_fields[field])
                if unknown_rule_fields:
                    raise SystemExit(f"规则类别 {field} 含未知字段：" + ", ".join(unknown_rule_fields))
                target = rule.get("target")
                if target != module:
                    raise SystemExit(f"规则目标与条目不一致：module={module!r} target={target!r}")
                path = rule.get("path")
                if path is not None and path not in allowed_rule_paths:
                    raise SystemExit(f"发现越出允许清单的规则来源：{path!r}")
                resolved_path = rule.get("resolved_path")
                if resolved_path is not None:
                    if path is None:
                        raise SystemExit("resolved_path 缺少对应规则 path")
                    relative_rule = path.removeprefix("go.opentelemetry.io/otelc/instrumentation/")
                    expected_path = (build_root / ".otelc-build" / "instrumentation" / relative_rule).resolve()
                    if pathlib.Path(resolved_path).resolve() != expected_path:
                        raise SystemExit(f"规则物理来源越出隔离生成目录：{resolved_path!r}")
                canonical_rules.append({key: value for key, value in rule.items() if key != "resolved_path"})
                rule_count += 1
        canonical_entry["rules"][field] = sorted(canonical_rules, key=lambda item: json.dumps(item, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
    canonical_entries.append(canonical_entry)
if rule_count == 0:
    raise SystemExit("插桩规则集合为空")
canonical_entries.sort(key=lambda item: (item["module_path"], item["package_name"]))
canonical = json.dumps(canonical_entries, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()
print("sha256:" + hashlib.sha256(canonical).hexdigest())
PY
}

run_otelc_build() {
  output=$1
  shift
  rm -f "${MATCH_FILE}"
  (
    cd "${BUILD_ROOT}"
    GOPROXY="${GO_PROXY}" OTELC_WORK_DIR="${BUILD_ROOT}" "${OTELC_BIN}" go build \
      -buildvcs=false -trimpath -tags "${BUILD_TAGS}" "$@" -o "${output}" "${BUILD_PACKAGE}"
  )
  [ -f "${MATCH_FILE}" ] || { echo "本段构建未生成 matched.json" >&2; return 1; }
}

ldflags="-s -w -buildid= -X github.com/fxbin/cairn/internal/buildinfo.Flavor=otelc -X github.com/fxbin/cairn/internal/buildinfo.ToolchainVersion=${OTELC_VERSION} -X github.com/fxbin/cairn/internal/buildinfo.InstrumentationDigest=${EXPECTED_INSTRUMENTATION_DIGEST} -X github.com/fxbin/cairn/internal/buildinfo.Revision=${revision} -X github.com/fxbin/cairn/internal/buildinfo.Dirty=${dirty}"
echo "==> 第一段：生成并校验正式参数对应的插桩清单"
run_otelc_build /dev/null -ldflags "${ldflags}"
digest=$(canonical_digest)
[ "${digest}" = "${EXPECTED_INSTRUMENTATION_DIGEST}" ] || { echo "插桩集合不匹配审核基线：${digest}" >&2; exit 1; }
echo "==> 插桩集合摘要 ${digest}"

echo "==> 第二段：构建临时 otelc 产物并复核最终清单"
run_otelc_build "${TEMP_BINARY}" -ldflags "${ldflags}"
final_digest=$(canonical_digest)
[ "${final_digest}" = "${digest}" ] || { echo "正式构建的插桩集合发生漂移：${digest} -> ${final_digest}" >&2; exit 1; }

rm -rf "${BUILD_ROOT}/.otelc-build"

echo "==> plain 构建健全性（源工作区模块文件始终只读）"
go build -o /dev/null ./cmd/cairn

current_revision=$(git rev-parse HEAD 2>/dev/null || true)
current_source_digest=$(source_fingerprint)
current_build_input_digest=$(build_input_fingerprint)
if [ "${current_revision}" != "${revision}" ] || [ "${current_source_digest}" != "${source_digest}" ] || [ "${current_build_input_digest}" != "${build_input_digest}" ]; then
  echo "构建期间源码发生变化，拒绝发布混合源码产物" >&2
  exit 1
fi

echo "==> NDJSON 纯度与构建身份门"
PURITY_WORKSPACE=$(mktemp -d "${TMPDIR:-/tmp}/cairn-otelc-doctor.XXXXXX")
OTEL_SDK_DISABLED=true OTEL_LOG_LEVEL=error "${TEMP_BINARY}" doctor -json \
  -workspace "${PURITY_WORKSPACE}" -state-dir "${PURITY_WORKSPACE}/state" 2>/dev/null \
  | python3 -c '
import json
import sys
lines = [line for line in sys.stdin if line.strip()]
if not lines:
    raise SystemExit("doctor 未输出 NDJSON")
report = [json.loads(line) for line in lines][-1]
if report.get("build", {}).get("flavor") != "otelc" or not report.get("build_valid", False):
    raise SystemExit("doctor 未报告合法 otelc 构建身份")
'

binary_digest=$(python3 - "${TEMP_BINARY}" <<'PY'
import hashlib
import sys

digest = hashlib.sha256()
with open(sys.argv[1], "rb") as stream:
    for block in iter(lambda: stream.read(1024 * 1024), b""):
        digest.update(block)
print(digest.hexdigest())
PY
)
payload_name=".cairn-otelc-${binary_digest}.bin"
payload_path="$(dirname "${OUTPUT}")/${payload_name}"
cat > "${TEMP_LAUNCHER}" <<LAUNCHER
#!/usr/bin/env bash
set -euo pipefail
SELF_DIR="\$(cd "\$(dirname "\${BASH_SOURCE[0]}")" && pwd)"
if [ "\${CAIRN_OTEL_PROCESS_FILTER_CONFIRMED:-}" != "true" ]; then
  export OTEL_SDK_DISABLED=true
fi
exec "\${SELF_DIR}/${payload_name}" "\$@"
LAUNCHER
chmod 0755 "${TEMP_LAUNCHER}"
chmod 0700 "${TEMP_BINARY}"
payload_created=$(python3 - "${TEMP_BINARY}" "${payload_path}" <<'PY'
import os
import stat
import sys

source, target = sys.argv[1:]
try:
    os.link(source, target, follow_symlinks=False)
    print("true")
except FileExistsError:
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(target, flags)
    try:
        metadata = os.fstat(descriptor)
        if not stat.S_ISREG(metadata.st_mode) or stat.S_IMODE(metadata.st_mode) != 0o700:
            raise SystemExit("拒绝复用非 0700 普通文件 payload")
        with open(source, "rb") as expected, os.fdopen(descriptor, "rb", closefd=False) as actual:
            while True:
                left = expected.read(1024 * 1024)
                right = actual.read(1024 * 1024)
                if left != right:
                    raise SystemExit("payload 摘要碰撞")
                if not left:
                    break
    finally:
        os.close(descriptor)
    print("false")
PY
)
if [ "${payload_created}" = true ]; then
  NEW_PAYLOAD="${payload_path}"
fi
rm -f "${TEMP_BINARY}"
TEMP_BINARY=""
validate_output "${OUTPUT}"
python3 - "${TEMP_LAUNCHER}" "${OUTPUT}" <<'PY'
import os
import stat
import sys

source, target = sys.argv[1:]
try:
    mode = os.lstat(target).st_mode
except FileNotFoundError:
    pass
else:
    if stat.S_ISLNK(mode) or stat.S_ISDIR(mode):
        raise SystemExit("otelc 输出在发布前变成符号链接或目录，拒绝替换")
os.replace(source, target)
PY
TEMP_LAUNCHER=""
PUBLISHED=true
NEW_PAYLOAD=""

echo "==> 完成：${OUTPUT}"
echo "遥测默认关闭；仅在收集器已过滤 process.* 后设置 CAIRN_OTEL_PROCESS_FILTER_CONFIRMED=true 启用。"
