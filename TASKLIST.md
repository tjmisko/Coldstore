# Open tasklist — coldstore

This is the authoritative open-work queue distilled from the deployment, interface review,
parallel branches, metadata research and two-user production authorization follow-up through
2026-08-21.

Detailed designs still govern their own surfaces. This file governs ordering and completion
state. If a detailed tasklist has stale checkboxes, update it as part of the corresponding
task rather than treating the stale box as evidence.

## Completion rule

A code task is not done until its focused tests and the relevant whole-tree tiers pass, docs
and fixtures describe the behavior, and no production action was taken without explicit
owner approval. Schema-changing work additionally requires a fresh SQLite `.backup` before
deployment.

## P0 — establish one integrated green branch

### 1. Stabilize the keyboard layer

- [x] Replace the passive-effect stale-handler window in `useKeymapLayer` with a lifecycle
      that exposes the current committed handler before a visible fetched row can receive
      keyboard input.
- [x] Add a regression test that fails when a key is sent immediately after the first fetched
      rows commit.
- [x] Run `keyboard.test.tsx` repeatedly, not once. Require at least 20 consecutive clean
      runs plus one clean full `make test-web`.
- [x] Preserve the single `window` keydown listener, scope stack, text-entry suspension and
      generated binding/cheatsheet source of truth.

### 2. Repair the z-index contract before adding more overlays

- [x] Make `ToastProvider` use the central `Z.toast` value so action failures remain visible
      above palettes, preview surfaces and dialogs.
- [x] Reconcile `web/src/index.css`, `docs/interface-refresh.md` and `docs/keymap-design.md`
      with `web/src/lib/zLayers.ts`; remove obsolete literal-ladder guidance.
- [x] Add or extend an overlay/toast test that holds the intended ordering.

### 3. Merge `feat/documents` onto `feat/interface-refresh`

- [x] Merge from the interface-refresh worktree. Do not merge keymap again.
- [x] Resolve `App.tsx`: preserve current providers/state and add the document-reader idle
      prefetch.
- [x] Resolve `FileBrowser.tsx`: keep context-menu state and add preview state. Filename click,
      keyboard Open and context-menu Open must all open the preview; none may remain a no-op
      or silently become Download.
- [x] Resolve `Lightbox.tsx`: keep `Portal`, focus trap, scroll lock, `useKeymapLayer`, shared
      `FileActions`, shared `BulkActions` and operator behavior; add `resolvePreview` and
      `PreviewSurface`. Do not restore the documents branch's private window listener.
- [x] Reconcile `docs/interface-refresh.md` manually even if Git auto-merges it: retain the
      interface branch's A/C/BUG status and the documents branch's H completion/deviations.
- [x] Confirm migration 6 is exactly the documents `detected_type` migration and every older
      schema migrates to it.

### 4. Finish the combined visual sweep

- [x] Convert raw Tailwind defaults in `KeymapProvider`, `FuzzyFinder`, `ContextMenu`,
      `CommandPalette`, `Cheatsheet` and `Lightbox` to the established tokens.
- [x] Sweep the newly merged `PreviewSurface` and PDF/EPUB/text reader components too.
- [x] Preserve the rule: color communicates file state/location, never generic action.
- [x] Preserve every existing `data-*` selector.
- [x] Run the documented raw-color grep and explain any intentional residue.

### 5. Reconcile feature acceptance status

- [x] Mark D/E items complete only where implementation and tests prove them.
- [x] Rewrite D3's obsolete “Space toggles selection” wording: Space is the leader by owner
      decision; keep the implemented non-colliding selection bindings.
- [x] Implement F3: fuzzy-finder preview pane using the merged preview surface.
- [x] Implement F4: keyboard open/select/act flow from fuzzy results.
- [x] Finish G2: add reveal-folder behavior, or document the platform limitation and replace
      it with a precise supported action.
- [x] Mark H0a–H8 and BUG-3 complete only after the combined branch passes verification.
- [x] Decide H9: larger preview derivative versus full original/browser rendering.
- [ ] Complete H10: benchmark PDF thumbnail generation on the actual two-core N40L using
      owner-provided command output; agents have no host access. Tracked by GitHub #18.

### 6. Prove the integrated branch

