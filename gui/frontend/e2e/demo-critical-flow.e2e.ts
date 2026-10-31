/**
 * 演示桥关键流程冒烟（issue #25 增量 7）。
 *
 * 在 ?cairn-demo=1 的显式演示入口下走完「新建任务 → 审批卡 → 批准 → 完成 →
 * 证据展示」的 UI 关键路径。演示桥替身复刻真实闭环的停点语义（等待审批、
 * 批准后完成），因此本用例验证的是界面流程与交互接线，不宣称真实 Provider
 * 验收——那由 server-real-backend.e2e.ts 与桌面真机签收（issue #20）承担。
 */
import { expect, test } from "@playwright/test";

/** evidenceDir 是留证截图目录（仓库根 .vidt/，Git 忽略）。 */
const evidenceDir = "../../.vidt/evidence/issue25-demo";

test("演示桥：新建任务→审批→完成→证据展示", async ({ page }) => {
  await page.goto("/?cairn-demo=1");

  // 演示身份必须显式可见，避免把演示数据误当真实 Journal 运行。
  await expect(page.getByTestId("demo-banner")).toBeVisible();

  // 新建任务
  await page.getByTestId("new-session").click();
  await page.getByTestId("composer-input").fill("演示：写一份周报并保存");
  await page.getByTestId("composer-send").click();

  // 任务发出后进入运行态：停止按钮出现，助手响应开始流式呈现
  await expect(page.getByTestId("composer-stop")).toBeVisible();
  await expect(page.getByTestId("assistant-body").first()).toBeVisible();

  // 审批停点：演示桥在写操作前请求人工批准
  const approval = page.getByTestId("approval-card");
  await expect(approval).toBeVisible();
  await page.screenshot({ path: `${evidenceDir}/02-approval-pending.png`, fullPage: true });

  // 批准并继续
  await page.getByTestId("approval-allow").click();
  await expect(approval.getByText("已允许并继续")).toBeVisible();

  // 完成停点：运行收口，完成状态进入时间线
  await expect(page.getByTestId("composer-stop")).toBeHidden();
  await expect(page.getByText("任务完成").first()).toBeVisible();

  // 证据展示：检查器详情页签呈现运行结果分层与用量证据
  await page.getByRole("button", { name: "详情" }).click();
  const details = page.getByTestId("rail-body");
  await expect(details.getByTestId("result-layers")).toBeVisible();
  await expect(details.getByText(/turns\s+3/).first()).toBeVisible();
  await page.screenshot({ path: `${evidenceDir}/05-evidence-details.png`, fullPage: true });
});
