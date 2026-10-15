/**
 * PocketBase HTTP transport (protocol §2, §5, §6, §7, §10).
 *
 * Speaks the sync endpoints with JSON bodies and the `X-Sync-Protocol: 1`
 * header, sending the injected token as `Authorization`. Non-2xx responses
 * become a `TransportError` carrying the HTTP `status`, the protocol `code`
 * (§12.1), and `retryAfter` for 429. Network failures become a `TransportError`
 * with no status, which the engine treats as offline (§11.2).
 */
import { TransportError } from "@pb-offline-sync/core";
import type { Mutation, PullResponse, PushResponse, Transport } from "@pb-offline-sync/core";
import { sseNudgeSource } from "./nudge";
import type { NudgeSource, SyncInfo, TokenProvider } from "./types";

export const PROTOCOL_HEADER = "X-Sync-Protocol";
export const PROTOCOL_VERSION = 1;

export interface PocketBaseTransportOptions {
  /** Origin, e.g. `https://pb.example.com` (trailing slash optional). */
  baseUrl: string;
  token: TokenProvider;
  /** Injectable for tests; defaults to the global `fetch`. */
  fetch?: typeof fetch;
  /** Optional diagnostics-only client identity (§6.1). */
  client?: { id: string; name?: string };
  /** Sync endpoint base path; default `/api/sync`. */
  basePath?: string;
  /** Nudge source (§10); defaults to an SSE reader at `{baseUrl}/api/realtime`. */
  nudge?: NudgeSource;
}

export class PocketBaseTransport implements Transport {
  private readonly origin: string;
  private readonly basePath: string;
  private readonly token: TokenProvider;
  private readonly doFetch: typeof fetch;
  private readonly client?: { id: string; name?: string };
  private readonly nudge: NudgeSource;

  constructor(opts: PocketBaseTransportOptions) {
    this.origin = opts.baseUrl.replace(/\/+$/, "");
    this.basePath = (opts.basePath ?? "/api/sync").replace(/\/+$/, "");
    this.token = opts.token;
    this.doFetch = opts.fetch ?? fetch;
    this.client = opts.client;
    this.nudge =
      opts.nudge ??
      sseNudgeSource({ url: `${this.origin}/api/realtime`, token: this.token, fetch: this.doFetch });
  }

  /** §5 capabilities/limits. Not part of the `Transport` seam; hosts read it to configure the engine. */
  async info(): Promise<SyncInfo> {
    return (await this.readJson(await this.send("GET", "/info"))) as SyncInfo;
  }

  /** §6 push. */
  async push(mutations: Mutation[]): Promise<PushResponse> {
    const body = this.client ? { client: this.client, mutations } : { mutations };
    return (await this.readJson(await this.send("POST", "/push", body))) as PushResponse;
  }

  /** §7 pull. */
  async pull(cursor: string | null, limit: number): Promise<PullResponse> {
    const params = new URLSearchParams();
    if (cursor != null && cursor !== "") params.set("cursor", cursor);
    params.set("limit", String(limit));
    return (await this.readJson(await this.send("GET", `/pull?${params.toString()}`))) as PullResponse;
  }

  /** §10 nudge subscription; returns the unsubscribe function. */
  subscribe(onNudge: () => void): () => void {
    return this.nudge.subscribe(onNudge);
  }

  private async send(method: string, path: string, body?: unknown): Promise<Response> {
    const headers: Record<string, string> = { [PROTOCOL_HEADER]: String(PROTOCOL_VERSION) };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const token = await this.token.getToken();
    if (token) headers.Authorization = token.startsWith("Bearer ") ? token : `Bearer ${token}`;

    let res: Response;
    try {
      res = await this.doFetch(`${this.origin}${this.basePath}${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
      });
    } catch (err) {
      // Offline / DNS / TLS: no status so the engine treats it as offline (§11.2).
      throw new TransportError(`network error: ${err instanceof Error ? err.message : String(err)}`);
    }
    if (!res.ok) throw await toTransportError(res);
    return res;
  }

  private async readJson(res: Response): Promise<unknown> {
    try {
      return await res.json();
    } catch {
      throw new TransportError(`invalid JSON response (HTTP ${res.status})`, { status: res.status });
    }
  }
}

async function toTransportError(res: Response): Promise<TransportError> {
  let code: string | undefined;
  let message = `HTTP ${res.status}`;
  try {
    const body = (await res.json()) as { code?: unknown; message?: unknown };
    if (body && typeof body === "object") {
      if (typeof body.code === "string") code = body.code;
      if (typeof body.message === "string") message = body.message;
    }
  } catch {
    // Non-JSON error body: fall back to the status-based code.
  }
  return new TransportError(message, {
    status: res.status,
    code: code ?? defaultCode(res.status),
    retryAfter: parseRetryAfter(res.headers?.get?.("Retry-After")),
  });
}

/** §12.1 status → code when the body carries no code. */
function defaultCode(status: number): string {
  switch (status) {
    case 401:
      return "unauthorized";
    case 403:
      return "forbidden";
    case 410:
      return "cursor_too_old";
    case 429:
      return "rate_limited";
    case 413:
      return "batch_too_large";
    default:
      return status >= 500 ? "server_error" : "invalid_request";
  }
}

function parseRetryAfter(value: string | null | undefined): number | undefined {
  if (!value) return undefined;
  const seconds = Number(value);
  if (Number.isFinite(seconds)) return seconds;
  const date = Date.parse(value);
  if (Number.isNaN(date)) return undefined;
  return Math.max(0, Math.round((date - Date.now()) / 1000));
}
