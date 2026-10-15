/**
 * Default nudge source (protocol §10): reads a Server-Sent Events stream and
 * fires the callback on any event. The payload is ignored — a nudge is only a
 * "pull now" hint, so `subscribe` is swappable and correctness never depends on
 * delivery.
 *
 * The reader is generic; PocketBase's realtime endpoint additionally needs a
 * subscribe POST after connect, which a host can add by injecting its own
 * `NudgeSource` into the transport.
 */
import { TransportError } from "@pb-offline-sync/core";
import type { NudgeSource, SseNudgeOptions } from "./types";

export function sseNudgeSource(opts: SseNudgeOptions): NudgeSource {
  return {
    subscribe(onNudge: () => void): () => void {
      const controller = new AbortController();
      void (async () => {
        const headers: Record<string, string> = { Accept: "text/event-stream" };
        const token = await opts.token?.getToken?.();
        if (token) headers.Authorization = token.startsWith("Bearer ") ? token : `Bearer ${token}`;
        const doFetch = opts.fetch ?? fetch;
        const res = await doFetch(opts.url, { headers, signal: controller.signal });
        if (!res.ok || !res.body) {
          throw new TransportError(`nudge stream failed (${res.status})`, { status: res.status });
        }
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buf = "";
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += decoder.decode(value, { stream: true });
          let end: number;
          while ((end = buf.indexOf("\n\n")) >= 0) {
            const block = buf.slice(0, end);
            buf = buf.slice(end + 2);
            // §10: any event is a hint only; ignore the payload.
            if (block.includes("data:")) onNudge();
          }
        }
      })().catch(() => {
        // Best-effort: a failed nudge stream must never break syncing (§10).
      });
      return () => controller.abort();
    },
  };
}
