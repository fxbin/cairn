import { beforeEach, describe, expect, it, vi } from "vitest";

// 真实绑定替身：验证浏览器客户端是否真的走了真实绑定（而不是演示桥），
// 以及传输层不可达时是否失败关闭。
const rawMock = vi.hoisted(() => ({
  Health: vi.fn(async () => ({ ok: true, app_name: "Cairn", version: "9.9.9", go_version: "go" })),
  ListRecentProjects: vi.fn(async () => []),
}));

vi.mock("../../bindings/github.com/fxbin/cairn/internal/desktop/service.js", () => rawMock);

type BridgeGlobals = {
  __CAIRN_HOST__?: string;
  __CAIRN_BRIDGE__?: unknown;
  __CAIRN_FORCE_DEMO__?: boolean;
};

function clearBridgeGlobals() {
  const w = window as unknown as BridgeGlobals;
  delete w.__CAIRN_HOST__;
  delete w.__CAIRN_BRIDGE__;
  delete w.__CAIRN_FORCE_DEMO__;
  try {
    window.history.replaceState({}, "", "/");
  } catch {
    // jsdom 可能限制 history
  }
}

describe("浏览器客户端经 HTTP 传输通道访问真实后端", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.clearAllMocks();
    clearBridgeGlobals();
  });

  it("调用真实绑定而不是演示桥", async () => {
    const mod = await import("../desktop/wails-api");
    expect(mod.bridgeMode()).toBe("http");
    const health = await mod.Bindings.Health();
    expect(health.version).toBe("9.9.9");
    expect(rawMock.Health).toHaveBeenCalled();
  });

  it("显式演示入口优先于 HTTP 传输通道", async () => {
    window.history.replaceState({}, "", "/?cairn-demo=1");
    const mod = await import("../desktop/wails-api");
    expect(mod.bridgeMode()).toBe("demo");
    await mod.Bindings.Health();
    // 演示入口是用户显式要求的，真实绑定不应被顶掉。
    expect(rawMock.Health).not.toHaveBeenCalled();
  });

  it("传输层不可达时失败关闭并说明是桌面后端不可达", async () => {
    // 浏览器报「Failed to fetch」，Node 报「fetch failed」且带 cause：
    // 两种都要判成传输层失败，不能只看 cause。
    rawMock.Health.mockRejectedValueOnce(new TypeError("Failed to fetch"));
    const mod = await import("../desktop/wails-api");
    const failure = mod.Bindings.Health();
    await expect(failure).rejects.toBeInstanceOf(mod.BridgeUnavailableError);
    await expect(failure).rejects.toThrow(/桌面后端不可用/);

    const nodeStyle = new TypeError("fetch failed");
    (nodeStyle as TypeError & { cause?: unknown }).cause = new Error("ECONNREFUSED");
    rawMock.Health.mockRejectedValueOnce(nodeStyle);
    await expect(mod.Bindings.Health()).rejects.toBeInstanceOf(mod.BridgeUnavailableError);
  });

  it("响应不是协议格式时同样按桌面后端不可达处理", async () => {
    rawMock.Health.mockRejectedValueOnce(new Error("<!DOCTYPE html><html></html>"));
    const mod = await import("../desktop/wails-api");
    await expect(mod.Bindings.Health()).rejects.toBeInstanceOf(mod.BridgeUnavailableError);
  });

  it("后端自己返回的错误不被改写成不可达", async () => {
    const backendError = new Error("workspace not found");
    (backendError as Error & { cause?: unknown }).cause = null;
    rawMock.Health.mockRejectedValueOnce(backendError);
    const mod = await import("../desktop/wails-api");
    await expect(mod.Bindings.Health()).rejects.toBe(backendError);
  });
});
