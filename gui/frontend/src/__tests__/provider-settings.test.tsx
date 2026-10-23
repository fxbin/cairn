import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const bridge = vi.hoisted(() => ({
 doctor: vi.fn(async () => ({})), mcpStatus: vi.fn(async () => []), listSkills: vi.fn(async () => []),
 listProjectInstructions: vi.fn(async () => ({ documents: [] })), listProviderPresets: vi.fn(async () => []),
 runProviderSetup: vi.fn(), probeProvider: vi.fn(), confirmProviderAdmission: vi.fn(),
}));
vi.mock("../desktop/client", () => ({ desktop: bridge }));
import { SettingsDrawer } from "../components/SettingsDrawer";

const props = () => ({
 open: true, onClose: vi.fn(), workspace: null, workspacePath: "/workspace/a", theme: "dark", onTheme: vi.fn(),
 fontSize: 14, onFontSize: vi.fn(), railWidth: 300, onRailWidth: vi.fn(), readOnly: false, onReadOnly: vi.fn(),
 onError: vi.fn(), onConfigChanged: vi.fn(),
});
const passingProbe = { result: "pass", model: "local-model", evidence_fingerprint: "evidence", config_fingerprint: "config", request_profile_fingerprint: "profile", tool_roundtrip: "ok", usage_streamed: true };

describe("Provider 设置闭环", () => {
 beforeEach(() => { vi.clearAllMocks(); bridge.runProviderSetup.mockReset(); bridge.confirmProviderAdmission.mockResolvedValue({ ok: true }); });
 it("将生成成功与待准入状态显示为通知，保留真实失败的错误通道", async () => {
  const callbacks = props();
  bridge.runProviderSetup.mockResolvedValue({ config_path: "/workspace/a/.agent/config.yaml", probe_ran: false, notes: ["密钥未设置"] });
  render(<SettingsDrawer {...callbacks} />);
  fireEvent.click(screen.getByRole("button", { name: "Provider" }));
  fireEvent.change(screen.getByTestId("setup-base-url"), { target: { value: "http://127.0.0.1:1/v1" } });
  fireEvent.change(screen.getByTestId("setup-model"), { target: { value: "local-model" } });
  fireEvent.click(screen.getByTestId("run-setup"));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("尚未实测"));
  expect(screen.getByRole("status")).toHaveTextContent("密钥未设置");
  expect(callbacks.onError).not.toHaveBeenCalled();
  bridge.runProviderSetup.mockRejectedValue({ body: { code: "configuration", message: "配置存在" } });
  fireEvent.click(screen.getByTestId("run-setup"));
  await waitFor(() => expect(callbacks.onError).toHaveBeenCalledWith({ code: "configuration", message: "配置存在" }));
 });
 it("误填密钥正文时不调用后端且错误不回显正文", async () => {
  const callbacks=props(); render(<SettingsDrawer {...callbacks} />);
  await act(async () => {});
  fireEvent.click(screen.getByRole("button", { name: "Provider" }));
  fireEvent.change(screen.getByTestId("setup-base-url"), { target: { value: "http://127.0.0.1:1/v1" } });
  fireEvent.change(screen.getByTestId("setup-model"), { target: { value: "model" } });
  fireEvent.change(screen.getByTestId("setup-key-env"), { target: { value: "sk-local-sensitive-value" } });
  fireEvent.click(screen.getByTestId("run-setup"));
  expect(bridge.runProviderSetup).not.toHaveBeenCalled();
  expect(callbacks.onError).toHaveBeenCalled();
  expect(JSON.stringify(callbacks.onError.mock.calls)).not.toContain("sk-local-sensitive-value");
 });
 it("探测通过后仍需填写理由并点击独立确认，向后端提交所见绑定", async () => {
  bridge.probeProvider.mockResolvedValue(passingProbe);
  const callbacks = props(); render(<SettingsDrawer {...callbacks} />);
  fireEvent.click(screen.getByRole("button", { name: "Provider" }));
  fireEvent.click(screen.getByTestId("probe-provider"));
  const confirm = await screen.findByTestId("confirm-provider-admission");
  expect(confirm).toBeDisabled(); expect(bridge.confirmProviderAdmission).not.toHaveBeenCalled();
  fireEvent.change(screen.getByTestId("admission-reason"), { target: { value: "复核两轮工具探测" } });
  fireEvent.click(confirm);
  await waitFor(() => expect(bridge.confirmProviderAdmission).toHaveBeenCalledWith({ workspace: "/workspace/a", evidence_fingerprint: "evidence", config_fingerprint: "config", request_profile_fingerprint: "profile", reason: "复核两轮工具探测" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("已记录明确准入决定"));
  expect(callbacks.onError).not.toHaveBeenCalled();
 });
 it("失败或仅文本探测不提供准入确认", async () => {
  bridge.probeProvider.mockResolvedValue({ ...passingProbe, result: "inconclusive", tool_roundtrip: "not_invoked" });
  render(<SettingsDrawer {...props()} />);
  fireEvent.click(screen.getByRole("button", { name: "Provider" }));
  fireEvent.click(screen.getByTestId("probe-provider"));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("inconclusive"));
  expect(screen.queryByTestId("confirm-provider-admission")).not.toBeInTheDocument();
 });
 it("切换工作区后丢弃旧探测成功回执", async () => {
  let finish!: (value: typeof passingProbe) => void;
  bridge.probeProvider.mockReturnValue(new Promise((resolve) => { finish = resolve; }));
  const callbacks=props(); const view=render(<SettingsDrawer {...callbacks} />);
  fireEvent.click(screen.getByRole("button", { name: "Provider" }));
  fireEvent.click(screen.getByTestId("probe-provider"));
  view.rerender(<SettingsDrawer {...callbacks} workspacePath="/workspace/b" />);
  await act(async () => { finish(passingProbe); });
  expect(screen.queryByTestId("provider-admission")).not.toBeInTheDocument();
  expect(screen.queryByTestId("provider-notice")).not.toBeInTheDocument();
  expect(callbacks.onError).not.toHaveBeenCalled();
 });
});
