/**
 * Default nudge source (protocol §10): reads a Server-Sent Events stream and
 * fires the callback on any event. The payload is ignored — a nudge is only a
 * "pull now" hint, so `subscribe` is swappable and correctness never depends on
 * delivery.
 *
 * The reader is generic; PocketBase's realtime endpoint additionally needs a
 * subscribe POST after connect, which a host can add by injecting its own
 * `NudgeSource` into the transport.
 *
 * Hardening (audit L-4): the read buffer is bounded so a stream that never
 * sends the `\n\n` delimiter cannot exhaust memory, the connection reconnects
 * with capped exponential backoff instead of silently dying, and only a
 * line-start `data:` field counts as an event.
 */
import { TransportError } from "@pb-offline-sync/core";
import type { NudgeSource, SseNudgeOptions } from "./types";

/** Above this many buffered characters the stream is treated as broken. */
const MAX_NUDGE_BUFFER = 256 * 1024;
const INITIAL_BACKOFF_MS = 1000;
const MAX_BACKOFF_MS = 30_000;

export function sseNudgeSource(opts: SseNudgeOptions): NudgeSource {
  return {
    subscribe(onNudge: () => void): () => void {
      const controller = new AbortController();
      let stopped = false;

      const readStream = async (): Promise<void> => {
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
          if (done) return;
          buf += decoder.decode(value, { stream: true });
          let end: number;
          while ((end = buf.indexOf("\n\n")) >= 0) {
            const block = buf.slice(0, end);
            buf = buf.slice(end + 2);
            // §10: any event is a hint only; ignore the payload. Match the
            // field at line start — a comment/other field containing "data:"
            // is not an event.
            if (/(^|\n)data:/.test(block)) onNudge();
          }
          if (buf.length > MAX_NUDGE_BUFFER) {
            throw new TransportError("nudge buffer overflow");
          }
        }
      };

      const sleep = (ms: number): Promise<void> =>
        new Promise((resolve) => {
          const timer = setTimeout(resolve, ms);
          controller.signal.addEventListener(
            "abort",
            () => {
              clearTimeout(timer);
              resolve();
            },
            { once: true },
          );
        });

      void (async () => {
        let backoff = INITIAL_BACKOFF_MS;
        while (!stopped) {
          try {
            await readStream();
            backoff = INITIAL_BACKOFF_MS; // a clean end is a fresh start
          } catch {
            // Best-effort: a failed nudge stream must never break syncing (§10).
          }
          if (stopped) return;
          await sleep(backoff);
          backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
        }
      })();

      return () => {
        stopped = true;
        controller.abort();
      };
    },
  };
}
