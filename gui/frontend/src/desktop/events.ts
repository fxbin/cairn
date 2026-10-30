/**
 * 运行流事件订阅。
 * 桌面使用 @wailsio/runtime 的模块化 Events API；演示桥使用显式订阅钩子。
 */
import { Events as WailsEvents } from "@wailsio/runtime";
import type { StreamEvent } from "./types";
import { detectDemoBridge, detectHttpTransport, detectWailsRuntime } from "./wails-api";

export const STREAM_EVENT_NAME = "cairn:stream";

type Handler = (event: StreamEvent) => void;

interface DemoSubscribeWindow {
  __CAIRN_SUBSCRIBE__?: (fn: Handler) => () => void | void;
}

/**
 * subscribeStream 订阅运行流事件，返回可重复调用的取消订阅函数。
 * 桌面路径使用 Wails Events.On 的清理句柄；无后端时失败关闭。
 */
export function subscribeStream(handler: Handler): () => void {
  if (detectWailsRuntime()) {
    return WailsEvents.On(STREAM_EVENT_NAME, (ev: { data?: unknown }) => {
      const event = extractStreamEvent(ev);
      if (event) {
        handler(event);
      }
    });
  }

  const win = window as unknown as DemoSubscribeWindow;
  if (detectDemoBridge() && typeof win.__CAIRN_SUBSCRIBE__ === "function") {
    const result = win.__CAIRN_SUBSCRIBE__(handler);
    if (typeof result === "function") {
      return result;
    }
    return () => undefined;
  }

  if (detectHttpTransport()) {
    // HTTP 传输通道没有事件通道：wails3 的 HTTP 传输只挂 /wails/runtime，
    // 事件由宿主注入的派发函数送达，浏览器侧收不到。这里退化为按 Journal
    // 投影刷新，不注入假事件源，也不让订阅失败拖垮界面。
    return () => undefined;
  }

  // 无 Wails 且无演示桥：不自动注入假事件源。
  throw new Error("事件流不可用：需要 Wails 运行时或显式演示入口");
}

function extractStreamEvent(payload: unknown): StreamEvent | null {
  if (!payload || typeof payload !== "object") {
    return null;
  }
  const candidate = payload as { data?: unknown } | StreamEvent;
  const raw =
    candidate && typeof candidate === "object" && "data" in candidate
      ? (candidate as { data?: unknown }).data
      : candidate;
  if (!raw || typeof raw !== "object") {
    return null;
  }
  const event = raw as StreamEvent;
  if (typeof event.kind !== "string") {
    return null;
  }
  return event;
}
