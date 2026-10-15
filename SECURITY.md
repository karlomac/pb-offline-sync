# Security Policy

## Reporting a vulnerability

Do **not** open a public issue for a suspected vulnerability. Report privately,
by email to the maintainer address listed on the repository, or via GitHub's
private vulnerability reporting on the repository's **Security** tab.

Please include: affected version(s)/commit, a description, and a minimal
reproduction or proof of concept. We aim to acknowledge within a few business
days and to provide a remediation timeline after triage.

## Scope

In scope: the server plugin (`server/`), the client engine (`client/`), and the
sync protocol as implemented. The highest-risk areas are ownership enforcement,
the pull owner-isolation filter, and change-log/transaction atomicity — see
`docs/threat-model.md`.

Especially relevant classes:

- Cross-user read or write of records (ownership bypass, existence oracle).
- Trusting client-supplied server-authoritative fields.
- Injection or bypass through the custom `/api/sync/*` routes.

## Disclosure

We will coordinate a fix and a release, and credit reporters who wish to be
credited, before any public write-up.
