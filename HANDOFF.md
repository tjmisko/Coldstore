# Handoff — coldstore closeout

Read these before changing anything:

1. `STATUS.md` — verified repository, test, blocker and production state.
2. `TASKLIST.md` — authoritative ordered work and completion state.
3. `docs/interface-refresh.md` — interface, preview, access and status-CLI decisions.
4. `docs/keymap-design.md` — keyboard grammar and interaction contract.
5. `docs/tagging-design.md` — verified metadata findings and tagging/rename plan.
6. `docs/access-and-export-interview.md` — owner answers for Tailscale, auth and XMP.
7. `onboarding.md` — Coldstore-only Tailscale Serve and co-owner onboarding runbook.

PR #16 is merged into `main` at `5e1dc67`; post-deployment documentation continues on
`main`. Start future implementation work from updated `main` on a new branch. The keyboard,
documents and tagging-design branches are already integrated; do not merge them again.

## Immediate pickup

1. Confirm `git status --short --branch`, update `main`, and read the two authoritative files
   above.
2. PR #16 merged with all six CI jobs green, including the corrected frontend regression,
   embedded-UI Playwright and two-device containerized Syncthing E2E jobs.
3. GitHub #23 tracks updating every stale issue body from planned tagging/rename migrations
   6/7 to the shipped 7/8 numbering, closing deployed issues #1–#14 and closing tracker #15.
4. Tagging implementation issues #1–#14 are deployed. XMP export is deferred in #19; the
   remaining access, performance and operations follow-ups are #17, #18 and #20–#22.
5. Production runs merged `main` at schema 8 behind loopback-only Tailscale Serve identity
   authentication. Basic credentials are inactive and Funnel is disabled. Both exact-login
   allowlisted users can access Coldstore; an unallowlisted third identity was denied in the
   owner's production test.
6. The second user is a non-admin tailnet member, not a machine-share recipient. A named
   Coldstore access group grants that identity only TCP 443 to the server; the owner's broad
   grant remains restricted to the tailnet owner. Do not change this back to
   `autogroup:shared` unless the account is first moved to a separate receiving tailnet.
7. Treat every future production/deployment checkbox as owner-mediated. The completed rollout
   does not grant continuing host or restart authority.

The current follow-up issue map is:

| Issue | Scope |
|---|---|
| #17 | Remaining cross-platform, off-LAN and network-boundary acceptance |
| #18 | PDF thumbnail benchmark on the N40L |
| #19 | Deferred XMP metadata export |
| #20 | Mount, snapshot, quota and temp-file safety gates |
| #21 | Syncthing upgrade and file-versioning decision |
| #22 | Deferred public/guest session authentication |
| #23 | Migration-number and deployed-tracker cleanup |

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
- Production remains loopback-only behind private Tailscale Serve HTTPS. Public guest access
  is deferred; do not enable Funnel or expose the raw listener.
- The second co-owner is a non-admin member with a narrow Coldstore group grant. Tailnet
  membership alone is not authorization: preserve both the TCP 443 grant and Coldstore's
  exact-login allowlist.
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
from running here, but GitHub Actions passed the corrected frontend tier, Playwright and live
Syncthing E2E on PR #16 at `16a7bf6`.

## Branch map

| Branch | State |
|---|---|
| `main` | PR #16 merge `5e1dc67`; deployed in production at schema 8. |
| `feat/interface-refresh` | Merged source branch; do not continue new work here. |
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
