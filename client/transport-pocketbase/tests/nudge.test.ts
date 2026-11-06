import { describe, expect, it } from "vitest";
import { sseNudgeSource } from "../src/nudge";

function streamResponse(chunks: string[]): Response {
  const enc = new TextEncoder();
  let i = 0;
  return new Response(
    new ReadableStream({
      pull(controller) {
        if (i < chunks.length) controller.enqueue(enc.encode(chunks[i++]));
        else controller.close();
      },
    }),
    { status: 200, headers: { "Content-Type": "text/event-stream" } },
  );
}

describe("sseNudgeSource (audit L-4)", () => {
  it("fires on a line-start data field and ignores other occurrences", async () => {
    let nudges = 0;
    const calls: Array<{ url: string; init: RequestInit }> = [];
    const fetchImpl = (async (url: string | URL | Request, init?: RequestInit) => {
      calls.push({ url: String(url), init: init ?? {} });
      if (calls.length === 1) {
        return streamResponse([
          ": comment mentioning data: only in a comment\n\n",
          "event: nudge\nid: 1\n",
          "data: {}\n\n",
        ]);
      }
      return streamResponse([]);
    }) as unknown as typeof fetch;

    const src = sseNudgeSource({
      url: "https://pb.test/api/realtime",
      fetch: fetchImpl,
      token: { getToken: () => "tok-1" },
    });
    const unsubscribe = src.subscribe(() => nudges++);

    await new Promise((resolve) => setTimeout(resolve, 50));
    unsubscribe();

    expect(nudges).toBe(1);
    // The token travels in a header, never in the URL (no proxy-log leak).
    expect(calls[0].url).toBe("https://pb.test/api/realtime");
    expect(calls[0].init.headers).toMatchObject({ Authorization: "Bearer tok-1" });
  });

  it("reconnects after the stream ends instead of going silent", async () => {
    let fetches = 0;
    const fetchImpl = (async () => {
      fetches++;
      return streamResponse(fetches === 1 ? ["data: {}\n\n"] : []);
    }) as unknown as typeof fetch;

    const src = sseNudgeSource({ url: "https://pb.test/api/realtime", fetch: fetchImpl });
    const unsubscribe = src.subscribe(() => {});
    // First stream closes, the source waits ~1s before reconnecting.
    await new Promise((resolve) => setTimeout(resolve, 1200));
    unsubscribe();

    expect(fetches).toBeGreaterThanOrEqual(2);
  });
});
