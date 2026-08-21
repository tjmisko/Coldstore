# Go Patterns in Coldstore

An annotated tour of Go idioms as they appear in this codebase.
Each section explains a pattern, why we use it, and where to find it.

---

## 1. `//go:embed` + `fs.Sub` + SPA Fallback

**What:** The `//go:embed` directive bakes the entire React build output into the
Go binary at compile time.  `fs.Sub` strips the leading `dist/` prefix so the
file server sees clean paths, and a custom handler implements SPA fallback --
any path that doesn't match a real file gets `index.html`.

**Why:** A single binary with zero external file dependencies is trivial to
deploy.  The SPA fallback means React Router can own client-side routes
(`/folders/photos`) without the server returning a 404.

**Where:** `internal/api/static.go:10-41`

```go
//go:embed all:dist
var distFS embed.FS

func staticHandler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("embedded dist not found: " + err.Error())
	}

	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Try to serve the file directly
		if path != "/" && !strings.HasPrefix(path, "/api/") {
			if f, err := sub.Open(strings.TrimPrefix(path, "/")); err == nil {
				f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// SPA fallback: serve index.html for all unmatched routes
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
```

The `all:` prefix in `//go:embed all:dist` is important -- without it, Go skips
files and directories starting with `.` or `_`, which would silently drop things
like `.vite/` manifest files.

---

## 2. Build Tags for Test Tiers (`//go:build e2e`)

**What:** Every file in `tests/e2e/` starts with `//go:build e2e`.  This means
a bare `go test ./...` never runs them.  You opt in with
`go test -tags e2e ./tests/e2e/`.

**Why:** E2E tests are slow (they spin up real HTTP servers, write temp files,
exercise the full daemon loop).  Unit tests in packages like
`internal/daemon/scanner_test.go` have *no* build tag, so they always run.  This
lets CI run the fast tests on every push and the e2e suite on merge or nightly.

**Where:** `tests/e2e/helpers_test.go:1`, `tests/e2e/daemon_scan_test.go:1`
(and every other file in that directory)

```go
//go:build e2e

package e2e
```

Note: the tag goes on the **first line**, before the `package` declaration, with
a blank line separating them.  The older `// +build` syntax still works but
`//go:build` is the standard since Go 1.17.

---

## 3. Go 1.22 Method+Path Routing

**What:** Go 1.22 added method-aware routing to `http.ServeMux`.  You write
`"GET /api/v1/files"` and the mux only matches GET requests to that path.  Path
parameters like `{id}` are extracted with `r.PathValue("id")`.

**Why:** Before 1.22 you needed a third-party router (chi, gorilla/mux) for
method dispatching.  Now the standard library handles it, which means fewer
dependencies.

**Where:** `internal/api/routes.go:3-21`

```go
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/v1/files", s.handleListFiles)
	s.mux.HandleFunc("GET /api/v1/files/{id}/download", s.handleDownloadFile)
	s.mux.HandleFunc("POST /api/v1/files/{id}/restore", s.handleRestoreFile)

	s.mux.HandleFunc("GET /api/v1/folders", s.handleListFolders)
	s.mux.HandleFunc("PUT /api/v1/folders/{label}", s.handleUpdateFolder)
	// ...

	// Serve embedded React frontend as fallback for all non-API routes
	s.mux.Handle("/", staticHandler())
}
```

The trailing-slash rule still applies: `"GET /api/v1/thumbnails/"` (with the
slash) matches any sub-path, acting like a prefix route.  A route *without* a
trailing slash is exact.

---

## 4. Injectable Function Fields for Testing

**What:** The `Daemon` struct stores `spaceCheck` as a `func` field instead of
calling `AvailableSpacePercent` directly.  Production code uses the real syscall;
tests swap in a stub.

**Why:** Disk-space checks depend on the OS.  Making the function injectable
means tests can simulate "disk full" or "5% free" without touching the
filesystem.  This is Go's lightweight alternative to interface-based mocking --
a single function is often all you need.

**Where:** `internal/daemon/daemon.go:34,47-48,52-54`

```go
type Daemon struct {
	// ...
	spaceCheck func(string) (float64, error)   // line 34
}

func New(cfg *config.Config, cat *catalog.Catalog, logger *slog.Logger) *Daemon {
	return &Daemon{
		// ...
		spaceCheck: AvailableSpacePercent,        // line 48: default impl
	}
}

// SetSpaceCheck replaces the disk space check function, for testing.
func (d *Daemon) SetSpaceCheck(fn func(string) (float64, error)) {
	d.spaceCheck = fn
}
```

The call site in `enforceFolder` (line 199) just calls `d.spaceCheck(...)` --
it doesn't know or care whether it's the real syscall or a test stub.

---

## 5. Panic Recovery in Goroutines

**What:** `safeEnforceQuotas` wraps the actual quota logic in a
`defer func() { recover() }()` block.

**Why:** The daemon's main loop runs in a single goroutine with a `select` on
tickers.  If `EnforceQuotas` panics (say, a nil-pointer from bad catalog data),
an unrecovered panic kills the entire process.  The recovery logs the error and
lets the next tick try again.  This is specifically useful for long-running
services where uptime matters more than strict correctness on a single cycle.

