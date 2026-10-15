/**
 * @pb-offline-sync/transport-pocketbase
 *
 * A `Transport` (protocol §6/§7) over HTTP using the global `fetch`, with an
 * injected token provider (§9.8) and a swappable realtime nudge source (§10).
 */
export { PocketBaseTransport, PROTOCOL_HEADER, PROTOCOL_VERSION } from "./transport";
export type { PocketBaseTransportOptions } from "./transport";
export { sseNudgeSource } from "./nudge";
export type { NudgeSource, SseNudgeOptions, SyncInfo, SyncLimits, TokenProvider } from "./types";
