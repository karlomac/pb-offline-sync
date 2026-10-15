# Test scenarios

A catalog of the scenarios the sync layer must satisfy, for **both** sides, mapped
to existing coverage so the gaps are explicit. The protocol (`docs/protocol.md`)
is the source of truth; where a scenario is protocol-affecting the §reference is
given.

**Legend** — Coverage: ✅ covered · ◐ partial · ✗ **gap** (no test yet).

> **Validation run (2026-10-15).** An agent team executed this catalog end to end
> and closed the gaps listed at the end of this file as new tests. No product
> bugs were found and no source changes were required. The inline markers below
> are the pre-run baseline; the **Validation run — results** section at the end
> is authoritative.

How to run everything:

```
server/                              go test ./... -count=1
client/core/                         pnpm install && pnpm run typecheck && pnpm test
client/transport-pocketbase/         pnpm install && pnpm run typecheck && pnpm test
client/adapter-expo-sqlite/          pnpm install && pnpm run typecheck && pnpm test
```

Concurrency/convergence harness: `go test ./syncplugin/ -run Convergence -count=5`.

---

## 1. Server — configuration & migrations

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| S-CFG-1 | Configured collection unknown / owner field missing / not a relation / relation not to an auth collection / view collection / invalid policy enum | §8 | `MustRegister` fails fast (error aborts `OnServe`) | ✅ `TestConfigValidationFailsFast` |
| S-CFG-2 | Zero limits get defaults; `DefaultPage` clamped to `MaxPage` | §5 | Defaults applied and advertised | ◐ implicit via `TestInfoShape` |
| S-MIG-1 | Fresh DB gets `sync_meta`, `sync_changes` (+`created`, +indexes), `sync_processed`, `sync_conflicts` | §17.1 | Schema present; all other tests rely on it | ✅ exercised by every test |
| S-MIG-2 | Migrations are idempotent (re-run registers once per process) | — | No error, no duplicate migration | ◐ `sync.Once`; not explicitly re-run |
| S-MIG-3 | **Upgrade** path: pre-M3 `sync_changes` without `created` → `ALTER` + bounded backfill; rows with `created = ''` repaired on any run | §13 | Column added, values filled, no long write lock | ✗ **gap** (only the fresh path runs) |

## 2. Server — change-log hooks & rev

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| S-HOOK-1 | Create → rev 1, changed set = all fields, one log row | §8.1–8.2 | `sync_meta.rev = 1`, log `op=create` | ✅ `TestRevIncrementsAndChangedFields` |
| S-HOOK-2 | Update → rev +1, changed set = differing fields | §8.2 | rev 2,3…; diff correct | ✅ `TestRevIncrementsAndChangedFields` |
| S-HOOK-3 | No-op update (identical state) skips the increment | §8.1 (MAY) | No rev bump, no log row | ✗ **gap** |
| S-HOOK-4 | Delete → rev increments and the `sync_meta` row is retained (tombstone) | §8.1, §17.1.1 | tombstone rev answerable | ✅ `TestDeleteIncrementsAndRetainsTombstone` |
| S-HOOK-5 | Rolled-back update/delete leaves no log row | §8.5 | No `sync_changes` row survives | ✅ `TestRollbackAtomicityUpdateAndDelete` |
| S-HOOK-6 | `On*AfterSuccess` is post-commit (must not log) | §17.1.4 | Spike proves the boundary | ✅ `TestAfterSuccessHookIsPostCommit` |
| S-HOOK-7 | A write via the normal PocketBase save path (not sync) is captured | §8.5 | Appears in pull with rev + changed fields | ✅ `TestPullNormalSavePath` |
| S-HOOK-8 | Pre-plugin record's first tracked write uses the next rev (no reuse of the snapshot rev) | §8.1 | First write rev 2, delivered | ✅ `TestLegacyRecordFirstWriteDoesNotReuseRev` |

