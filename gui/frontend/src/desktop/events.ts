/**
 * 运行流事件订阅。桌面用 Wails Events，浏览器验收用 bridge 回调。
 */
import type { StreamEvent } from "./types";

export const STREAM_EVENT_NAME = "cairn:stream";

type Handler = (event: StreamEvent) => void;

export function subscribeStream(handler: Handler): () => void {
  const win = window as unknown as {
    runtime?: { Events?: { On: (name: string, cb: (payload: unknown) => void) => void; Off: (name: string, cb: (payload: unknown) => void) => unknown } };
    __CAIRN_SUBSCRIBE__?: (fn: Handler) => void;
  };

  if (win.runtime?.Events) {
    const wrapped = (payload: unknown) => {
      const event = extractStreamEvent(payload);
      if (event) {
        handler(event);
      }
    };
    win.runtime.Events.On(STREAM_EVENT_NAME, wrapped);
    return () => {
      void win.runtime!.Events!.Off(STREAM_EVENT_NAME, wrapped);
    };
  }

  if (win.__CAIRN_SUBSCRIBE__) {
    win.__CAIRN_SUBSCRIBE__(handler);
    return () => undefined;
  }

  // 延迟：bridge 可能稍后注入
  const timer = window.setInterval(() => {
    if (win.__CAIRN_SUBSCRIBE__) {
      win.__CAIRN_SUBSCRIBE__(handler);
      window.clearInterval(timer);
    }
  }, 50);
  return () => window.clearInterval(timer);
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
