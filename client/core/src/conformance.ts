import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

/**
 * Conformance fixture validator (protocol §15).
 *
 * Mirrors the Go validator in server/syncplugin/conformance_test.go so both
 * implementations agree on the contract fixtures. The execution driver is
 * wired in at M4; at M0 this validates well-formedness and the wire rules.
 */

export const RECORD_ID_RE = /^[a-z0-9]{15}$/;
export const MUTATION_ID_RE = /^[A-Za-z0-9_-]{1,64}$/;

export interface Mutation {
  id: string;
  collection: string;
  recordId: string;
  op: "create" | "update" | "delete";
  baseRev?: number;
  fields?: Record<string, unknown>;
}

export interface Step {
  as: string;
  push?: { mutations: Mutation[] };
  pull?: Record<string, unknown>;
  /** §18 media-channel step; executed runner-side (server driver) or by client media fixtures. */
  media?: { op: "init" | "upload" | "offset" | "commit" | "abort"; request?: unknown; blob?: string; slice?: number[] };
  expect: unknown;
}

export interface Fixture {
  name: string;
  description: string;
  config: unknown;
  users: string[];
  /** Named blob contents; "$sha:<name>"/"$size:<name>" sentinels in media steps refer to these. */
  blobs?: Record<string, string>;
  steps: Step[];
}

export function validateFixture(file: string, f: Fixture): string[] {
  const errors: string[] = [];
  const want = file.replace(/\.json$/, "");
  if (f.name !== want) errors.push(`${file}: name ${JSON.stringify(f.name)} must equal filename`);
  if (!f.description) errors.push(`${file}: description is required`);
  if (f.config == null) errors.push(`${file}: config is required`);
  if (!Array.isArray(f.users) || f.users.length === 0) {
    errors.push(`${file}: users must be non-empty`);
  }
  if (!Array.isArray(f.steps) || f.steps.length === 0) {
    errors.push(`${file}: steps must be non-empty`);
  }

  const users = new Set(f.users ?? []);
  (f.steps ?? []).forEach((s, i) => {
    const ctx = `${file} step ${i}`;
    if (!s.as || !users.has(s.as)) errors.push(`${ctx}: \`as\` must name a configured user`);
    const hasPush = s.push != null;
    const hasPull = s.pull != null;
    const hasMedia = s.media != null;
    if (hasMedia ? hasPush || hasPull : hasPush === hasPull) {
      errors.push(`${ctx}: exactly one of push/pull/media must be set`);
    }
    if (s.expect == null) errors.push(`${ctx}: expect is required`);
    if (hasMedia && s.media != null) {
      const m = s.media;
      if (!["init", "upload", "offset", "commit", "abort"].includes(m.op)) {
        errors.push(`${ctx}: media.op must be init|upload|offset|commit|abort`);
      }
      if ((m.op === "init" || m.op === "commit") && (m.request == null || typeof m.request !== "object")) {
        errors.push(`${ctx}: media ${m.op} requires request`);
      }
      if (m.op === "upload") {
        if (!m.blob) {
          errors.push(`${ctx}: media upload requires blob`);
        } else if (f.blobs == null || !(m.blob in f.blobs)) {
          errors.push(`${ctx}: media upload references unknown blob ${m.blob}`);
        } else if (m.slice != null) {
          if (m.slice.length !== 2 || m.slice.some((n) => !Number.isInteger(n))) {
            errors.push(`${ctx}: media upload slice must be [from,to] integers`);
          } else {
            const len = new TextEncoder().encode(f.blobs[m.blob]).byteLength;
            const [from, to] = m.slice;
            if (from < 0 || to > len || from > to) errors.push(`${ctx}: media upload slice out of range for blob ${m.blob}`);
          }
        }
      }
      return;
    }
    if (!hasPush || s.push == null) return;
    if (!Array.isArray(s.push.mutations) || s.push.mutations.length === 0) {
      errors.push(`${ctx}: push.mutations must be non-empty`);
      return;
    }
    s.push.mutations.forEach((m, j) => {
      const mctx = `${ctx} mutation ${j} (${m.id})`;
      if (!MUTATION_ID_RE.test(m.id)) errors.push(`${mctx}: invalid mutation id`);
      if (!m.collection) errors.push(`${mctx}: collection required`);
      if (!RECORD_ID_RE.test(m.recordId)) errors.push(`${mctx}: invalid recordId ${m.recordId}`);
      if (m.op === "create") {
        if (m.baseRev != null) errors.push(`${mctx}: create must omit baseRev`);
      } else if (m.op === "update") {
        if (m.baseRev == null) errors.push(`${mctx}: update requires baseRev`);
        if (m.fields == null || Object.keys(m.fields).length === 0) {
          errors.push(`${mctx}: update requires non-empty fields`);
        }
      } else if (m.op === "delete") {
        if (m.baseRev == null) errors.push(`${mctx}: delete requires baseRev`);
        if (m.fields != null) errors.push(`${mctx}: delete must omit fields`);
      } else {
        errors.push(`${mctx}: op must be create|update|delete`);
      }
    });
  });

  return errors;
}

export function validateFixturesDir(dir: string): { files: number; errors: string[] } {
  const files = readdirSync(dir).filter((n) => n.endsWith(".json"));
  const errors: string[] = [];
  for (const name of files) {
    const f = JSON.parse(readFileSync(join(dir, name), "utf8")) as Fixture;
    errors.push(...validateFixture(name, f));
  }
  if (files.length === 0) errors.push(`no fixtures found in ${dir}`);
  return { files: files.length, errors };
}