## 3. Server — `/info`

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| S-INFO-1 | Shape: `protocol`, `supported`, `pluginVersion`, `serverTime`, `limits`, `collections[].fields[].writable` (file=false), system/owner fields excluded | §5 | Matches §5 | ✅ `TestInfoShape` |
| S-INFO-2 | Missing/unsupported `X-Sync-Protocol` → `400 protocol_unsupported` with `supported` | §2 | 400 + body | ✅ `TestInfoProtocolHeader` |
| S-INFO-3 | Unauthenticated → 401 | §14.1 | 401 | ✅ `TestInfoRequiresAuth` |
| S-INFO-4 | `X-Sync-Protocol` enforced on `/push` and `/pull` too | §2 | 400 `protocol_unsupported` | ✗ **gap** |

## 4. Server — `/pull`

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| S-PULL-1 | Initial snapshot pages with `hasMore`, then transitions to incremental at the captured head | §7.3 | `mode` snapshot→incremental | ✅ `TestPullSnapshotPagingHasMoreAndTransition` |
| S-PULL-2 | Incremental dedupe to latest per `(collection, recordId)`; deleted records delivered as `delete` | §7.2 | One change per record; delete op | ✅ `TestPullIncrementalDedupeAndDelete` |
| S-PULL-3 | Owner isolation: one user never sees another's changes | §7.2, §14.3 | Only own rows | ✅ `TestPullOwnerIsolation`, `TestSecurityPullIsolationWithBothUsers` |
| S-PULL-4 | Malformed cursor → 400; stale/ahead cursor → 410 | §7.4 | 400 `cursor_invalid` / 410 `cursor_too_old` | ✅ `TestPullCursorErrors` |
| S-PULL-5 | `maxPage` clamps; `defaultPage` used when no limit | §7.1 | Bounded page size | ✅ `TestPullEnforcesMaxPage`, `TestPullUsesDefaultPage` |
| S-PULL-6 | Snapshot stays correct when records change mid-snapshot (new/edited/deleted rows have `seq > H` and are redelivered incrementally; no record skipped/duplicated across the boundary) | §7.3.2 | Eventual completeness, no data loss | ✗ **gap** |

## 5. Server — `/push` (validation, rev, conflicts, delete)

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| S-PUSH-1 | create→rev1, update→rev2, delete→rev3 happy path | §6, §8.1 | revs 1,2,3; records returned | ✅ `TestPushCreateUpdateDeleteRevIncrements` |
| S-PUSH-2 | Structural validation: bad op, bad record id, missing/zero `baseRev`, `baseRev` on create, bad `baseRev > current` | §6.2, §12.2 | Correct rejected codes | ✅ `TestPushStructuralValidation`, `TestPushInvalidBaseRev` |
| S-PUSH-3 | Field rejection: server-authoritative (`id`/owner/`created`/`updated`/`collectionId`), unknown, file | §4.1 | `forbidden_field`/`unknown_field`/`file_field_unsupported` | ✅ `TestPushFieldRejections` |
| S-PUSH-4 | Stale base, non-overlapping fields → `ok`, `merged:true` | §8.2 | Applied merge | ✅ `TestPushStaleBaseNonOverlappingMerge` |
| S-PUSH-5 | Stale base, overlapping fields under `serverWins` / `clientWins` / `reject` | §8.3 | `conflict/field_conflict` / applied + `conflictId` / `conflict` | ✅ `TestPushStaleBaseOverlap{ServerWins,ClientWins,Reject}` |
| S-PUSH-6 | Delete under `deleteWins` (stale applies) and `editWins` (stale → `conflict/edited_since`) | §8.4 | Policy outcomes | ✅ `TestPushDeleteWinsStaleBase`, `TestPushEditWinsStaleBase` |
| S-PUSH-7 | create with an existing own record id → `conflict/exists`; with another user's or a deleted id → `rejected/id_unavailable` | §6.3 | Indistinguishable for foreign/deleted | ✅ `TestPushCreateExistsSameUser`, `TestPushCreateIDUnavailable` |
| S-PUSH-8 | update after delete → `conflict/deleted`; update of never-existing → `rejected/not_found`; delete of missing → `ok` | §6.4 | Correct codes | ✅ `TestPushUpdateAfterDeleteConflictDeleted`, `TestPushUpdateNeverExistingNotFound`, `TestPushDeleteMissingRecordOK` |
| S-PUSH-9 | Later same-record mutation after a non-`ok` result → `rejected/dependency_failed`, not applied | §6.2.3 | Held | ✅ `TestPushDependencyFailedSameRecord` |
| S-PUSH-10 | Ordering + per-mutation isolation (one fails, earlier ok, later different-record still applied) | §6.2 | Independent per-mutation transactions | ✅ `TestPushBatchOrderingAndIsolation` |
| S-PUSH-11 | `unique_violation` and `validation_failed` mapping | §12.2 | Correct rejected codes | ✅ `TestPushUniqueViolation`, `TestPushRequiredValidationFailed` |
| S-PUSH-12 | Request-level errors: protocol header, 401, malformed JSON, empty batch, over `maxBatch`, over `maxBodyBytes` | §6.1, §12.1 | Correct status/code | ✅ `TestPushRequestErrors` |
| S-PUSH-13 | Cross-user update/delete behave as missing (no existence oracle); target untouched | §14.5 | `not_found`/`ok`; other record unchanged | ✅ `TestPushCrossUserNoOracle`, `TestSecurity{Update,Delete}NoExistenceOracle` |
| S-PUSH-14 | Forged `owner`/`id`/`created`/`updated` never applied | §4.1, §14.2 | Rejected; no row created | ✅ `TestSecurityForgedServerFieldsNeverApplied` |

