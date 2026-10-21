/**
 * 运行流事件订阅。事件仅作通知；事实仍来自 Go/Journal 快照查询。
 */
import { Events } from "@wailsio/runtime";
import type { StreamEvent } from "./types";

/** Wails 事件通道名，与 gui/main.go 常量一致。 */
export const STREAM_EVENT_NAME = "cairn:stream";

type Handler = (event: StreamEvent) => void;

/**
 * 订阅运行流；返回取消函数。
 * 迟到/重复由 Go 层按 runID+seq 过滤，前端仍做会话归属校验。
 */
export function subscribeStream(handler: Handler): () => void {
  const wrapped = (payload: unknown) => {
    const event = extractStreamEvent(payload);
    if (event) {
      handler(event);
    }
  };
  // Wails Events.On 类型在不同版本间有差异，这里以运行时行为为准。
  (Events.On as unknown as (name: string, cb: (payload: unknown) => void) => void)(
    STREAM_EVENT_NAME,
    wrapped,
  );
  return () => {
    void (Events.Off as unknown as (name: string, cb: (payload: unknown) => void) => unknown)(
      STREAM_EVENT_NAME,
      wrapped,
    );
  };
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
