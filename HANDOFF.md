# Handoff — coldstore closeout

Read these before changing anything:

1. `STATUS.md` — verified repository, test, blocker and production state.
2. `TASKLIST.md` — authoritative ordered work and completion state.
3. `docs/interface-refresh.md` — interface, preview, access and status-CLI decisions.
4. `docs/keymap-design.md` — keyboard grammar and interaction contract.
5. `docs/tagging-design.md` — verified metadata findings and tagging/rename plan.
6. `docs/access-and-export-interview.md` — owner answers for Tailscale, auth and XMP.
7. `onboarding.md` — Coldstore-only Tailscale Serve and co-owner onboarding runbook.

Work in `.worktrees/feat/interface-refresh` on `feat/interface-refresh`. The keyboard,
documents and tagging-design branches are already integrated; do not merge them again.

## Immediate pickup

1. Confirm `git status --short --branch` and read the two authoritative files above.
2. Keep draft PR #16 green. Its first GitHub Actions run passed both the embedded-UI
   Playwright job and the two-device containerized Syncthing E2E job, but exposed a Gallery
   pending-context-motion race. The current branch fixes that replay lifecycle and passes
   the exact regression 20 consecutive times plus the complete 334-test frontend suite.
3. When GitHub connectivity exists, update every issue body that still calls tagging schema
   migration 6 or rename migration 7. The landed plan uses 7 and 8; issue-body edits are the
   only unfinished part of `TASKLIST.md` item 8.
4. Tagging implementation issues #1–#14 are complete locally. The unfiled XMP export
   follow-up, GitHub tracker bookkeeping and external verification/deployment gates remain.
5. Tailscale Serve HTTPS is operational and the owner verified it through the temporary
   production Basic Auth gate. The selected application gate is now an exact two-login
   Tailscale identity allowlist, with separately revocable co-owners and full existing
   mutating UI/API access. The implementation is on this branch; keep the backend loopback,
   never enable Funnel, and follow `onboarding.md` for the owner-mediated build/config/restart
   and second-account machine share. Basic Auth must remain until the new binary is deployed.
6. Treat every production/deployment checkbox as owner-mediated. No host access or rollout
   was authorized during this integration.

## Integrated branch

| Slice | State |
|---|---|
| Keyboard lifecycle and z-index contract | Landed in `7abb04e`; 20 consecutive focused runs pass. |
| Folder-label identity guard (#13) | Landed in `debf77b`; startup and `--check` reject relabeling without writes. |
| Document previews | Two-parent merge `752c210`; schema 6 belongs solely to `detected_type`. |
| Combined token/readers documentation sweep | `9c7e5d4` through `72914b0`. |
| Finder preview/actions and Files reveal | Landed in `41e8c00`. |
| Read-only archive health CLI | Landed in `97c69e0`; `--status [--json]`, exit 0/1/2. |
| Tagging design | Two-parent merge `9b5be84`; planned migrations renumbered 7/8 in `f14af62`. |
| Acceptance and recovery docs | Reconciled through `4f029d1`. |
| Metadata and tag foundation | Issues #1-#8 through `56c1364`: migration 7, extraction, tag CRUD, capture/location and descriptions. |
| Live Photo, search and mouse tagging | Issues #6/#9/#10/#12 through `45c2a05`; safety audit fixes in `1847a4d`. |
| Durable rename | Issue #14 in `22297b2`, hardened against overwrite, partial-recovery, scanner and symlink races in `356aa82`. |
| Keyboard tagging | Issue #11 in `d9a72af`; nine adversarial acceptance fixes in `0ed5572`. |

## Production boundary

- The owner explicitly does not authorize SSH or other direct host access. Ask for specific
  command output; do not probe the host indirectly.
- Do not deploy, restart the service, migrate the production catalog, change budgets or run
  archive/restore operations without separate approval.
- Production remains loopback-only behind an SSH tunnel. A permanent route must work in an
  ordinary browser, accommodate guests and keep the backend private.
- Take a fresh SQLite `.backup` immediately before any production schema upgrade.
- The owner already confirmed one real archive propagated deletion through Syncthing and a
  restore succeeded. Do not repeat destructive production checks for ceremony.

## Non-negotiable invariants

- One SQLite connection/writer and one shared `daemon.Mover`.
- Claim a row with compare-and-swap before moving bytes.
- Startup recovery completes before concurrent actors exist.
- Never overwrite or truncate a destination. Preserve ambiguous bytes and log an event.
- Folder labels are permanent identities; migration 7's editable `display_name` is the
  cosmetic alternative.
- A status/check surface must not migrate or reconcile the catalog for writes.
- Preserve frontend `data-*` attributes; tests treat them as API.
- Keep one generated keymap source, one window dispatcher, tokenized layers and zero ESLint
  warnings.
- Assign migration versions in landing order; never reuse a shipped number.

## Verification boundary

The integrated branch passed lint, 31 files / 334 Vitest tests, 20 consecutive runs of the
eight keyboard/tagging adversarial files (147 tests each), 20 consecutive runs of the
pending-context-motion regression, vet, all Go tests, the full internal race suite,
in-process E2E and a final build. That build measured 3,624 KiB for `web/dist` and 21,483,024
bytes for the binary.

The fresh checkout needed `npm_config_ignore_scripts=true` because this sandbox denies
esbuild's npm postinstall self-exec; the target still performed its own `npm ci` and built
the real bundle and binary. Local loopback restrictions still prevent those two harnesses
from running here, but GitHub Actions passed both Playwright and live Syncthing E2E on PR
#16 at `b7f8910`.

## Branch map

| Branch | State |
|---|---|
| `main` | Local baseline `9bcb7e8`; production/upstream remains `37ad497`. |
| `feat/interface-refresh` | Active integrated branch; all work above is committed. |
| `feat/keymap` | Merged ancestor. Do not merge again. |
| `feat/documents` | Merged by `752c210`. Do not merge again. |
| `feat/tagging-design` | Merged by `9b5be84`; its source branch retains obsolete 6/7 numbers, while the integrated document is authoritative at 7/8. |

## Where detail lives

- Current evidence and production facts: `STATUS.md`
- Ordered work and definitions of done: `TASKLIST.md`
- Global data-safety contract: `docs/control-plane-design.md` section 0
- Build and tier boundaries: `docs/build-and-test.md`
- Deployment and offline health usage: `docs/deployment.md`
- Host-specific installation records: ignored local files under `docs/`; keep them ignored
  and never copy their identifiers into tracked documentation.