## 6. Server — idempotency

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| S-IDEM-1 | Same mutation id + same payload → `duplicate` carrying the original result; no re-apply | §9 | `status:"duplicate"` + stored fields | ✅ `TestPushDuplicateMutationIDSamePayload` |
| S-IDEM-2 | Same mutation id + different payload → `rejected/mutation_id_reuse`; original preserved | §9 | Rejected; stored result unchanged | ✅ `TestPushMutationIDReuseDifferentPayload` |
| S-IDEM-3 | Final rejected results are stored and replay as `duplicate` | §9 | Idempotent replay | ✅ `TestPushRejectedResultIsStored` |

## 7. Server — retention & pruning

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| S-PRUNE-1 | `Prune` deletes only rows older than retention; keeps recent; idempotent | §13 | Bounded deletes | ✅ `TestPruneIdempotentKeepsRecent` |
| S-PRUNE-2 | A cursor valid before a prune → 410 after; a cursor at the retained head still works; `changedUnion` reports incomplete history | §13, §8.2.3 | 410 + conservative merge | ✅ `TestPruneMakesOldCursor410` |
| S-PRUNE-3 | `sync_processed` pruned by the idempotency window | §9, §13 | Old entries removed | ✅ `TestPruneProcessedRespectsIdempotencyWindow` |
| S-PRUNE-4 | `created = ''` rows repaired by the migration backfill | §13 | No row is deleted by the `''` trap | ✗ **gap** (same as S-MIG-3) |

## 8. Server — security

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| S-SEC-1 | Cross-user update/delete: no existence oracle, target unchanged | §14.5 | `not_found` / `ok` | ✅ `TestSecurityUpdateNoExistenceOracle`, `TestSecurityDeleteNoExistenceOracle` |
| S-SEC-2 | ID squatting: another user's live id and a deleted id are byte-indistinguishable | §6.3, §14.5 | `id_unavailable` | ✅ `TestSecurityCreateIDSquattingIndistinguishable` |
| S-SEC-3 | Forged server-authoritative fields never applied | §4.1, §14.2 | `forbidden_field`; no row | ✅ `TestSecurityForgedServerFieldsNeverApplied` |
| S-SEC-4 | Pull isolation with both users active | §14.3 | Only own records | ✅ `TestSecurityPullIsolationWithBothUsers` |
| S-SEC-5 | Error codes: `unique_violation`, `validation_failed` | §12.2 | Correct codes | ✅ `TestSecurityUniqueAndValidationCodes` |
| S-SEC-6 | Raw SQL never interpolates client input (only config identifiers / binds) | §14 | No injection surface | ◐ audited in review, no test |

