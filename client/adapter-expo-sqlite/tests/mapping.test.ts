import { describe, expect, it } from "vitest";
import { decodeField, encodeField, isJsonField, localColumnType } from "../src/mapping";
import type { PbFieldType, SchemaField } from "../src/mapping";

const field = (type: PbFieldType, multiple?: boolean): SchemaField => ({ name: "f", type, multiple });

const VALUES: Array<[PbFieldType, unknown, boolean?]> = [
  ["text", "héllo 🌍"],
  ["email", "a@b.co"],
  ["url", "https://example.com/x?y=1"],
  ["editor", "<p>hi</p>"],
  ["number", 42.5],
  ["number", 7],
  ["bool", true],
  ["bool", false],
  ["date", "2026-10-15 08:30:00.000Z"],
  ["autodate", "2026-10-15 08:30:00.000Z"],
  ["select", "open"],
  ["select", ["a", "b"], true],
  ["json", { a: 1, b: [2, { c: true }] }],
  ["json", [1, "two", null]],
  ["geoPoint", { lon: 1.5, lat: -2.25 }],
  ["relation", "aaaaaaaaaaaaaaa"],
  ["relation", ["aaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbb"], true],
  ["file", ["a.png", "b.pdf"]],
];

describe("PDD §9.4 column types", () => {
  it.each<[PbFieldType, boolean | undefined, ("TEXT" | "REAL" | "INTEGER")]>([
    ["text", undefined, "TEXT"],
    ["email", undefined, "TEXT"],
    ["url", undefined, "TEXT"],
    ["editor", undefined, "TEXT"],
    ["number", undefined, "REAL"],
    ["bool", undefined, "INTEGER"],
    ["date", undefined, "TEXT"],
    ["autodate", undefined, "TEXT"],
    ["select", undefined, "TEXT"],
    ["select", true, "TEXT"],
    ["json", undefined, "TEXT"],
    ["geoPoint", undefined, "TEXT"],
    ["relation", undefined, "TEXT"],
    ["relation", true, "TEXT"],
    ["file", undefined, "TEXT"],
  ])("%s (multiple=%s) → %s", (type, multiple, expected) => {
    expect(localColumnType(field(type, multiple))).toBe(expected);
  });

  it("flags JSON-backed fields", () => {
    expect(isJsonField(field("json"))).toBe(true);
    expect(isJsonField(field("geoPoint"))).toBe(true);
    expect(isJsonField(field("file"))).toBe(true);
    expect(isJsonField(field("select", true))).toBe(true);
    expect(isJsonField(field("relation", true))).toBe(true);
    expect(isJsonField(field("select"))).toBe(false);
    expect(isJsonField(field("relation"))).toBe(false);
    expect(isJsonField(field("text"))).toBe(false);
  });
});

describe("round-trip: server value → local → server value", () => {
  it.each(VALUES)("%s %o round-trips", (type, value, multiple) => {
    const f = field(type, multiple);
    expect(decodeField(f, encodeField(f, value))).toEqual(value);
  });

  it.each(VALUES)("%s null (empty) round-trips as null", (type, _value, multiple) => {
    const f = field(type, multiple);
    expect(encodeField(f, null)).toBeNull();
    expect(encodeField(f, undefined)).toBeNull();
    expect(decodeField(f, encodeField(f, null))).toBeNull();
  });

  it("stores bool as 0/1 and JSON types as text", () => {
    expect(encodeField(field("bool"), true)).toBe(1);
    expect(encodeField(field("bool"), false)).toBe(0);
    expect(encodeField(field("json"), { a: 1 })).toBe('{"a":1}');
    expect(encodeField(field("select", true), ["x"])).toBe('["x"]');
    expect(encodeField(field("file"), ["a.png"])).toBe('["a.png"]');
    expect(encodeField(field("number"), 3)).toBe(3);
    expect(encodeField(field("text"), "hi")).toBe("hi");
  });
});
