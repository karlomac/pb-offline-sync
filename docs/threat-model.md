# Threat model

Scope: the sync layer for per-user owned records. Assets are per-user record
data, the integrity of other users' records, and server availability. The
normative requirements live in `protocol.md` §14; this document explains the
threats and how each is mitigated, and what must be tested.

## Trust boundaries

- The **client is untrusted**. Everything it sends (owner, `rev`, `created`,
  `updated`, `collectionId`, field values, mutation IDs, cursors) is validated;
  server-authoritative fields are rejected, and the owner is taken only from the
  authenticated user.
- The **sync routes are custom PocketBase routes**: they bypass collection API
  rules by default, so authorization is enforced explicitly in the plugin and
  covered by tests.

## Threats and mitigations

| Threat | Mitigation | Where |
|---|---|---|
| Client writes another user's record | Verify `owner == auth.id` on every update/delete; owner never read from the request | protocol §6.2, §14.2 |
| Client sets `owner` on create | Forced to the authenticated user server-side | protocol §6.2 rule 6 |
| Forging server fields (`rev`, `created`, `updated`, `collectionId`, `expand`) | Rejected (`forbidden_field`) | protocol §4.1, §12.2 |
| Reading others' changes via pull | Pull filters by authenticated owner **at the data layer**; post-filtering is not acceptable | protocol §7.2, §14.3 |
| ID squatting / existence oracle | Create with another user's or a deleted ID → `id_unavailable`, indistinguishably; authz failures use generic messages | protocol §6.3, §6.4, §12.1 |
| Replay of old mutations | `sync_processed` idempotency keyed by `(user, mutation_id)`; differing payload → `mutation_id_reuse` | protocol §9 |
| Oversized batches / DoS | `maxBatch`, `maxPage`, `maxBodyBytes` enforced; rate limiting recommended | protocol §6.1, §13, §14.7 |
| Collection rules bypassed by custom routes | v1 evaluates ownership only and documents it; `rule_denied` reserved | protocol §14.6, §17.1.2 |
| Info leak through error text | Generic messages for authz failures; no record contents at default log levels | protocol §12.1, §14.9 |

## File fields

File content sync is a v1 non-goal. Clients receive file field values (filenames)
on pull but MUST NOT send file fields on push (`file_field_unsupported`).

## Media uploads (v1.1, protocol §18)

The media channel moves untrusted bytes; the additional threats:

| Threat | Mitigation | Where |
|---|---|---|
| Resuming/committing another user's session | Sessions carry the owner from the token; a foreign `uploadId` is `upload_not_found`, indistinguishable from a missing one | protocol §18.3 |
| Stored XSS via upload | MIME allowlist; `text/html` and `image/svg+xml` always denied; serving keeps PocketBase's `nosniff`/disposition handling | protocol §18.6 |
| Client-claimed integrity | `sha256` recomputed server-side at `commit`; mismatch → `hash_mismatch`; re-verification after download is host-side (deferred with download wiring) | protocol §18.4 |
| Cross-user content-existence oracle | Dedup keyed per user `(owner, sha256)`, never global | protocol §18.1 |
| Disk-fill via abandoned partials | Staging TTL sweep; per-user quota and max file size enforced at `init` before bytes move; `init` rate-limiting recommended | protocol §18.2, §18.6 |
| Long-lived public URLs for private media | Downloads go through PocketBase file serving with short-lived file tokens | protocol §18.4 |

## Required tests

- A **cross-user access suite**: forged owner, forged server fields, ID squatting,
  cross-user update/delete/create, and "no existence oracle" (responses for
  another user's record are indistinguishable from a nonexistent one).
- Atomicity: a rolled-back write leaves no change-log entry
  (`server/syncplugin/spike_test.go`).

## Disclosure

See `../SECURITY.md` for the reporting process. Do not open public issues for
suspected vulnerabilities.