## 9. Server — conformance & convergence

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| S-CONF-1 | Every fixture is well-formed and internally consistent | §15 | 0 validator errors | ✅ `TestConformanceFixturesAreWellFormed` |
| S-CONV-1 | Randomized multi-device convergence (offline buffering, duplicate delivery), 8 seeds | §11 | Every device equals server at quiescence | ✅ `TestConvergenceRandomized` |
| S-CONV-2 | No acknowledged write is lost across a user's devices | §11 | Other device converges | ✅ `TestNoLostAcknowledgedWrite` |
| S-CONV-3 | Duplicate delivery is idempotent | §6.6, §9 | One record, rev 1 | ✅ `TestDuplicateDeliveryIsIdempotent` |
| S-CONV-4 | `410` resync preserves the outbox and converges | §11.5 | Divergence-free | ✅ `TestResyncOn410AfterPrune` |
| S-CONV-5 | Delete-vs-edit under `editWins` keeps the edited record | §8.4 | Record intact everywhere | ✅ `TestDeleteVsEditEditWins` |
| S-CONV-6 | Same-record concurrency under `clientWins` (applied + `conflictId`) converges | §8.3 | All devices converge | ✗ **gap** (only serverWins is in the randomized run) |
| S-CONV-7 | Three+ devices all edit one record in the same window under each policy | §8.3 | No lost/duplicated updates beyond policy | ◐ covered only probabilistically by S-CONV-1 |
| S-CONV-8 | **Execute every fixture** (not just validate it) against the server | §15 | Fixture `expect` satisfied | ✗ **gap** (M6 harness replaced this; driver unwritten) |

---

## 10. Client core — outbox & apply

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| C-OUT-1 | Coalesce consecutive updates: union of fields, earliest `baseRev`, stable id | §11.1 | One entry | ✅ `outbox coalescing › merges consecutive updates` |
| C-OUT-2 | create + update collapses to create (no `baseRev`) | §11.1 | One create | ✅ `… collapses create + update` |
| C-OUT-3 | create + delete cancels entirely | §11.1 | No entry | ✅ `… cancels create + delete` |
| C-OUT-4 | A mutation already `sent` is not coalesced | §11.1 | Separate entry | ✅ `… does not coalesce a mutation that was already sent` |
| C-OUT-5 | Coalesced result persists through one transaction; insertion order preserved | §11.1 | Ordered, atomic | ✅ `enqueue › persists coalesced result…` |
| C-APP-1 | Apply-by-rev: stale/duplicate upsert ignored, newer applied | §11.3 | Only `rev > localRev` | ✅ `apply-by-rev idempotency` |
| C-APP-2 | Delete of an unknown record applies; an older delete is ignored | §11.3 | Idempotent delete | ✅ `apply-by-rev idempotency` |
| C-ATOM-1 | Whole pull page + cursor persisted in one transaction (rollback on failure) | §11.3 | No partial apply | ✅ `page apply + cursor persist atomicity` |
| C-REB-1 | Rebase: a pending local edit survives a pulled server change; its `baseRev` is updated | §11.3 | Edit preserved | ✅ `rebase-on-pull` |
| C-RES-1 | Full resync preserves the outbox, keeps pending creates, drops unreferenced rows | §11.5 | Correct local set | ✅ `full resync with pending mutations` |
| C-RES-2 | A **resumed** snapshot does not delete previously delivered records | §11.5 | No data loss | ✅ `snapshot cleanup only runs for a fresh snapshot` |