**Where:** `internal/daemon/daemon.go:112-119`

```go
func (d *Daemon) safeEnforceQuotas() {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("panic in quota enforcement", "panic", r)
		}
	}()
	d.EnforceQuotas()
}
```

A rule of thumb: **never** `recover()` from panics in library code -- that hides
bugs.  But in a top-level service loop, it's the right call to keep the daemon
alive.

---

## 6. Temp sibling, verify from disk, then rename

**What:** A cross-device move never writes to the destination path. It writes to
`dst + ".coldstore-tmp"`, fsyncs it, **re-reads it from disk** to hash what the
filesystem actually kept, restores mode/mtime/atime, and only then renames the
temp over the destination -- an atomic operation, since the temp is a sibling.
The source is removed last, after re-stat'ing it to confirm it is still the same
inode.

**Why:** three separate data-loss paths close at once.

1. **Nothing partial is ever visible at the destination.** These are Syncthing
   folders: a truncated file at the destination path is replicated to every
   device the owner has. A kill at any instant can only leave a `.coldstore-tmp`
   file, which no catalog row references and which startup recovery deletes.
2. **The verification means something.** The old code hashed the outgoing stream
   twice -- once through an `io.TeeReader` on the read side and once through an
   `io.MultiWriter` on the write side -- and compared the two. They came from the
   same bytes in memory, so they could not differ by construction: it verified
   nothing about what landed on disk. Re-reading the temp file costs one extra
   sequential pass and is the only version of this check that can fail.
3. **A file cannot be truncated by a move.** The temp is opened `O_EXCL` and an
   occupied destination is `ErrDestinationExists`, never an overwrite.

**Where:** `internal/daemon/move.go`

```go
tmp := dst + tmpSuffix
os.Remove(tmp)                                                    // a previous kill
tmpFile, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)

streamHash := sha256.New()
written, err := copyAndVerify(srcFile, destWriter(tmpFile), streamHash)
syncFile(tmpFile); tmpFile.Close(); syncDirPath(filepath.Dir(tmp))

diskSum, diskSize, err := hashFile(tmp)                           // read it BACK
if diskSize != before.size || string(diskSum) != string(streamHash.Sum(nil)) {
	os.Remove(tmp)
	return errors.New("the copy on disk differs from the bytes that were sent")
}

os.Chmod(tmp, before.mode.Perm())
os.Chtimes(tmp, before.atime, before.mtime)                       // keep the date
os.Rename(tmp, dst)                                               // commit
// --- only now may the source be touched ---
```

The ordering is the specification, and the states it makes reachable are the
whole point:

```
  dst absent, tmp partial   -> killed during the copy; retry from the source
  dst absent, tmp complete  -> killed before the commit; retry from the source
  dst complete              -> committed and verified; the source may go
```

`destWriter`, `syncFile` and `syncDirPath` are package-level function variables
purely so tests can fault-inject: a writer that acknowledges bytes it does not
store, an fsync that fails, a process that panics mid-copy.

**Restoring mtime and atime is not cosmetic.** Without `os.Chtimes`, a bulk
restore re-dates every file to today; Syncthing propagates the new mtime to
every device and the next scan writes it over `files.file_mtime`, destroying the
only remaining record of when the photo was taken. `OffloadCandidates` orders by
`MAX(file_mtime, file_atime)`, so re-dated files also become permanently the
newest thing in the folder and are never offloaded again.

---

## 7. SQLite WAL Mode + Busy Timeout

**What:** The catalog opens SQLite with `_journal_mode=WAL&_busy_timeout=5000`
appended to the DSN.

**Why:** WAL (Write-Ahead Logging) allows concurrent readers and a single writer
without blocking each other -- critical when the scanner, API server, and
offloader all hit the DB from different goroutines.  The `_busy_timeout=5000`
tells SQLite to retry for up to 5 seconds when the DB is locked, instead of
immediately returning `SQLITE_BUSY`.

**Where:** `internal/catalog/catalog.go:17`

```go
func Open(dbPath string) (*Catalog, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	// ...
}
```

Without WAL, you'd see sporadic "database is locked" errors under load.
Without the busy timeout, a single contended write would fail instantly.

---

## 8. Generation-Stamped Missing Detection

**What:** Every walk takes a generation (`time.Now().UnixNano()`) and stamps it
onto `files.last_seen` as it upserts each file it finds.  After a walk that
completed cleanly, one statement marks as `missing` every `synced` row in that
folder whose `last_seen` predates the generation.

**Why:** The previous shape was mark-then-restore -- flip every `synced` row in
the folder to `missing` up front and let the walk put them back.  For the whole
duration of the walk (minutes on a 50,000-file camera roll) every mover CAS of
the form `WHERE status='synced'` failed and `FolderSizes` returned zero, so a
bulk archive reported hundreds of spurious failures and the budget bar
collapsed to empty.  A generation stamp makes missing detection a single atomic
statement at the end, and a walk that errored simply never runs it.

