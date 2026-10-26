import { expect, it, describe } from "vitest";
import { PocketBaseTransport, PROTOCOL_HEADER, PROTOCOL_VERSION, TUS_CONTENT_TYPE } from "../src/transport";

interface Call {
  url: string;
  init: RequestInit;
}

/** Fetch stub queueing JSON/text/binary and header responses. */
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

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const token = { getToken: () => "tok-123" };
const initReq = { collection: "media", sha256: "a".repeat(64), size: 5, mime: "image/png", filename: "p.png" };

describe("PocketBaseTransport media (§18)", () => {
  it("mediaInit posts JSON to /api/sync/media/init", async () => {
    const { calls, fetch } = stubFetch([json(200, { status: "upload", uploadId: "u_1", chunkSize: 4, expiresIn: 60 })]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch });

    const res = await t.mediaInit(initReq);

    expect(calls[0].url).toBe("https://pb.test/api/sync/media/init");
    expect(calls[0].init.method).toBe("POST");
    expect(calls[0].init.headers).toMatchObject({
      [PROTOCOL_HEADER]: String(PROTOCOL_VERSION),
      Authorization: "Bearer tok-123",
    });
    expect(JSON.parse(calls[0].init.body as string)).toEqual(initReq);
    expect(res).toEqual({ status: "upload", uploadId: "u_1", chunkSize: 4, expiresIn: 60 });
  });

  it("mediaCommit posts the uploadId (optionally with a poster session)", async () => {
    const { calls, fetch } = stubFetch([
      json(200, { status: "ok", mediaId: "m1", rev: 1, file: { name: "p.png", size: 5 } }),
    ]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch });

    const res = await t.mediaCommit({ uploadId: "u_1", posterUploadId: "u_2" });

    expect(JSON.parse(calls[0].init.body as string)).toEqual({ uploadId: "u_1", posterUploadId: "u_2" });
    expect(res.mediaId).toBe("m1");
  });

  it("mediaOffset reads the Upload-Offset header from HEAD", async () => {
    const { calls, fetch } = stubFetch([
      new Response(null, { status: 200, headers: { "Upload-Offset": "12", "Upload-Length": "34" } }),
    ]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch });

    const offset = await t.mediaOffset("u_abc");

    expect(calls[0].init.method).toBe("HEAD");
    expect(calls[0].url).toBe("https://pb.test/api/sync/media/upload/u_abc");
    expect(offset).toBe(12);
  });

  it("mediaAppend PATCHes tus-encoded bytes and validates the echoed offset", async () => {
    const replies: Array<Response | (() => never)> = [
      new Response(null, { status: 204, headers: { "Upload-Offset": "5" } }),
      new Response(null, { status: 204, headers: { "Upload-Offset": "9" } }),
    ];
    const { calls, fetch } = stubFetch(replies);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch });

    const chunk = new TextEncoder().encode("bytes");
    await t.mediaAppend("u_abc", 0, chunk.slice(0, 5));

    expect(calls[0].init.method).toBe("PATCH");
    expect(calls[0].init.headers).toMatchObject({ "Upload-Offset": "0", "Content-Type": TUS_CONTENT_TYPE });
    expect(new Uint8Array(calls[0].init.body as ArrayBuffer)).toEqual(new Uint8Array([98, 121, 116, 101, 115]));

    await expect(t.mediaAppend("u_abc", 4, chunk.slice(5))).rejects.toMatchObject({
      name: "TransportError",
    });
  });

  it("mediaAbort DELETEs the session", async () => {
    const { calls, fetch } = stubFetch([new Response(null, { status: 204 })]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch });

    await t.mediaAbort("u_abc");

    expect(calls[0].init.method).toBe("DELETE");
    expect(calls[0].url).toBe("https://pb.test/api/sync/media/upload/u_abc");
  });

  it("surfaces protocol error codes from the media endpoints", async () => {
    const { fetch } = stubFetch([json(410, { status: 410, code: "upload_expired", message: "expired" })]);
    const t = new PocketBaseTransport({ baseUrl: "https://pb.test", token, fetch });

    await expect(t.mediaOffset("u_gone")).rejects.toMatchObject({ code: "upload_expired", status: 410 });
  });
});