## 11. Client core — engine loop / push results / lifecycle

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| C-PUSH-1 | `ok` stores the returned record+rev and clears the entry | §11.2 | Local = server | ✅ `push result handling › ok` |
| C-PUSH-2 | `conflict` replaces local state, records the conflict, drops the entry | §11.4 | Conflict persisted | ✅ `push result handling › conflict` |
| C-PUSH-3 | `rejected` marks errored and holds later same-record mutations | §11.2 | Held, not pushed | ✅ `push result handling › rejected` |
| C-PUSH-4 | A `sent` (unacknowledged) create blocks later same-record edits → never pushes `baseRev: 0` | §8.2, §11.1 | Create first, then rebased edit | ✅ `a sent create blocks later same-record edits` |
| C-PUSH-5 | `duplicate` carrying a conflict code → conflict path; other code → errored; no code → treat as ok | §6.5, §9 | Correct branch | ✗ **gap** |
| C-PUSH-6 | Unknown mutation status → errored (not dropped) | §2 | Flagged | ✗ **gap** |
| C-PUSH-7 | Batch larger than `maxBatch` is split and the remainder is sent | §11.6 | All entries eventually sent | ✗ **gap** |
| C-PUSH-8 | Pull loop follows `hasMore` across multiple pages | §11.3 | Reaches head | ◐ single-page in current tests |
| C-LIFE-1 | Single-flight: concurrent `syncNow()` share one loop | §11.6 | One push/pull sequence | ✗ **gap** |
| C-LIFE-2 | A `410` during the loop triggers auto-resync (not just explicit `resync()`) | §11.5 | Recovers without host action | ✗ **gap** |
| C-LIFE-3 | `401` pauses without dropping the outbox | §11.2 | Outbox intact, surfaced error | ✗ **gap** |
| C-LIFE-4 | Backoff is exponential with jitter and capped | §11.2 | Bounded delay | ✗ **gap** |
| C-LIFE-5 | `onForeground` / `onConnectivity` / `onNudge` funnel into `syncNow` | §9.6, §10 | A sync runs | ✗ **gap** |
| C-LIFE-6 | `status()` reports state, pending count, last error, conflicts | — | Correct shape | ✗ **gap** |

## 12. Client — PocketBase transport

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| C-TR-1 | Push = `POST /api/sync/push` with `X-Sync-Protocol`, `Authorization`, JSON body; pull = GET with `cursor`/`limit`, omitting a null cursor | §2, §6, §7 | Request shape | ✅ `request shape` |
| C-TR-2 | No double-`Bearer` when the token already has a scheme | §14.8 | Single prefix | ✅ `does not double-prefix…` |
| C-TR-3 | Decode push results / pull changes / `/info` | §5–§7 | Parsed | ✅ `decodes …` |
| C-TR-4 | Error mapping: `410`/`400`/`401`/`429(+Retry-After)`/`503`, non-JSON body, network failure → no-status offline error | §12.1 | `TransportError` with status/code | ✅ `error mapping` |
| C-TR-5 | Nudge: injected source + default SSE reader fire the callback; unsubscribe works | §10 | Callback fired | ✅ `nudge` |
| C-TR-6 | A nudge is never authoritative (no data applied from it) | §10 | Only triggers a pull | ◐ by construction |

## 13. Client — adapter & schema mapping

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| C-AD-1 | CRUD; `list`/`collections` reflect stored rows; delete removes | §9.3 | Consistent reads | ✅ `SqliteStorageAdapter CRUD` |
| C-AD-2 | Transaction rolls back every write on throw | §11.3 | Atomicity | ✅ `transaction atomicity` |
| C-AD-3 | Outbox order + coalescing through the adapter; conflicts/sync state stored | §9.3 | Persisted | ✅ `outbox / conflicts / sync state` |
| C-AD-4 | `put` passthrough: absent key unchanged; `null` clears | §4.1 | Field semantics | ✅ `put passthrough rules` |
| C-AD-5 | Engine push-ok / rebase / page+cursor atomicity through the real adapter | §11 | Integration green | ✅ `engine integration` |
| C-AD-6 | Schema creation is idempotent across reopens | §9.3 | No error | ✗ **gap** |
| C-AD-7 | `_outbox` ordering survives a reopen (no clock dependency) | §11.6 | Stable order | ✗ **gap** |
| C-MAP-1 | Column type per field type | §9.4 | text/email/url/editor→TEXT, number→REAL, bool→INTEGER, date→TEXT, select/relation→TEXT, json/geoPoint/file→TEXT JSON | ✅ `PDD §9.4 column types` |
| C-MAP-2 | Round-trip server→local→server for every type incl. `null` and undeclared keys | §9.4 | Bijective | ✅ `round-trip …` |
| C-MAP-3 | JSON-backed field detection (json/geoPoint/file/multi-select/multi-relation) | §9.4 | Flagged | ✅ `flags JSON-backed fields` |
| C-MAP-4 | Contact with a real `expo-sqlite` binding | §9.4 | Works on device | ✗ **gap** (device-only; binding behind a driver) |

