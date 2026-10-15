# Conformance fixtures

Language-neutral JSON fixtures for **protocol v1** of pb-offline-sync. The
normative detail lives in `docs/protocol.md`; this directory is a read-only
contract consumed by both the Go server tests and the TS client tests.

- Format and required categories: protocol **§15** (this file restates §15.1).
- Push semantics: §6. Pull: §7. Conflict/rev: §8. Idempotency: §9. Errors: §12.

## Layout

- `fixtures/<name>.json` — one scenario per file; the filename equals the
  fixture's `name`.

## Fixture shape

```json
{
  "name": "update-stale-base-nonoverlapping-merges",
  "description": "A stale baseRev with non-overlapping fields is merged.",
  "config": {
    "collections": {
      "notes": {
        "ownerField": "owner",
        "conflictPolicy": "serverWins",
        "deletePolicy": "deleteWins",
        "fields": [
          { "name": "title", "type": "text", "writable": true },
          { "name": "body", "type": "editor", "writable": true },
          { "name": "attachments", "type": "file", "writable": false }
        ]
      }
    }
  },
  "users": ["alice", "bob"],
  "steps": [ /* see below */ ]
}
```

- `name` — unique; matches the filename (`<name>.json`).
- `description` — human-readable, one line.
- `config` — server configuration the runner installs before running steps. It
  mirrors the `/info` collection descriptor (protocol §5): `ownerField`,
  `conflictPolicy` (`serverWins` | `clientWins` | `reject`), `deletePolicy`
  (`deleteWins` | `editWins`), and `fields[]` (`name`, PocketBase `type`,
  `writable`). `fields` extends the §15.1 example; it is required so
  `unknown_field` and `file_field_unsupported` are deterministic.
- `users` — identities the runner registers. A record's owner is the user named
  in the step's `as`; the owner field is set server-side (protocol §6.2 rule 6).
- `steps` — run **sequentially against a fresh server** (or client + mock
  server), so `rev`, `seq`, and idempotency state all start from a known point.

## Steps

A step names the acting user (`as`) and exactly one operation.

**Push** (protocol §6):

```json
{ "as": "alice",
  "push": { "mutations": [
    { "id": "m1", "collection": "notes", "recordId": "notesrecord0001",
      "op": "create", "fields": { "title": "t" } }
  ] },
  "expect": { "results": [ { "id": "m1", "status": "ok", "rev": 1 } ] } }
```

**Pull** (protocol §7):

```json
{ "as": "alice", "pull": { "cursor": "$prev", "limit": 2 },
  "expect": {
    "mode": "snapshot",
    "changes": [
      { "collection": "notes", "recordId": "notesrecord0001", "op": "upsert", "rev": 1 }
    ],
    "hasMore": true
  } }
```

- `pull` carries the `/api/sync/pull` query parameters; `{}` means an initial
  snapshot (cursor omitted/empty).
- Nested objects such as `results[].record` and `changes[].record` are matched
  under the same subset rule described below.

**Request-level error** (protocol §12.1): when the whole request is expected to
fail, `expect` describes the error body instead of `results`/`changes`:

```json
{ "as": "alice", "pull": { "cursor": "not a cursor!" },
  "expect": { "status": 400, "code": "cursor_invalid" } }
```

## `expect` is a subset match

- Only the properties listed in `expect` must match; anything unlisted
  (timestamps, opaque cursors, unlisted record fields) is ignored.
- Objects: every listed key must be present and match, recursively.
- Arrays: matched element-wise and in order; the actual array **MUST have the
  same length** as the expected array. This lets a fixture assert, for example,
  that a pull returns exactly one record.
- Wildcards: `"$any"` (any value), `"$string"` (any string), `"$int"` (any
  integer).

## Cursors

- Cursors are opaque; fixtures never write one literally in `expect`
  (protocol §7.4).
- A step passes the previous pull's cursor **by reference**:
  `"cursor": "$prev"`. The runner remembers the `cursor` returned by the most
  recent pull step.
- To exercise the stale-cursor path, use `"cursor": "$stale"`: the runner
  supplies a cursor its server decodes but that is older than retained history
  or ahead of head (e.g. it can encode `seq` beyond head), so the expected
  result is `410 cursor_too_old`. The runner-side driver builds this with the
  server's own cursor encoder; clients skip server-only sentinels.
- Pull `changes` are listed in **ascending `seq`** order with at most one entry
  per `(collection, recordId)` (dedupe, protocol §7.2). Fixtures assert
  ordering/`op`/`rev`, never literal cursor values.

## Interpretation notes (spec ambiguities)

1. **`duplicate` status (§6.5, §9).** §6.5 says a duplicate "carries the original
   stored result (including its original `status`…)", which conflicts with
   `status` being one of `ok|duplicate|conflict|rejected`. Fixtures expect
   `status: "duplicate"` carrying the original `rev`/`record`. A runner that
   nests the original result must still surface `status: "duplicate"`.
2. **Array matching.** §15.1 defines subset match for properties but says
   nothing about arrays; this file treats arrays as ordered, equal-length
   matches.
3. **Request-level error `expect`.** §15.1 only defines per-mutation `results`;
   a whole-request failure is expressed as
   `expect: { "status": <http>, "code": "…" }`.
4. **`cursor_too_old` input (§7.4).** Cursors are opaque, so a fixture must not
   construct one literally. The stale-cursor case uses the runner sentinel
   `"$stale"`: the runner encodes a decodable-but-out-of-range cursor with its
   own encoder. A literal undecodable token would instead be `400 cursor_invalid`
   by §7.4, which is why the fixture no longer uses one.
5. **`config.collections[].fields`.** Added to the §15.1 config shape (which
   omits it) so field-level rejections are deterministic.

## Validation

Every file must be valid JSON. From the repo root:

```
for f in conformance/fixtures/*.json; do node -e "JSON.parse(require('fs').readFileSync('$f'))" || echo "BAD $f"; done
```
