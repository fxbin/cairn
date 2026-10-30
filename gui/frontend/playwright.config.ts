/**
 * Playwright 关键流程用例配置（issue #25 增量 7 前置切片）。
 *
 * 只服务本机验收，不接入 CI：ci.yml 的 frontend job 维持 typecheck/test/build
 * 三件套，CI 无桌面与 wails3 环境。演示桥用例经 vite preview 提供 dist；
 * 真实后端用例在 spec 内自行拉起 server 模式二进制，缺前置条件时如实跳过。
 * 截图与运行产物写入仓库根 .vidt/（Git 忽略），不进入提交。
 */
import { defineConfig } from "@playwright/test";

/** demoPreviewPort 是 vite preview 服务演示桥页面的固定端口。 */
const demoPreviewPort = 4173;

export default defineConfig({
  testDir: "./e2e",
  // 只认 *.e2e.ts：与 vitest 的默认包含规则（*.test.* / *.spec.*）互不重叠，
  // npm test 不会误收 Playwright 用例。
  testMatch: "**/*.e2e.ts",
  timeout: 300_000,
  expect: { timeout: 120_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  outputDir: "../../.vidt/playwright-output",
  use: {
    baseURL: `http://127.0.0.1:${demoPreviewPort}`,
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { browserName: "chromium" } }],
  webServer: {
    command: `npm run build && npm run preview -- --port ${demoPreviewPort} --strictPort`,
    url: `http://127.0.0.1:${demoPreviewPort}`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
  },
});
