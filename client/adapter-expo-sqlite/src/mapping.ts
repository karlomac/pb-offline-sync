/**
 * PocketBase field type → local SQLite column mapping (PDD §9.4).
 *
 * The mappings are bijective: `decodeField(field, encodeField(field, v))`
 * equals the server value for every supported type, including `null` (PocketBase's
 * empty value). Select/relation multiplicity and the JSON-backed types are
 * carried in the schema, since `/info` only advertises the base field type.
 */
import type { SQLiteValue } from "./driver";

export type PbFieldType =
  | "text"
  | "email"
  | "url"
  | "editor"
  | "number"
  | "bool"
  | "date"
  | "autodate"
  | "select"
  | "json"
  | "geoPoint"
  | "relation"
  | "file";

/** One synced collection field, as needed for storage. `multiple` applies to select/relation. */
export interface SchemaField {
  name: string;
  type: PbFieldType;
  multiple?: boolean;
}

export type SQLiteColumnType = "TEXT" | "REAL" | "INTEGER";

/** PDD §9.4 column type for a field. */
export function localColumnType(field: SchemaField): SQLiteColumnType {
  switch (field.type) {
    case "number":
      return "REAL";
    case "bool":
      return "INTEGER";
    default:
      // text/email/url/editor/date/autodate → TEXT; select/relation single → TEXT;
      // select/relation multi, json, geoPoint, file → TEXT (JSON).
      return "TEXT";
  }
}

/** True for fields stored as a JSON string (JSON/geoPoint, multi-select, multi-relation, file). */
export function isJsonField(field: SchemaField): boolean {
  if (field.type === "json" || field.type === "geoPoint" || field.type === "file") return true;
  return (field.type === "select" || field.type === "relation") && field.multiple === true;
}

/** Server value → SQLite column value. `null` is PocketBase's empty value. */
export function encodeField(field: SchemaField, value: unknown): SQLiteValue {
  if (value === null || value === undefined) return null;
  switch (field.type) {
    case "number":
      return typeof value === "number" ? value : Number(value);
    case "bool":
      return value ? 1 : 0;
    default:
      if (isJsonField(field)) return JSON.stringify(value);
      return typeof value === "string" ? value : String(value);
  }
}

/** SQLite column value → server value. */
export function decodeField(field: SchemaField, stored: SQLiteValue | undefined): unknown {
  if (stored === null || stored === undefined) return null;
  switch (field.type) {
    case "number":
      return typeof stored === "number" ? stored : Number(stored);
    case "bool":
      return Number(stored) !== 0;
    default:
      if (isJsonField(field)) return typeof stored === "string" ? JSON.parse(stored) : stored;
      return typeof stored === "string" ? stored : String(stored);
  }
}
