# Handoff — coldstore closeout

This is the short pickup document. Read these files before changing anything:

1. `STATUS.md` — verified repository, branch, test and production state.
2. `TASKLIST.md` — the authoritative ordered list of remaining work.
3. `docs/interface-refresh.md` — detailed interface/access/document-preview decisions.
4. `docs/keymap-design.md` — keyboard grammar and interaction contract.
5. `docs/tagging-design.md` on `feat/tagging-design` — authoritative tagging design;
   merge it only at the point named in `TASKLIST.md`.

The old handoff said the keymap merge was unresolved and uncommitted. That was stale.
Commit `28ba626` is the completed two-parent merge, and `feat/interface-refresh` is clean
apart from work deliberately performed after this document was read.

## Start here

Work in `.worktrees/feat/interface-refresh` on `feat/interface-refresh`.

1. Confirm `git status --short --branch` and read `STATUS.md` / `TASKLIST.md`.
2. Fix the intermittent keyboard handler race described in `STATUS.md` before merging
   another frontend branch.
3. Merge `feat/documents` **onto** `feat/interface-refresh`. Preserve the interface/keymap
   structure and graft the document reader into it; do not take either frontend side whole.
4. Complete the combined design-token and z-index sweep.
5. Run the combined verification matrix, including the live Syncthing tier.
6. Merge `feat/tagging-design`, then repair its migration numbers and corresponding GitHub
   issue text before implementing tagging.

Exact conflict guidance and test commands are in `TASKLIST.md`.

## Production boundary

- The owner explicitly does **not** authorize SSH or other direct host access. Ask the owner
  for any production observation or command output. Do not probe the host indirectly.
- Do not deploy, restart the service, alter production budgets, migrate the production
  catalog or run archive/restore operations without separate explicit approval.
- Production is intentionally loopback-only behind an SSH tunnel for now.
- The permanent access solution must work in an ordinary browser without requiring a VPN
  client and should accommodate guests. Treat public exposure of the raw coldstore port as
  rejected; domain-backed HTTPS requires the session-auth work in the tasklist.
- The owner has confirmed a real archive propagates deletion through Syncthing and that
  restore works correctly.

## Branch map

| Branch | State |
|---|---|
| `main` | Clean local baseline at `9bcb7e8`, nine commits ahead of deployed/upstream `37ad497`. |
| `feat/interface-refresh` | Active integration branch. Contains identity, design refresh, BUG-1/2 and the completed keymap merge. |
| `feat/keymap` | Done and already an ancestor of `feat/interface-refresh`. Do not merge again. |
| `feat/documents` | Done in isolation, not merged. Twelve commits from shared base `5cab271`; adds schema 6. |
| `feat/tagging-design` | Done, documentation only, not merged. Its migration numbers are stale after documents. |

All existing worktrees were clean at the status audit. Re-check rather than assuming.

## Non-negotiable implementation invariants

- One SQLite connection/writer.
- One shared `daemon.Mover`; filesystem moves and pin writes go through it.
- Claim a file row with compare-and-swap before moving bytes.
- Startup recovery finishes before concurrent actors exist.
- Never overwrite or truncate a destination. Ambiguity keeps bytes and records an event.
- TOML owns folder identity/path; SQLite owns runtime policy.
- A status/diagnostic command must open the catalog read-only and must not migrate it.
- Never remove or rename an existing frontend `data-*` attribute; tests use them as API.
- Zero ESLint warnings and no `eslint-disable` in `web/src`.

## Important hazards

1. `feat/documents` consumes migration 6 for `detected_type`; tagging currently also calls
   its schema change migration 6 and rename migration 7. They must become 7 and 8.
2. Production still runs `37ad497`, schema 2. The deployed binary predates the implemented
   `require_mountpoint` guard; its systemd `RequiresMountsFor=` is currently the sole mount
   guard until a newer binary is deployed.
3. Issue #13: changing a TOML folder label duplicates the catalog. Do not rename labels.
4. Issue #6: Live Photo members are not atomic yet and can be split by quota enforcement.
5. The documents branch has never passed `test-e2e-live`; isolated branch success is not a
   substitute for the combined live test.
6. Use an isolated Playwright root/port. A schema-6 test database cannot be reopened by an
   older branch binary.

## Where detail lives

- Current evidence and production facts: `STATUS.md`
- Ordered work and definitions of done: `TASKLIST.md`
- Verified metadata decisions and library caveats: `feat/tagging-design:docs/tagging-design.md`
- Global data-safety contract: `docs/control-plane-design.md` section 0
- Host-specific installation records: ignored local files under `docs/`; never commit their
  identifiers or copy them into tracked documentation.