## 14. Client — conformance

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| C-CONF-1 | Every fixture is well-formed and internally consistent (TS mirror of S-CONF-1) | §15 | 0 validator errors | ✅ `conformance fixtures` |
| C-CONF-2 | TS engine replay of the fixtures against a mock server | §15 | `expect` satisfied | ✗ **gap** |

---

## 15. End-to-end / cross-language

| ID | Scenario | Ref | Expected | Coverage |
|---|---|---|---|---|
| E2E-1 | The **same** 24 fixtures executed by a Go driver against a live server | §15 | Both sides agree with the contract | ✗ **gap** |
| E2E-2 | The TS engine driven by the fixtures against a server-equivalent mock | §15 | Client agrees with the contract | ✗ **gap** |
| E2E-3 | Real TS engine over HTTP against the real Go plugin (auth, push, pull, nudge end-to-end) | — | Works as a whole | ✗ **gap** |
| E2E-4 | Expo example app on a device/simulator (`examples/expo-notes`, M7) | — | Runs offline→online | ✗ **gap** (M7 unbuilt) |

---

## Validation run — results (2026-10-15)

Executed by a file-disjoint agent team (one member per area), then verified by
the lead. **No product bugs found; no source changes were needed** — every gap
was a missing test, not a defect.

**Gaps closed** (new tests; symbol prefixes `SrvScen` / `driver` for Go):

| Scenario | Test |
|---|---|
| S-HOOK-3 no-op update | `TestSrvScenNoOpUpdate` |
| S-MIG-3 / S-PRUNE-4 upgrade path | `TestSrvScenUpgradeAddChangeLogCreated` |
| S-PULL-6 snapshot stability mid-snapshot | `TestSrvScenSnapshotStabilityMidSnapshot` |
| S-INFO-4 protocol header on `/push` + `/pull` | `TestSrvScenProtocolHeaderEnforced` |
| S-CONV-6 `clientWins` convergence | `TestConvergenceRandomizedClientWins`, `TestClientWinsConvergence` |
| S-CONV-8 / E2E-1 execute the fixtures | `TestFixtureDriverExecutesAllFixtures` — **24/24 fixtures execute and pass** |
| C-PUSH-5/6/7/8 duplicate branches, unknown status, batch split, multi-page pull | `engine-push-status.test.ts` |
| C-LIFE-1..6 single-flight, auto-resync on 410, 401 pause, backoff, lifecycle hooks, status | `engine-lifecycle.test.ts` |
| C-CONF-2 replay all 24 fixtures against a mock server | `conformance-replay.test.ts` |
| C-AD-6 / C-AD-7 adapter reopen: schema idempotency, outbox order | `adapter-reopen.test.ts` |
| C-TR: non-null cursor sent; scheme-less token gets `Bearer`; non-JSON body; `Retry-After` numeric + HTTP-date | `transport-cases.test.ts` |

**Final results:** server `gofmt`/`go build`/`go vet` clean, `go test ./...` ok
(24 fixtures driven, 0 skipped); `client/core` 57 tests · `transport-pocketbase`
20 · `adapter-expo-sqlite` 70 — all typecheck clean.

**Still open** (unchanged by this run):

- **E2E-3** real TS engine over HTTP against the Go plugin — needs both sides
  running together.
- **E2E-4 / C-MAP-4** the Expo example app and the real `expo-sqlite` binding on a
  device/simulator.
- **S-CFG-2 / S-MIG-2 / S-SEC-6 / C-TR-6** remain ◐ (partially covered by
  construction or by existing assertions).
- **Driver field-type coverage:** `fixture_driver_test.go` supports the field
  types the fixtures use (`text`/`editor`/`file`) and fails loudly on others — a
  future fixture with a new type needs a one-line addition to `drvBuildField`.

## Prioritized gaps to close next

1. **E2E-3** — wire the client packages into an end-to-end run against the real
   server (the client packages are `file:`-linked; a small harness can start the
   Go app and drive `PocketBaseTransport` over HTTP).
2. **E2E-4 / C-MAP-4** — the M7 example app and the on-device `expo-sqlite`
   binding.
3. Optional hardening: broaden the fixture driver's field-type table; add a
   `sync_meta`-pruning scenario if that ships.
