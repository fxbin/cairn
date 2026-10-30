/**
 * server 模式真实后端闭环（issue #25 增量 7）。
 *
 * 以 -tags server 构建的桌面后端在 127.0.0.1:8099 暴露静态资源与
 * /wails/runtime，浏览器客户端经 HTTP 传输通道驱动真实 App/Journal：
 * 新建任务 → 写操作触发审批停点（HTTP 模式下由投影轮询感知）→ 批准 →
 * 运行完成 → 检查器展示完成证据。
 *
 * 前置条件缺失（二进制未构建、密钥未注入、显式跳过）时如实跳过并说明
 * 原因，不写 PASS；真实模型调用依赖网络与配额，失败即失败，不重试伪装。
 * 密钥只经子进程环境注入一次性临时环境（HOME 重定向，prefs 与 Journal 都
 * 不落真实用户目录），不写入文件、日志或截图。
 */
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { expect, test } from "@playwright/test";

/** serverPort 是 wails server 模式的固定监听端口。 */
const serverPort = 8099;

/** serverBin 是 -tags server 构建产物路径（gui/bin/cairn-gui-server）。 */
const serverBin = path.resolve(process.cwd(), "../bin/cairn-gui-server");

/**
 * 真实 Provider 参数经环境覆盖：默认 DeepSeek，密钥失效时可用
 * CAIRN_E2E_BASE_URL / CAIRN_E2E_MODEL / CAIRN_E2E_KEY_ENV 换用其他
 * OpenAI-compatible 真实端点；不提供任何模拟替身。
 */
const providerBaseURL = process.env.CAIRN_E2E_BASE_URL ?? "https://api.deepseek.com";
const providerModel = process.env.CAIRN_E2E_MODEL ?? "deepseek-v4-flash";
const apiKeyEnv = process.env.CAIRN_E2E_KEY_ENV ?? "DEEPSEEK_API_KEY";

/** evidenceDir 是留证截图目录（仓库根 .vidt/，Git 忽略）。 */
const evidenceDir = "../../.vidt/evidence/issue25-server";

const apiKey = process.env[apiKeyEnv] ?? "";
const prerequisitesReady = existsSync(serverBin) && apiKey !== "" && process.env.CAIRN_E2E_SKIP_REAL !== "1";

let server: ChildProcess | null = null;
let tempHome = "";
let tempWorkspace = "";

/** waitForServer 轮询静态资源就绪，超时即抛错（不静默继续）。 */
async function waitForServer(timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const response = await fetch(`http://127.0.0.1:${serverPort}/`);
      if (response.ok) return;
    } catch {
      // 尚未监听，继续等待
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error(`server 模式后端在 ${timeoutMs}ms 内未就绪`);
}