- [x] `make lint`
- [x] `make test-web`
- [x] repeated targeted keyboard test (20 consecutive runs)
- [x] `go vet ./...`
- [x] `go test ./...`
- [x] `go test -race ./internal/...`
- [x] `make test-e2e`
- [x] `make test-playwright` against an isolated root and unused port
- [x] fresh-checkout `make build` with no preceding `npm ci`
- [x] `make test-e2e-live` with the `e2e_live` tag alone
- [x] Measure and record final frontend distribution and binary sizes: 3,624 KiB and
      21,483,024 bytes respectively after the final `0ed5572` code integration.

The local execution environment refused loopback binds even after scoped escalation, so the
browser/network tiers could not run here. GitHub Actions subsequently ran both against PR
#16: the embedded-UI Playwright job and the two-device containerized Syncthing E2E job passed
on `b7f8910`. The fresh-checkout target installed its own dependencies and built successfully
with `npm_config_ignore_scripts=true`; npm's esbuild postinstall self-check is likewise denied
by the local execution sandbox.

## P0 — integrate the tagging plan without corrupting migrations

### 7. Merge the tagging design

- [x] Merge `feat/tagging-design` after documents so the next schema number is unambiguous.
- [x] Preserve its verified/source/unverified labels and the recorded GPS-blocker retraction.
- [x] Update root documentation indexes to include interface, keymap and tagging designs.

### 8. Renumber planned migrations everywhere

- [x] Change tagging schema migration 6 to migration 7.
- [x] Change rename/job migration 7 to migration 8.
- [x] Amend all affected references in `docs/tagging-design.md`.
- [ ] Amend GitHub issues #2, #3, #7, #8, #14 and tracker #15 where they name the old
      versions; search all issue bodies rather than trusting this list to be exhaustive.
      Tracked by GitHub #23.
- [x] Keep the migration rule explicit: assign versions in landing order and never reuse a
      number already shipped on another branch.

GitHub connectivity is restored. The deferred body edits and deployed-issue closeout now live
in #23.

## P0 — live data-safety bugs

### 9. GitHub #13 — reject TOML folder-label changes

- [x] Before reconciliation writes anything, detect a declared path already catalogued under
      another label and refuse startup with a recovery-oriented error.
- [x] Make the read-only config/check path report the same problem without opening/migrating
      the catalog for writes.
- [x] Preserve legitimate path changes under an unchanged label and legitimate removal of a
      folder from TOML.
- [x] Cover swapped labels, cleaned paths/trailing slashes and no-write-on-error behavior.
- [x] Document labels as permanent identifiers and `display_name` as the cosmetic alternative.
- [x] Run Go unit and in-process E2E suites.

### 10. GitHub #6 — make Live Photos atomic

This followed metadata issues #1–#3 and schema issue #2 below and was treated as P0 because
it was a real half-archive data bug.

- [x] Pair by matching content identifier, not filename authority.
- [x] Use `ffprobe` for MOV and resident ExifTool subprocess for candidate stills.
- [x] Treat basename only as an explicitly degraded source when ExifTool is unavailable.
- [x] Expand pair groups at every existing archive and restore entry point and in quota byte
      accounting; the completed #14 rename path preserves the same group contract.
- [x] Add the mover-level `pair_split` warning backstop.
- [x] Test UUID collisions, edited `IMG_E...` variants, AAE membership, exact-one-member
      budgets, bulk job totals, restore expansion and 20,000-ID chunking.

## P1 — private cross-platform access; public guest access deferred

### 11. Prepare the selected v1 Tailscale ingress

Use `docs/access-and-export-interview.md` to record the owner choices that gate this section.

- [x] Treat the current loopback SSH tunnel as the safe interim state.
- [x] Select Tailscale Serve plus exact-login application authentication for v1 owner access.
      Public-domain, zero-install guest access is deferred until it becomes a real requirement.
- [x] Keep the coldstore listener private; do not expose raw port 8090 directly to the LAN,
      tailnet or public internet.
- [x] Add an owner-facing Tailscale Serve runbook that proxies HTTPS only to the loopback
      listener, uses separately revocable Tailscale identities, does not enable Funnel and
      includes rollback/revocation. The deployed topology uses a non-admin tailnet member in
      a named Coldstore group, with only TCP 443 to the server; machine sharing remains a
      documented alternative for a recipient who administers a separate tailnet. See
      `onboarding.md`.
- [x] Implement `auth_mode = "tailscale"` with an exact login allowlist, whole-mux protection,
      loopback-only validation, fail-closed header parsing and no Basic Auth bypass.
- [x] Reject cross-origin browser mutations in Tailscale mode and journal accepted mutations
      with login, authentication provider, method, path and response status.
