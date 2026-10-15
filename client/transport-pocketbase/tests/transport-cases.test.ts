/**
 * Transport sub-cases not proven by the original suite (docs/test-scenarios.md
 * §12 C-TR-1/C-TR-2/C-TR-4): a non-null pull cursor is actually sent, a token
 * without a scheme gets `Bearer ` prepended on GET as well as POST, a non-JSON
 * error body still maps by status, and 429 `Retry-After` parses as both seconds
 * and an HTTP-date.
 */
import { describe, expect, it } from "vitest";
import { TransportError } from "@pb-offline-sync/core";
import type { Mutation } from "@pb-offline-sync/core";
import { PocketBaseTransport } from "../src/transport";

interface Call {
  url: string;
  init: RequestInit;
}

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

const mutation: Mutation = {
  id: "m1",
  collection: "notes",
  recordId: "aaaaaaaaaaaaaaa",
  op: "update",
  baseRev: 3,
  fields: { title: "New" },
};

describe("PocketBaseTransport request sub-cases (C-TR-1/C-TR-2)", () => {
  it("sends a non-null pull cursor as the exact `cursor` query parameter", async () => {
    const { calls, fetch } = stubFetch([
      json(200, { mode: "incremental", changes: [], cursor: "c2", hasMore: false, serverTime: "" }),
    ]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token: { getToken: () => "raw-tok" }, fetch });

    await t.pull("opaque/cur sor=1", 25);

    const url = new URL(calls[0].url);
    expect(url.pathname).toBe("/api/sync/pull");
    expect(url.searchParams.get("cursor")).toBe("opaque/cur sor=1");
    expect(url.searchParams.get("limit")).toBe("25");
  });

  it("omits the cursor entirely when it is null or empty", async () => {
    const { calls, fetch } = stubFetch([
      json(200, { mode: "incremental", changes: [], cursor: "c", hasMore: false, serverTime: "" }),
      json(200, { mode: "incremental", changes: [], cursor: "c", hasMore: false, serverTime: "" }),
    ]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token: { getToken: () => "raw-tok" }, fetch });

    await t.pull(null, 10);
    await t.pull("", 10);

    expect(calls[0].url).toBe("https://pb.test/api/sync/pull?limit=10");
    expect(calls[1].url).toBe("https://pb.test/api/sync/pull?limit=10");
  });

  it("prepends `Bearer ` to a schemeless token on GET and does not double-prefix a full value", async () => {
    const { calls, fetch } = stubFetch([
      json(200, { protocol: 1, supported: [1], limits: {}, collections: {} }),
      json(200, { protocol: 1, supported: [1], limits: {}, collections: {} }),
    ]);
    const schemeless = new PocketBaseTransport({ baseUrl: "https://pb.test", token: { getToken: () => "ABC" }, fetch });
    const withScheme = new PocketBaseTransport({ baseUrl: "https://pb.test", token: { getToken: () => "Bearer ABC" }, fetch });

    await schemeless.info();
    await withScheme.info();

    expect((calls[0].init.headers as Record<string, string>).Authorization).toBe("Bearer ABC");
    expect((calls[1].init.headers as Record<string, string>).Authorization).toBe("Bearer ABC");
  });
});

describe("PocketBaseTransport error sub-cases (C-TR-4)", () => {
  it("maps a non-JSON error body by status (5xx → server_error)", async () => {
    const { fetch } = stubFetch([new Response("<html>gateway</html>", { status: 502, headers: { "Content-Type": "text/html" } })]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token: { getToken: () => "t" }, fetch });

    const err = (await t.push([mutation]).catch((e: unknown) => e)) as TransportError;
    expect(err).toBeInstanceOf(TransportError);
    expect(err.status).toBe(502);
    expect(err.code).toBe("server_error");
    expect(err.retryAfter).toBeUndefined();
  });

  it("maps a non-JSON 400 body to invalid_request", async () => {
    const { fetch } = stubFetch([new Response("plain text", { status: 400 })]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token: { getToken: () => "t" }, fetch });

    const err = (await t.push([mutation]).catch((e: unknown) => e)) as TransportError;
    expect(err.status).toBe(400);
    expect(err.code).toBe("invalid_request");
  });

  it("parses a numeric Retry-After as seconds", async () => {
    const { fetch } = stubFetch([json(429, { code: "rate_limited", message: "slow down" }, { "Retry-After": "120" })]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token: { getToken: () => "t" }, fetch });

    const err = (await t.push([mutation]).catch((e: unknown) => e)) as TransportError;
    expect(err.status).toBe(429);
    expect(err.code).toBe("rate_limited");
    expect(err.retryAfter).toBe(120);
  });

  it("parses a date Retry-After as seconds from now", async () => {
    const when = new Date(Date.now() + 90_000).toUTCString();
    const { fetch } = stubFetch([json(429, { code: "rate_limited", message: "slow down" }, { "Retry-After": when })]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token: { getToken: () => "t" }, fetch });

    const err = (await t.push([mutation]).catch((e: unknown) => e)) as TransportError;
    expect(err.status).toBe(429);
    expect(err.retryAfter).toBeGreaterThan(85);
    expect(err.retryAfter).toBeLessThanOrEqual(90);
  });
});
