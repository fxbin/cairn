/**
 * HTTP 传输通道事件订阅的守卫用例。
 *
 * 事实依据（wails v3.0.0-beta.34 源码核对）：server 构建在 run() 里创建
 * WebSocketBroadcaster 并注册为 wails 事件监听器；HTTP mux 挂 /wails/custom.js
 * 与 /wails/events，custom.js 连接 WebSocket 并把消息交给
 * window._wails.dispatchWailsEvent；@wailsio/runtime 在模块装载时自动拉取
 * /wails/custom.js（sideEffects 保底），Events.On 的监听由 dispatchWailsEvent
 * 按事件名分发。因此浏览器客户端与原生 WebView 共用同一条 Events 通道。
 *
 * 这些用例锁定：HTTP 模式订阅必须走该通道并能收到后端事件（而非空句柄），
 * 以及演示桥不受影响。
 */
import { beforeEach, describe, expect, it } from "vitest";

import { STREAM_EVENT_NAME, subscribeStream } from "../desktop/events";

type WailsRuntimeGlobal = {
  _wails?: { dispatchWailsEvent?: (event: { name: string; data?: unknown }) => void };
};

function runtimeDispatch(): (event: { name: string; data?: unknown }) => void {
  const win = window as unknown as WailsRuntimeGlobal;
  const dispatch = win._wails?.dispatchWailsEvent;
  if (typeof dispatch !== "function") {
    throw new Error("@wailsio/runtime 未在当前环境注册 dispatchWailsEvent");
  }
  return dispatch;
}

// jsdom 下无宿主标记与演示入口、真实绑定存在，bridgeMode 为 http。
function clearBridgeGlobals() {
  delete (window as unknown as { __CAIRN_HOST__?: string }).__CAIRN_HOST__;
  delete (window as unknown as { __CAIRN_BRIDGE__?: unknown }).__CAIRN_BRIDGE__;
  delete (window as unknown as { __CAIRN_FORCE_DEMO__?: boolean }).__CAIRN_FORCE_DEMO__;
}

describe("HTTP 传输通道的事件订阅", () => {
  beforeEach(() => {
    clearBridgeGlobals();
  });

  it("HTTP 模式订阅走 Wails Events 通道并接收后端事件", () => {
    const seen: string[] = [];
    const off = subscribeStream((event) => seen.push(event.kind));
    expect(typeof off).toBe("function");
    // 模拟 server 模式 custom.js 的投递路径：WebSocket 消息 → dispatchWailsEvent。
    runtimeDispatch()({ name: STREAM_EVENT_NAME, data: { kind: "run.event", run_id: "run-1" } });
    expect(seen).toEqual(["run.event"]);
    off();
    runtimeDispatch()({ name: STREAM_EVENT_NAME, data: { kind: "run.finished", run_id: "run-1" } });
    expect(seen).toEqual(["run.event"]);
  });

  it("事件名不匹配或负载非法时不派发给处理器", () => {
    const seen: string[] = [];
    const off = subscribeStream((event) => seen.push(event.kind));
    runtimeDispatch()({ name: "cairn:other", data: { kind: "run.event" } });
    runtimeDispatch()({ name: STREAM_EVENT_NAME, data: "not-an-object" });
    expect(seen).toHaveLength(0);
    off();
  });

  it("演示桥优先于 Events 通道，订阅走显式钩子", () => {
    const handlers: Array<(event: { kind: string }) => void> = [];
    (window as unknown as {
      __CAIRN_BRIDGE__?: unknown;
      __CAIRN_SUBSCRIBE__?: (fn: (event: { kind: string }) => void) => () => void;
    }).__CAIRN_BRIDGE__ = {};
    (window as unknown as {
      __CAIRN_SUBSCRIBE__?: (fn: (event: { kind: string }) => void) => () => void;
    }).__CAIRN_SUBSCRIBE__ = (fn) => {
      handlers.push(fn);
      return () => undefined;
    };
    const seen: string[] = [];
    const off = subscribeStream((event) => seen.push(event.kind));
    // Wails 通道的派发不应到达演示桥的订阅者。
    runtimeDispatch()({ name: STREAM_EVENT_NAME, data: { kind: "run.event" } });
    expect(seen).toHaveLength(0);
    for (const handler of handlers) handler({ kind: "run.waiting_approval" });
    expect(seen).toEqual(["run.waiting_approval"]);
    off();
  });
});