- [x] Deploy the reviewed identity build, remove the temporary Basic credentials from the
      service environment, and verify direct loopback requests fail without a Serve identity.
- [x] Admit the second non-admin Tailscale identity through the narrow TCP 443 group grant and
      Coldstore exact-login allowlist; verify both allowlisted users can load Coldstore.
- [x] Verify an unallowlisted third Tailscale identity cannot access Coldstore.
- [ ] After separate owner approval, finish platform coverage from Linux and Windows where
      available, plus cellular/off-LAN coverage and the existing mutating owner API. macOS and
      iPhone access are verified. Do not expose the raw port or create a reduced remote-only
      API. Tracked by GitHub #17.
- [x] Reconcile the existing contradictory Cloudflare Tunnel guidance before choosing a
      provider or topology.
- [x] Document certificate renewal, DNS ownership, proxy trust boundaries, request-size/range
      behavior for large media and an emergency disable procedure.

### 12. Deferred — build session authentication only if public ingress later wins

These are future public/guest requirements, not v1 blockers. A later token upgrade can use
opaque, revocable server-side browser sessions and separately scoped API bearer tokens;
Tailscale identity authentication does not prevent either migration. The deferred bundle is
tracked by GitHub #22.

- [ ] Add owner password-to-opaque-session exchange with `HttpOnly`, `SameSite=Lax`, `Path=/`
      cookie, plus individually revocable and expiring read-only guest grants.
- [ ] Enforce guest authorization on every API, thumbnail, preview and download route; proxy
      method filtering and hidden frontend controls are defence in depth, not authorization.
- [ ] Keep Basic Auth accepted for CLI/runbook compatibility.
- [ ] Make `Secure` an explicit trusted configuration value; TLS terminates before Go, so
      `r.TLS` is not authoritative and arbitrary forwarded headers are not trusted.
- [ ] Add CSRF protection for non-GET operations, login rate limiting and a real logout path.
- [ ] Verify thumbnails, Range requests, downloads and new preview routes work through the
      authenticated browser session without frontend bearer-header workarounds.
- [ ] Update deployment and access runbooks after the topology is selected.

## P1 — operational status dump

Detailed acceptance criteria are in `docs/interface-refresh.md` section I.

### 13. Add a safe diagnostic CLI

- [x] Choose and document the CLI shape without breaking existing flags.
- [x] Produce a concise human health report and optional structured JSON.
- [x] Work when the daemon/HTTP listener is down.
- [x] Open the catalog read-only: no migrations, reconciliation, write probes, scan or other
      mutation.
- [x] Report build/running version, supported/on-disk schema, mount/storage health, folder
      readiness and budgets, counts, jobs, derived-data failures, warning events and last
      successful scan/offload.
- [x] Redact credentials and device identifiers by default.
- [x] Exit non-zero for load-bearing failures while separating warnings.
- [x] Test missing/degraded mount, schema mismatch, missing `.stfolder`, stale scan, wedge,
      interrupted/failed job and recent conflict/data-safety events.
- [x] Document checks that need root or a Syncthing API and therefore cannot be performed by
      the binary itself.

## P1 — tagging, metadata and rename implementation

Follow the dependency order in GitHub #15 after the migration renumbering above.

### 14. Issue sequence

- [x] #1 — pure metadata package: primary and HEIC cross-check readers, QuickTime reader,
      resident ExifTool probe, real fixtures and cancellation/deadline safety.
- [x] #2 — migration 7: tags, descriptions, capture/location fields, pairing fields and
      `display_name`.
- [x] #3 — background metadata extraction pass with its own queue, attempt budget and
      owner-edit preservation.
- [x] #6 — atomic Live Photo pairing and movement (P0 specification above).
- [x] #7 — normalized tag catalog primitives and CRUD API.
- [x] #4 — capture-date sorting/filtering/grouping with honest unknown-offset display.
- [x] #5 — location display/proximity filtering and location tagging without geocoding.
- [x] #8 — descriptions API/editor with keymap-safe text entry.
- [x] #9 — extend search to path/description/tags and add tag/untagged filters. Benchmark
      `LIKE` on representative N40L-scale data before accepting the no-FTS5 claim. The
      50,000-row ARM64 run measured 14.0–47.9 ms, below the 100 ms threshold; this was not a
      benchmark on the production N40L.
