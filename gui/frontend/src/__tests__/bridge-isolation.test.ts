import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

type BridgeGlobals = {
  _wails?: unknown;
  wails?: { invokeAsync?: unknown };
  __CAIRN_HOST__?: string;
  __CAIRN_BRIDGE__?: unknown;
  __CAIRN_FORCE_DEMO__?: boolean;
};

function clearBridgeGlobals() {
  const w = window as unknown as BridgeGlobals;
  delete w._wails;
  delete w.wails;
  delete w.__CAIRN_HOST__;
  delete w.__CAIRN_BRIDGE__;
  delete w.__CAIRN_FORCE_DEMO__;
  try {
    window.history.replaceState({}, "", "/");
  } catch {
    // jsdom 可能限制 history
  }
}

describe("N01 真实桥接与 Mock 隔离", () => {
  beforeEach(() => {
    vi.resetModules();
    clearBridgeGlobals();
  });

  afterEach(() => {
    clearBridgeGlobals();
  });

  it("无 Wails 且无显式入口时走 HTTP 传输通道，不宣称原生 Wails", async () => {
    const mod = await import("../desktop/wails-api");
    expect(mod.detectWailsRuntime()).toBe(false);
    expect(mod.detectDemoBridge()).toBe(false);
    // 浏览器客户端：真实绑定经 runtime 的 HTTP 传输通道调用真实后端，
    // 是否可达由调用结果决定，模式本身不作断言。
    expect(mod.bridgeMode()).toBe("http");
    expect(mod.detectHttpTransport()).toBe(true);
  });

  it("宿主标记 __CAIRN_HOST__ 被识别为 wails 模式", async () => {
    (window as unknown as BridgeGlobals).__CAIRN_HOST__ = "wails";
    const mod = await import("../desktop/wails-api");
    expect(mod.detectWailsRuntime()).toBe(true);
    expect(mod.bridgeMode()).toBe("wails");
  });

  it("仅 window._wails 不足以判定真实 Wails", async () => {
    (window as unknown as BridgeGlobals)._wails = { dispatchWailsEvent: () => undefined };
    const mod = await import("../desktop/wails-api");
    expect(mod.detectWailsRuntime()).toBe(false);
    // 仍然不是原生 Wails 模式：只是退到 HTTP 传输通道。
    expect(mod.bridgeMode()).not.toBe("wails");
  });

  it("仅在显式 cairn-demo=1 时启用演示桥", async () => {
    window.history.replaceState({}, "", "/?cairn-demo=1");
    const mod = await import("../desktop/wails-api");
    expect(mod.detectDemoBridge()).toBe(true);
    expect(mod.bridgeMode()).toBe("demo");
    expect(mod.demoBridgeActive()).toBe(true);
  });

  it("无后端时 Health 失败关闭，不返回虚构完成", async () => {
    const mod = await import("../desktop/wails-api");
    await expect(mod.Bindings.Health()).rejects.toBeInstanceOf(mod.BridgeUnavailableError);
  });

  it("事件订阅在 HTTP 传输通道退化为空句柄，不注入假事件源", async () => {
    const events = await import("../desktop/events");
    // HTTP 传输通道没有事件通道，但订阅本身不应拖垮界面。
    const off = events.subscribeStream(() => undefined);
    expect(typeof off).toBe("function");
    off();
  });

  it("显式演示桥订阅返回清理句柄", async () => {
    (window as unknown as BridgeGlobals).__CAIRN_FORCE_DEMO__ = true;
    const bridge = await import("../desktop/browserBridge");
    bridge.bindBrowserBridge();
    const events = await import("../desktop/events");
    const seen: string[] = [];
    const off = events.subscribeStream((e) => seen.push(e.kind));
    expect(typeof off).toBe("function");
    off();
    expect(seen).toHaveLength(0);
  });
});
