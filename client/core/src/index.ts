/**
 * pb-offline-sync client core.
 *
 * Protocol version + conformance validator (M0), and the sync engine:
 * wire types, platform seams, in-memory adapter/transport, outbox coalescing,
 * and the sync loop (M4).
 */
export const PROTOCOL_VERSION = 1;

export * from "./types";
export * from "./interfaces";
export * from "./outbox";
export * from "./memory";
export * from "./engine";
