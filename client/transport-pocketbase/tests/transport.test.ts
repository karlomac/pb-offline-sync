import { describe, expect, it } from "vitest";
import { TransportError } from "@pb-offline-sync/core";
import type { Mutation } from "@pb-offline-sync/core";
import { PocketBaseTransport, PROTOCOL_HEADER } from "../src/transport";
import type { NudgeSource } from "../src/types";

interface Call {
  url: string;
  init: RequestInit;
}

/** Fetch stub that records calls and replies from a queue (or throws). */
function stubFetch(replies: Array<Response | (() => never)>): { calls: Call[]; fetch: typeof fetch } {
  const calls: Call[] = [];
  const fn = (async (input: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(input), init: init ?? {} });
    const next = replies.shift();
    if (!next) throw new Error("no more stub responses");
    if (typeof next === "function") next();
    return next;
  }) as unknown as typeof fetch;
  return { calls, fetch: fn };
}

function json(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

const token = { getToken: () => "tok-123" };
const mutation: Mutation = {
  id: "m1",
  collection: "notes",
  recordId: "aaaaaaaaaaaaaaa",
  op: "update",
  baseRev: 3,
  fields: { title: "New" },
};

describe("PocketBaseTransport request shape", () => {
  it("push sends POST /api/sync/push with protocol, auth, and JSON body", async () => {
    const { calls, fetch } = stubFetch([json(200, { results: [] })]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test/", token, fetch, client: { id: "dev-1" } });

    await t.push([mutation]);

    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe("https://pb.test/api/sync/push");
    expect(calls[0].init.method).toBe("POST");
    const headers = calls[0].init.headers as Record<string, string>;
    expect(headers[PROTOCOL_HEADER]).toBe("1");
    expect(headers["Content-Type"]).toBe("application/json");
    expect(headers.Authorization).toBe("Bearer tok-123");
    expect(JSON.parse(calls[0].init.body as string)).toEqual({
      client: { id: "dev-1" },
      mutations: [mutation],
    });
  });

  it("does not double-prefix an Authorization value that already has a scheme", async () => {
    const { calls, fetch } = stubFetch([json(200, { results: [] })]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token: { getToken: () => "Bearer x" }, fetch });
    await t.push([mutation]);
    expect((calls[0].init.headers as Record<string, string>).Authorization).toBe("Bearer x");
  });

  it("pull sends GET /api/sync/pull with cursor and limit, omitting a null cursor", async () => {
    const { calls, fetch } = stubFetch([
      json(200, { mode: "incremental", changes: [], cursor: "c1", hasMore: false, serverTime: "" }),
      json(200, { mode: "incremental", changes: [], cursor: "c2", hasMore: false, serverTime: "" }),
    ]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch });

    await t.pull(null, 200);
    await t.pull("opaque cursor", 50);

    expect(calls[0].url).toBe("https://pb.test/api/sync/pull?limit=200");
    expect(calls[0].init.method).toBe("GET");
    expect(calls[1].url).toContain("cursor=opaque+cursor");
    expect(calls[1].url).toContain("limit=50");
  });

  it("decodes push results and pull changes", async () => {
    const pushRes = {
      results: [{ id: "m1", status: "ok", rev: 4, merged: false, record: { id: "aaaaaaaaaaaaaaa", title: "New" } }],
    };
    const pullRes = {
      mode: "snapshot",
      changes: [{ collection: "notes", recordId: "aaaaaaaaaaaaaaa", op: "upsert", rev: 1, record: { id: "a" } }],
      cursor: "c9",
      hasMore: true,
      serverTime: "2026-10-15T08:31:00.000Z",
    };
    const { fetch } = stubFetch([json(200, pushRes), json(200, pullRes)]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch });

    await expect(t.push([mutation])).resolves.toEqual(pushRes);
    await expect(t.pull(null, 200)).resolves.toEqual(pullRes);
  });

  it("decodes /info", async () => {
    const info = {
      protocol: 1,
      supported: [1],
      limits: { maxBatch: 100, maxPage: 500, defaultPage: 200, maxBodyBytes: 1048576, idempotencyRetentionDays: 14 },
      collections: {},
    };
    const { calls, fetch } = stubFetch([json(200, info)]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch });
    await expect(t.info()).resolves.toEqual(info);
    expect(calls[0].url).toBe("https://pb.test/api/sync/info");
  });
});

describe("PocketBaseTransport error mapping (§12.1)", () => {
  const cases: Array<[number, string | undefined, string, number | undefined]> = [
    [410, "cursor_too_old", "cursor_too_old", undefined],
    [400, "invalid_request", "invalid_request", undefined],
    [401, "unauthorized", "unauthorized", undefined],
    [429, "rate_limited", "rate_limited", 120],
    [503, "server_error", "server_error", undefined],
  ];

  it.each(cases)("maps %i to a TransportError", async (status, bodyCode, expectedCode, retryAfter) => {
    const body = bodyCode ? { status, code: bodyCode, message: "nope" } : "not json";
    const { fetch } = stubFetch([
      status === 429
        ? json(status, body, { "Retry-After": "120" })
        : bodyCode
          ? json(status, body)
          : new Response("not json", { status }),
    ]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch });

    const err = await t.push([mutation]).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(TransportError);
    expect((err as TransportError).status).toBe(status);
    expect((err as TransportError).code).toBe(expectedCode);
    expect((err as TransportError).retryAfter).toBe(retryAfter);
  });

  it("maps a network failure to a TransportError with no status (offline)", async () => {
    const { fetch } = stubFetch([
      () => {
        throw new Error("ECONNREFUSED");
      },
    ]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch });

    const err = await t.push([mutation]).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(TransportError);
    expect((err as TransportError).status).toBeUndefined();
    expect((err as TransportError).message).toContain("ECONNREFUSED");
  });
});

describe("PocketBaseTransport nudge (§10)", () => {
  it("delegates to an injected nudge source and returns its unsubscribe", () => {
    const added: Array<() => void> = [];
    const source: NudgeSource = {
      subscribe(onNudge) {
        added.push(onNudge);
        return () => {
          const i = added.indexOf(onNudge);
          if (i >= 0) added.splice(i, 1);
        };
      },
    };
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch: stubFetch([]).fetch, nudge: source });

    let fired = 0;
    const unsubscribe = t.subscribe(() => fired++);
    expect(added).toHaveLength(1);

    added[0]();
    expect(fired).toBe(1);

    unsubscribe();
    expect(added).toHaveLength(0);
  });

  it("default SSE source fires the callback on a streamed event", async () => {
    const encoder = new TextEncoder();
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode("event: change\ndata: {}\n\n"));
        controller.close();
      },
    });
    const streamFetch = (async () =>
      new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } })) as unknown as typeof fetch;

    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch: streamFetch });
    const fired = await new Promise<boolean>((resolve) => {
      const stop = t.subscribe(() => {
        stop();
        resolve(true);
      });
      setTimeout(() => resolve(false), 2000);
    });
    expect(fired).toBe(true);
  });
});
