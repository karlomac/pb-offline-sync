import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { validateFixturesDir } from "../src/conformance";

const here = dirname(fileURLToPath(import.meta.url));
const fixturesDir = resolve(here, "../../../conformance/fixtures");

describe("conformance fixtures", () => {
  it("are well-formed and consistent with protocol v1", () => {
    const { files, errors } = validateFixturesDir(fixturesDir);
    expect(errors).toEqual([]);
    expect(files).toBeGreaterThan(0);
  });
});
