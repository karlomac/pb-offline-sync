/**
 * Wire types for protocol v1 (docs/protocol.md). These mirror the JSON on the
 * wire; they intentionally contain nothing platform-specific.
 */

export type MutationOp = "create" | "update" | "delete";
export type MutationStatus = "ok" | "duplicate" | "conflict" | "rejected";

/** §6.1 request mutation. */
export interface Mutation {
  id: string;
  collection: string;
  recordId: string;
  op: MutationOp;
  baseRev?: number;
  fields?: Record<string, unknown>;
}

export interface PushRequest {
  client?: { id: string; name?: string };
  mutations: Mutation[];
}

/** §6.5 per-mutation result. */
export interface PushResult {
  id: string;
  status: MutationStatus;
  rev?: number;
  record?: Record<string, unknown>;
  merged?: boolean;
  code?: string;
  message?: string;
  conflictId?: string;
}

export interface PushResponse {
  results: PushResult[];
}

export type ChangeOp = "upsert" | "delete";

/** §7.1 pull change entry. */
export interface Change {
  collection: string;
  recordId: string;
  op: ChangeOp;
  rev: number;
  record?: Record<string, unknown>;
}

export interface PullResponse {
  mode: "snapshot" | "incremental";
  changes: Change[];
  cursor: string;
  hasMore: boolean;
  serverTime: string;
}

/** §12.2 conflict codes. */
export const CONFLICT_CODES = ["field_conflict", "deleted", "exists", "edited_since"] as const;
export type ConflictCode = (typeof CONFLICT_CODES)[number];

/** §12.2 rejected codes. */
export const REJECTED_CODES = [
  "collection_not_synced",
  "invalid_op",
  "invalid_id",
  "invalid_base_rev",
  "unknown_field",
  "forbidden_field",
  "file_field_unsupported",
  "validation_failed",
  "unique_violation",
  "rule_denied",
  "id_unavailable",
  "not_found",
  "dependency_failed",
  "mutation_id_reuse",
] as const;
export type RejectedCode = (typeof REJECTED_CODES)[number];

/** §12.1 request-level error codes. */
export const REQUEST_ERROR_CODES = [
  "protocol_unsupported",
  "invalid_request",
  "cursor_invalid",
  "batch_too_large",
  "unauthorized",
  "forbidden",
  "cursor_too_old",
  "rate_limited",
  "server_error",
] as const;
export type RequestErrorCode = (typeof REQUEST_ERROR_CODES)[number];

export function isConflictCode(code: string | undefined): boolean {
  return code != null && (CONFLICT_CODES as readonly string[]).includes(code);
}

/**
 * Thrown by a Transport implementation for request-level failures (§12.1) and
 * network problems. The engine maps `status`/`code` to retry vs. resync (§11.2).
 */
export class TransportError extends Error {
  readonly status?: number;
  readonly code?: string;
  readonly retryAfter?: number;

  constructor(
    message: string,
    opts: { status?: number; code?: string; retryAfter?: number } = {},
  ) {
    super(message);
    this.name = "TransportError";
    this.status = opts.status;
    this.code = opts.code;
    this.retryAfter = opts.retryAfter;
  }
}