**Where:** `internal/daemon/scanner.go` (the walk), `catalog.UpsertFile` (the
stamp), `catalog.MarkFilesMissingBeforeGeneration` (the sweep).

```go
generation := time.Now().UnixNano()

err = filepath.WalkDir(folder.SyncthingPath, func(path string, d os.DirEntry, err error) error {
	// ...
	conflict, err := s.cat.UpsertFile(record, generation)
	if err != nil {
		s.logger.Warn("upsert failed", "path", relPath, "error", err)
		incomplete++
	}
	// ...
})
if err != nil {
	return err // no sweep after a failed walk
}
if incomplete == 0 {
	s.cat.MarkFilesMissingBeforeGeneration(folder.FolderLabel, generation)
}
```

**The corollary that bites:** any transition that puts a file back on the
device outside a walk -- `MarkRestored`, the archiving->synced revert,
adopting a device copy -- must stamp `last_seen` in the same statement, or a
walk that is already running sweeps the row to `missing` while the file sits on
disk.  That is what `catalog.ClaimFileOnDevice` exists for.

---

## 9. Upsert Conflict Logic Protecting Transient States

**What:** The `ON CONFLICT ... DO UPDATE` clause in `UpsertFile` uses a `CASE`
expression that only resets status to `synced` when the existing status is
`missing` or `archived`.  Files in transient states (`archiving`, `restoring`)
are left alone.

**Why:** Without this guard, a scan that runs *during* an offload would flip a
file from `archiving` back to `synced`, and the offloader would leave an orphan
copy in cold storage.  The `CASE` makes the upsert safe to run concurrently with
state machine transitions.

**Where:** `internal/catalog/catalog.go:39-48`

```go
ON CONFLICT(folder_label, relative_path) DO UPDATE SET
	filename = excluded.filename,
	size_bytes = excluded.size_bytes,
	file_mtime = excluded.file_mtime,
	file_atime = excluded.file_atime,
	mime_type = excluded.mime_type,
	status = CASE
		WHEN files.status IN ('missing', 'archived') THEN 'synced'
		ELSE files.status
	END
```

This is the file status state machine in a nutshell:

```
synced --> archiving --> archived
  ^                        |
  |       restoring  <-----+
  +-----------+
  ^
  |
missing (set by mark-and-sweep, cleared by re-scan)
```

The upsert only touches the `synced <-> missing` and `archived -> synced`
transitions.  Everything else is managed by the offloader and restorer.

---

## 10. Fan-Out Worker Pool

**What:** `ProcessQueue` sends all work items into a buffered channel, closes
it, then launches N goroutines that each `range` over the channel.  A
`sync.WaitGroup` gates the return until all workers finish.

**Why:** Thumbnail generation is CPU-bound (image decoding, resizing) and
I/O-bound (reading source files, shelling out to ffmpeg).  Parallelizing with a
fixed worker count keeps throughput high without overwhelming the system.  The
channel acts as a work queue with automatic load balancing -- fast workers
naturally pull more items.

**Where:** `internal/thumbnail/thumbnail.go:174-206`

```go
func (g *Generator) ProcessQueue(files []catalog.FileRecord, sourcePathFn func(*catalog.FileRecord) string, workers int) {
	if workers <= 0 {
		workers = 2
	}

	ch := make(chan catalog.FileRecord, len(files))
	for _, f := range files {
		ch <- f
	}
	close(ch)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range ch {
				src := sourcePathFn(&file)
				if err := g.Generate(&file, src); err != nil {
					g.logger.Warn("thumbnail generation failed", ...)
				}
			}
		}()
	}
	wg.Wait()
}
```

A few things to notice:
- The channel is buffered to `len(files)`, so the producer loop never blocks.
  This is fine when the work queue is bounded (we know all files upfront).
- `for range workers` (Go 1.22+) replaces `for i := 0; i < workers; i++`.
- Each goroutine owns nothing shared -- the channel and WaitGroup handle all
  synchronization.

---

## 11. Anonymous Struct Embedding for One-Off JSON Responses

**What:** The `handleStatus` handler builds a response by embedding the named
`statusResponse` struct into an anonymous struct that adds one extra field.
This composes the JSON output without declaring a new named type.

**Why:** `ColdStorageFreeBytes` is only relevant to this one endpoint.  Declaring
a `statusResponseWithDisk` type would add noise to the package for a one-off
use.  Anonymous struct embedding lets you extend a type inline.

**Where:** `internal/api/handlers_status.go:53-65`

```go
resp := struct {
	statusResponse
	ColdStorageFreeBytes uint64 `json:"cold_storage_free_bytes"`
}{
	statusResponse: statusResponse{
		TotalFiles:    total,
		SyncedFiles:   synced,
		ArchivedFiles: archived,
		MissingFiles:  missing,
		Folders:       folders,
	},
	ColdStorageFreeBytes: coldFree,
}

writeJSON(w, http.StatusOK, resp)
```

The resulting JSON includes all fields from `statusResponse` *plus*
`cold_storage_free_bytes`, flattened into a single object.  This is a handy
pattern whenever you need to decorate an existing struct with extra context
for a specific API response.
