# Coldstore E2E Testing — Interview

Answer inline or delete options that don't apply. Leave blank if unsure.

---

## 1. Scope & Goals

**What do you consider "end to end" for this project?**
<!-- Which layers should the tests exercise together? -->
- [x] API → Catalog → Filesystem (no daemon loop)
- [x] Full daemon lifecycle (scan → offload → restore) driven via API
- [x] Frontend → API → Catalog (Playwright browser tests)
- [x] All of the above

**What's the primary goal of these tests?**
- [ ] Catch regressions in the offload/restore pipeline
- [x] Validate the API contract for frontend development
- [x] Confidence before deploying to my actual server
- [ ] Other: ___

---

## 2. Test Environment

**Should tests run against a real Syncthing instance or mock it out?**
- [ ] No Syncthing — just use temp dirs with fake folder configs
- [x] Real Syncthing in a container (docker-compose)
- [ ] Other: ___

**Should tests use a real cold storage directory (tmpdir) or something else?**
- [ ] `t.TempDir()` for everything (current unit test pattern)
- [x] A dedicated test fixtures directory checked into the repo
    - [ ] Other: Should spin up a docker container to test this, with all infrastructure and setup handled there. Can create white noise images.

**Should e2e tests start a live HTTP server (`httptest.Server`) or use the recorder pattern?**
- [ ] Live server on a random port (true e2e)
- [ ] Recorder (already used in unit tests, faster)
- [x] Both — recorders for fast CI, live server for full e2e
- [ ] Other: ___

---

## 3. Daemon Lifecycle Tests

**Which daemon behaviors matter most to test end-to-end?**
Rate 1-3 (1 = must have, 2 = nice to have, 3 = skip):

- [1] 1 Scan detects new files and populates catalog
- [1] 1 Quota enforcement offloads oldest files first
- [1] 1 Grace period prevents offloading recent files
- [1] 1 Persistent path patterns are respected during offload
- [1] 1 Restore moves file back and updates catalog status
- [1] 1 Crash recovery (`archiving` state on startup)
- [1] 1 Cross-device move fallback (copy + hash verify)
- [1] 1 Cold storage free-space check pauses offloading
- [1] 1 Thumbnail generation for newly scanned files
- [1] 1 `atime` tracking influences offload ordering

---

## 4. API Contract Tests

**Which API behaviors need e2e coverage beyond the existing handler tests?**

- [x] 1 `GET /files` pagination (offset/limit) with real data
- [x] 1 `GET /files` sorting (mtime/size/name)
- [x] 1 `GET /files/{id}/download` for archived files (served from cold storage)
- [x] 1 `POST /files/{id}/restore` full round-trip (API → filesystem → catalog)
- [x] 1 `GET /thumbnails/` serves correct image bytes
- [x] 1 `GET /status` reflects actual folder sizes after offload
- [x] 1 Basic auth middleware (enabled vs disabled)
- [x] 2 Folder config CRUD lifecycle
- [x] 2 Persistent path CRUD lifecycle
- [x] 2 Error cases: 404 on missing file, 409 on restoring file, etc.

---

## 5. Frontend / Browser Tests

**Do you want Playwright browser tests?**
- [ ] Yes — cover core user flows (browse, view, restore)
- [ ] No — API-level tests are sufficient for now
- [x] Later — stub it out but don't implement yet

**If yes, which flows?**
- [x] Gallery view loads thumbnails for an image folder
- [x] File browser lists files with correct status badges
- [x] Clicking restore triggers API call and updates UI
- [x] Sidebar navigation between folders and views
- [x] Settings panel saves config changes
- [ ] Other: ___

---

## 6. Test Organization

**Where should e2e tests live?**
- [x] `tests/e2e/` at the repo root (separate Go test package)
- [ ] `internal/e2e/` as an internal package
- [ ] `e2e_test.go` files alongside existing packages
- [ ] Other: ___

**How should e2e tests be gated?**
- [ ] Build tag (`//go:build e2e`) — run with `go test -tags e2e ./...`
- [ ] Separate `make e2e` target
- [ ] `-short` flag (skip e2e when `-short` is set)
- [ ] Other: brainstorm this with docker guidacne from above

**Should e2e tests be part of CI?**
- [ ] Yes, every push
- [x] Yes, but only on PRs / main
- [ ] No, local only for now
- [x] Will spinning up docker on GitHub Actions complicate?

---

## 7. Test Data & Fixtures

**What file types should test fixtures include?**
- [x] Small JPEG/PNG images (for thumbnail pipeline)
- [x] A short video clip (for video thumbnail)
- [x] HEIF files (requires ffmpeg)
- [x] Plain text / generic files (for file browser tests)
- [x] PDF Files
- [x] EPUB Files
- [ ] Other: ___

**How many files / folders should a typical e2e scenario create?**
- [ ] Minimal (5-10 files, 1-2 folders) — fast, focused
- [ ] Medium (50-100 files, 3-4 folders) — realistic pagination
- [x] Large (1000+ files) — stress / performance
- [ ] Other: ___

---

## 8. Configuration Testing

**Which config edge cases should be covered?**
- [x] Multiple folders with different budgets
- [x] Budget of `0` (offload everything immediately)
- [x] Overlapping persistent patterns across folders
- [ ] Changing budget mid-run (reconfigure and re-enforce)
- [x] Invalid config (missing required fields, bad budget string)
- [ ] Auth enabled vs disabled
- [ ] Other: ___

---

## 9. Anything Else

**Any specific scenarios you've hit in practice that you want tested?**


**Any constraints on test runtime? (e.g., "must finish in under 30s")**


**Anything you explicitly do NOT want tested?**

