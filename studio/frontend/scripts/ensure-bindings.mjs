/**
 * Wails v3 前端 TypeScript 绑定按需生成与新鲜度检查脚本。
 *
 * 职责：在本地开发、类型检查、单测与构建前确保 studio/frontend/bindings/ 已就绪且未落后于
 * 仓库 Go 模块定义与 studio/ 源码；支持 --force 强制重生成，以及通过 WAILS_BINDINGS_SKIP=1 跳过。
 *
 * Author: fxbin
 */

import { execFileSync } from "node:child_process";
import { existsSync, readdirSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scriptsDir = path.dirname(fileURLToPath(import.meta.url));
const frontendDir = path.resolve(scriptsDir, "..");
const studioDir = path.resolve(frontendDir, "..");
const repoRootDir = path.resolve(studioDir, "..");

const serviceBindingPath = path.join(
  frontendDir,
  "bindings",
  "github.com",
  "fxbin",
  "cairn",
  "studio",
  "backend",
  "service.ts",
);
const eventCreateBindingPath = path.join(
  frontendDir,
  "bindings",
  "github.com",
  "wailsapp",
  "wails",
  "v3",
  "internal",
  "eventcreate.ts",
);

const skippedStudioDirs = new Set(["frontend", "node_modules", "bin", ".task"]);
const wailsModulePath = "github.com/wailsapp/wails/v3";
const defaultBuildFlags = "-tags server";

/**
 * 读取指定文件的毫秒级最后修改时间（mtimeMs）。
 *
 * 输入：targetPath 为目标文件的绝对路径字符串。
 * 输出：文件存在时返回毫秒级修改时间戳（number），文件不存在时返回 0。
 * 约束：仅当路径不存在时返回 0，其他文件系统权限或 I/O 异常直接抛出以失败关闭。
 */
function readMtimeMsIfPresent(targetPath) {
  if (!existsSync(targetPath)) {
    return 0;
  }
  return statSync(targetPath).mtimeMs;
}

/**
 * 递归扫描 studio/ 及其子目录中的全部 .go 源文件并返回最大 mtimeMs。
 *
 * 输入：dirPath 为待扫描目录的绝对路径字符串。
 * 输出：该目录子树内所有 .go 文件的最大毫秒级修改时间戳（无匹配文件时为 0）。
 * 约束：显式跳过 frontend、node_modules、bin、.task 目录，不跟随符号链接目录以防越界或循环。
 */
function findLatestGoSourceMtimeMs(dirPath) {
  let maxMtimeMs = 0;
  const entries = readdirSync(dirPath, { withFileTypes: true });
  for (const entry of entries) {
    if (entry.isSymbolicLink()) {
      continue;
    }
    const fullPath = path.join(dirPath, entry.name);
    if (entry.isDirectory()) {
      if (skippedStudioDirs.has(entry.name)) {
        continue;
      }
      const childMax = findLatestGoSourceMtimeMs(fullPath);
      if (childMax > maxMtimeMs) {
        maxMtimeMs = childMax;
      }
      continue;
    }
    if (entry.isFile() && entry.name.endsWith(".go")) {
      const fileMtimeMs = statSync(fullPath).mtimeMs;
      if (fileMtimeMs > maxMtimeMs) {
        maxMtimeMs = fileMtimeMs;
      }
    }
  }
  return maxMtimeMs;
}

/**
 * 判断现有前端绑定产物是否齐全且不晚于仓库 Go 依赖及 studio/ 源码修改时间。
 *
 * 输入：无（使用脚本定位的仓库路径常量）。
 * 输出：boolean，true 表示现有绑定可直接复用，false 表示缺失或已过期需重新生成。
 * 约束：要求 service.ts 与 eventcreate.ts 同时存在，且 service.ts 的 mtimeMs >= maxGoMtime。
 */
function areBindingsUpToDate() {
  if (!existsSync(serviceBindingPath) || !existsSync(eventCreateBindingPath)) {
    return false;
  }
  const serviceMtimeMs = statSync(serviceBindingPath).mtimeMs;
  const goModMtimeMs = readMtimeMsIfPresent(path.join(repoRootDir, "go.mod"));
  const goSumMtimeMs = readMtimeMsIfPresent(path.join(repoRootDir, "go.sum"));
  const studioGoMtimeMs = findLatestGoSourceMtimeMs(studioDir);
  const maxGoMtime = Math.max(goModMtimeMs, goSumMtimeMs, studioGoMtimeMs);
  return serviceMtimeMs >= maxGoMtime;
}

/**
 * 从仓库根 go.mod 读取锁定的 github.com/wailsapp/wails/v3 模块精确版本。
 *
 * 输入：无（固定在 studio/ 目录下调用 go list）。
 * 输出：去除首尾空白的版本字符串（例如 "v3.0.0-beta.34"）。
 * 约束：通过 execFileSync 直接传递参数数组，不经过 shell；若未解析到有效版本则抛出错误。
 */
function resolveWailsVersion() {
  const output = execFileSync(
    "go",
    ["list", "-m", "-f", "{{.Version}}", wailsModulePath],
    {
      cwd: studioDir,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "inherit"],
    },
  );
  const version = output.trim();
  if (!version) {
    throw new Error(`无法从 go.mod 解析 ${wailsModulePath} 的锁定版本`);
  }
  return version;
}

/**
 * 调用锁定版本的 wails3 CLI 从 Go 服务签名生成前端 TypeScript 绑定。
 *
 * 输入：无（从环境变量 WAILS_BINDINGS_FLAGS 读取构建标签，默认 "-tags server"）。
 * 输出：无（子进程输出直接继承至当前终端，绑定文件写入 studio/frontend/bindings/）。
 * 约束：使用 @version 语法运行 wails3，绝不修改根 go.mod / go.sum；不经过 shell 拼接参数。
 */
function generateBindings() {
  const version = resolveWailsVersion();
  const buildFlags = process.env.WAILS_BINDINGS_FLAGS ?? defaultBuildFlags;
  execFileSync(
    "go",
    [
      "run",
      `${wailsModulePath}/cmd/wails3@${version}`,
      "generate",
      "bindings",
      "-f",
      buildFlags,
      "-clean=true",
      "-ts",
      "-i",
    ],
    {
      cwd: studioDir,
      stdio: "inherit",
    },
  );
}

/**
 * 脚本主入口：处理 --force 参数、环境变量跳过开关与增量新鲜度短路。
 *
 * 输入：argv 为命令行参数字符串数组（通常为 process.argv.slice(2)）。
 * 输出：无（满足跳过条件时直接退出状态码 0，生成失败时抛出异常使进程非零退出）。
 * 约束：未传 --force 时，WAILS_BINDINGS_SKIP === "1" 或绑定最新均静默 exit(0)。
 */
function main(argv) {
  const force = argv.includes("--force");
  if (!force) {
    if (process.env.WAILS_BINDINGS_SKIP === "1") {
      process.exit(0);
    }
    if (areBindingsUpToDate()) {
      process.exit(0);
    }
  }
  generateBindings();
}

main(process.argv.slice(2));
