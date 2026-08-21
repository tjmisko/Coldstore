# Handoff — coldstore closeout

Read these before changing anything:

1. `STATUS.md` — verified repository, test, blocker and production state.
2. `TASKLIST.md` — authoritative ordered work and completion state.
3. `docs/interface-refresh.md` — interface, preview, access and status-CLI decisions.
4. `docs/keymap-design.md` — keyboard grammar and interaction contract.
5. `docs/tagging-design.md` — verified metadata findings and tagging/rename plan.

Work in `.worktrees/feat/interface-refresh` on `feat/interface-refresh`. The keyboard,
documents and tagging-design branches are already integrated; do not merge them again.

## Immediate pickup

1. Confirm `git status --short --branch` and read the two authoritative files above.
2. Run Playwright and `make test-e2e-live` on an execution host that permits loopback binds.
   Both were attempted normally and with scoped escalation here, but failed before their
   servers could listen. Do not report them as code failures or as passed.
3. When GitHub connectivity exists, update every issue body that still calls tagging schema
   migration 6 or rename migration 7. The landed plan uses 7 and 8; issue-body edits are the
   only unfinished part of `TASKLIST.md` item 8.
4. Follow `docs/tagging-design.md` section 8 for implementation order. Metadata issues #1–#3
   are complete, so the Live Photo atomicity fix in #6 can now land.
5. Leave ingress/session-auth work at its decision gate until the owner selects a domain,
   proxy and trust boundary. Do not expose the raw control-plane listener.
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
- Folder labels are permanent identities; `display_name` is the planned cosmetic field.
- A status/check surface must not migrate or reconcile the catalog for writes.
- Preserve frontend `data-*` attributes; tests treat them as API.
- Keep one generated keymap source, one window dispatcher, tokenized layers and zero ESLint
  warnings.
- Assign migration versions in landing order; never reuse a shipped number.

## Verification boundary

The integrated branch passed lint, all 260 Vitest tests, 20 consecutive keyboard runs, vet,
all Go tests, the full internal race suite, in-process E2E and a detached fresh-checkout
build. The measured outputs were 3,568 KiB for `web/dist` and 18,187,688 bytes for the
binary.

The fresh checkout needed `npm_config_ignore_scripts=true` because this sandbox denies
esbuild's npm postinstall self-exec; the target still performed its own `npm ci` and built
the real bundle and binary. Playwright and live E2E remain open solely because loopback bind
is unavailable here.

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
