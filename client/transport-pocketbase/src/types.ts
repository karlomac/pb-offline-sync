/**
 * Shared types for the PocketBase transport. `TransportError` and the wire
 * request/response shapes come from `@pb-offline-sync/core` (protocol §6/§7).
 */

/** Supplies the PocketBase auth token per request (PDD §9.8). Raw token or a full `Bearer …` value. */
export interface TokenProvider {
  getToken(): string | null | undefined | Promise<string | null | undefined>;
}

/** Protocol §5 `/info` limits. */
export interface SyncLimits {
  maxBatch: number;
  maxPage: number;
  defaultPage: number;
  maxBodyBytes: number;
  idempotencyRetentionDays: number;
}

/** Protocol §5 `/info` response. */
export interface SyncInfo {
  protocol: number;
  supported: number[];
  pluginVersion?: string;
  serverTime?: string;
  limits: SyncLimits;
  collections: Record<string, unknown>;
}

/** A swappable nudge source (protocol §10). Payload is never authoritative. */
export interface NudgeSource {
  subscribe(onNudge: () => void): () => void;
}

export interface SseNudgeOptions {
  url: string;
  token?: TokenProvider;
  fetch?: typeof fetch;
}