test.beforeAll(async () => {
  if (!prerequisitesReady) {
    test.skip(
      true,
      existsSync(serverBin)
        ? `缺少 ${apiKeyEnv} 或设置 CAIRN_E2E_SKIP_REAL=1，真实闭环记 not_run`
        : `未找到 ${serverBin}，先执行 go build -tags server -o gui/bin/cairn-gui-server ./gui，真实闭环记 not_run`,
    );
    return;
  }

  // 一次性环境：HOME 重定向隔离 prefs 与用户根；工作区只含最小配置。
  tempHome = await mkdtemp(path.join(tmpdir(), "cairn-e2e-home-"));
  tempWorkspace = await mkdtemp(path.join(tmpdir(), "cairn-e2e-ws-"));
  await mkdir(path.join(tempWorkspace, ".agent"), { recursive: true });
  await writeFile(
    path.join(tempWorkspace, ".agent", "config.yaml"),
    [
      "version: 2",
      "provider:",
      "  admission: optional",
      `  base_url: ${providerBaseURL}`,
      `  model: ${providerModel}`,
      `  api_key_env: ${apiKeyEnv}`,
      "  request_timeout: 120s",
      "  capability:",
      "    tool_calls: true",
      "    streaming_usage: true",
      "    stop_reasons: [stop, length, tool_calls]",
      "",
    ].join("\n"),
    { mode: 0o600 },
  );
  // 预置最近项目，让浏览器端首屏直接装载该工作区（原生目录选择在 server 模式不可用）。
  await mkdir(path.join(tempHome, ".cairn"), { recursive: true });
  await writeFile(
    path.join(tempHome, ".cairn", "gui-preferences.json"),
    JSON.stringify({
      recent_projects: [
        {
          path: tempWorkspace,
          name: "e2e-ws",
          last_used: new Date().toISOString(),
          pinned: false,
          archived: false,
        },
      ],
      session_meta: [],
    }),
    { mode: 0o600 },
  );

  server = spawn(serverBin, [], {
    cwd: path.dirname(serverBin),
    env: {
      ...process.env,
      HOME: tempHome,
      WAILS_SERVER_HOST: "127.0.0.1",
      WAILS_SERVER_PORT: String(serverPort),
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  server.stdout?.on("data", () => undefined);
  server.stderr?.on("data", () => undefined);
  await waitForServer(30_000);
  if (server.exitCode !== null) {
    throw new Error(`server 模式后端提前退出（exit=${server.exitCode}），检查端口 ${serverPort} 是否被占用`);
  }
});

test.afterAll(async () => {
  if (server && server.exitCode === null) {
    server.kill("SIGTERM");
    await new Promise<void>((resolve) => {
      const timer = setTimeout(() => {
        server?.kill("SIGKILL");
        resolve();
      }, 5000);
      server?.on("exit", () => {
        clearTimeout(timer);
        resolve();
      });
    });
  }
  for (const dir of [tempHome, tempWorkspace]) {
    if (dir) {
      await rm(dir, { recursive: true, force: true });
    }
  }
});

test("真实后端：新建任务→审批停点→批准→完成→证据", async ({ page }) => {
  test.setTimeout(300_000);
  await page.goto(`http://127.0.0.1:${serverPort}/`);

  // 浏览器客户端经 HTTP 传输通道连上真实后端：无演示身份、无失败关闭横幅。
  await expect(page.getByTestId("demo-banner")).toBeHidden();
  await expect(page.getByTestId("bridge-unavailable-banner")).toBeHidden();
  await expect(page.getByTestId("workspace-chip")).toContainText("cairn-e2e-ws");  await page.screenshot({ path: `${evidenceDir}/01-workspace-ready.png`, fullPage: true });

  // 新建任务：写文件必然经过 提议→Policy→审批 的授权链
  await page.getByTestId("new-session").click();
  await page.getByTestId("composer-input").fill("在工作区创建 note.txt，内容为一行 hello cairn，写完即结束，不要执行其他操作。");
  await page.getByTestId("composer-send").click();
  await expect(page.getByTestId("composer-stop")).toBeVisible();

  // 审批停点：HTTP 模式没有事件推送，审批卡只能来自投影轮询感知的 Journal 事实
  const approval = page.getByTestId("approval-card");
  await expect(approval).toBeVisible();
  await page.screenshot({ path: `${evidenceDir}/02-approval-pending.png`, fullPage: true });

  await page.getByTestId("approval-allow").click();
  await expect(approval.getByText("已允许并继续")).toBeVisible();

  // 完成停点：投影轮询感知终态，运行指示收口
  await expect(page.getByTestId("composer-stop")).toBeHidden({ timeout: 240_000 });

  // 证据展示：检查器详情页签呈现真实 Journal 的结果分层与用量
  await page.getByRole("button", { name: "详情" }).click();
  const details = page.getByTestId("rail-body");
  await expect(details.getByTestId("result-layers")).toBeVisible();
  await page.screenshot({ path: `${evidenceDir}/05-evidence-details.png`, fullPage: true });

  // 留证：工作区产物存在即真实副作用落盘（只断言存在，不比对内容）
  const notePath = path.join(tempWorkspace, "note.txt");
  expect(existsSync(notePath), "审批通过的工作区写操作应落盘").toBe(true);
});