- [x] #10 — mouse-first tagging UI, typeahead prompt and tag manager.
- [x] #12 — one bulk tagging endpoint, 500-ID statement chunks and real undo.
- [x] #14 — migration 8 and durable, group-preserving directory/file rename jobs; cosmetic
      `display_name` shipped earlier in migration 7.
- [x] #11 — tag operators, quick slots and dedicated Tag mode on the existing grammar.
- [x] File separate XMP export issue #19; tracker #15 previously named it without an issue.
- [ ] Close tracker #15 only when its children and #19 are accounted for. Tracked by #23.

## P1 — deployment and operations follow-up

Agents may prepare code/docs but may not access or mutate the host. Every production step below
requires the owner.

### 15. Before the next production upgrade

- [ ] Ask the owner to confirm whether btrfs snapshots are actually scheduled; current
      evidence is contradictory. Tracked by GitHub #20.
- [x] Take a fresh SQLite `.backup` immediately before applying schema migrations.
- [x] Deploy merged `main` and migrate the production catalog from schema 5 to schema 8.
- [ ] Reconcile the optional `require_mountpoint` guard, intentionally absent from the
      production TOML, with the distinct systemd `RequiresMountsFor=` dependency; record why
      each is enabled or disabled. Tracked by GitHub #20.
- [ ] Confirm the unit still requires the real archive mount after any service-file update.
      Tracked by GitHub #20.
- [x] Confirm the deployed binary reports schema 8 and remains active after restart.
- [x] Keep `thumbnail_workers=1` unless measured memory evidence supports increasing it;
      production is configured at one worker and no contrary memory evidence exists.

### 16. Finish rollout coverage

- [ ] Run the isolated throwaway-folder test before relying on automatic quota enforcement:
      oldest-first, grace period, persistent paths, archive layout, exact restore and pinning.
      Tracked by GitHub #20.
- [ ] Verify no coldstore temp/superseded file appears as a synced item. Tracked by #20.
- [x] Production manual archive/deletion propagation/restore is already confirmed; do not
      repeat destructive tests merely for ceremony.
- [ ] Upgrade the server's Syncthing v1.19.2 through an owner-approved maintenance plan.
      Tracked by GitHub #21; reconfirm the installed version before acting.
- [ ] Decide separately whether to enable Syncthing file versioning. The owner currently
      considers backups sufficient, so this is not a release blocker. Tracked by #21.

## P2 — documentation and maintenance cleanup

### 17. Repair tracked documentation

- [x] Correct rollback prose: migrated catalogs are refused by migration-aware older binaries;
      the silent downgrade hazard applies to pre-migration binaries.
- [x] Remove the false claim that archived content can be rebuilt by scanning hot folders.
- [x] Add document preview and EPUB endpoints to the API architecture list.
- [x] Document preview readers and FileBrowser lightbox flow in the React guide.
- [x] Update the root docs index for interface/keymap/tagging documents.
- [x] Mark `docs/control-plane-design.md` historical where its old build instructions conflict
      with current `.gitkeep` and dependency rules.
- [x] Confirm the ignored stale `coldstore-demo.toml` is absent; `make demo` uses the
      generated safe loopback config.

### 18. Repair local-only deployment records

- [x] Keep `docs/first-deploy.md` and `docs/deploy-checklist.md` ignored and uncommitted.
- [x] Update their old split-build procedure: Node 24 on the server now supports `make build`.
- [x] Update PDF/EPUB dependency and thumbnail/preview behavior after documents lands.
- [x] Record actual rollout progress: initial scan plus real manual archive propagation and
      successful restore; throwaway quota test remains open.
- [ ] Record snapshot scheduling only after the owner confirms it. Tracked by #20.

## Settled or completed — do not reopen without new evidence

- [x] Identity/title/icons/PWA assets.
- [x] Dark-only visual direction and base token system.
- [x] BUG-1 touch selection and BUG-2 mobile heading overlap.
- [x] Operator-pending keymap architecture and completed keymap branch merge.
- [x] Poppler approved; `poppler-utils` and ExifTool are installed on the host.
- [x] Real iPhone HEIC removed the synthetic GPS blocker; metadata issue #1 is unblocked.
- [x] Static wired address is outside the DHCP pool.
- [x] Real manual archive propagated its deletion through Syncthing and restore works.
- [x] Production remains private behind loopback-only Tailscale Serve; Funnel and the raw
      network listener remain disabled.
- [x] Direct agent SSH/host access is not authorized.
