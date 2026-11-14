e Coldstore as an archive of record

Status: design, awaiting approval. 2026-11-13.
Scope of this document: system architecture and major abstractions. Details that do not
change the shape are listed under "Deferred details" and are settled during planning.

## 1. Why

Coldstore was built to evict old files from Syncthing folders under a byte budget. In
production it has archived 924K. The job it is actually needed for is different: be the
trusted long-term copy of the household's photos, take in large existing collections, and
prune phones as a side effect.

The current model cannot do that job safely:

- A file's identity is `(folder, relative path)` inside a tree that Syncthing and every
  device also write to. Most data-safety fixes to date defend that shared tree.
- Offload is a *move*. For a moment the archive holds the only copy, and the same move is
  the deletion signal to every device.
- No content hash is stored (`files.content_hash` is never written), so nothing can be
  re-verified and there is no scrub.
- The SQLite catalog is the only record of what is archived, sits on the same filesystem,
  and cannot be rebuilt from disk.
- There is no way in except through a Syncthing folder.
- Coldstore has no Syncthing client. It cannot tell whether a folder is Send & Receive,
  whether Syncthing is running, or whether a file is mid-sync.

Source material: `docs/implementation-review-2026-11.md` (items A1 to A6, B1) and
`docs/photo-export/` in the `docs/implementation-review` worktree.

## 2. Decisions already made

| Question | Decision |
|---|---|
| Syncthing's role | Transport and working set. It stays, because it is the only background sync on iOS. |
| Archive's role | A superset. Everything that arrives is copied in; nothing is moved in. |
| Eviction | A delete of the hot copy after the archive copy is confirmed by hash. Pruning phones this way is a first-class feature. |
| Device-side delete | The archive keeps its copy. |
| Store layout | Blobs named by SHA-256, plus a rebuildable hardlink view that mirrors folder paths. |
| Source of truth | An append-only journal. SQLite is an index rebuilt from it. |
| What a "photo" is | A stable Item with versions. Tags, descriptions and pairing attach to the Item. |
| Destroying bytes | Only an explicit delete in Coldstore, through trash, then purge. Nothing automatic. |
| Second copy | Not now. RAID1 plus hash scrub. The layout keeps it one `rsync` away. |
| Build path | New core beside the old daemon; folders cut over one at a time; legacy mover deleted at the end. |
| Order | Milestone 1: archive (store, journal, import, upload, scrub, rebuild). Milestone 2: Syncthing (ingest on arrival, gate, eviction, cutover). |

## 3. Architecture

```
            sources                         archive (vault, RAID1)
  ┌──────────────────────────┐
  │ importdir   (CLI, M1)    │──┐
  │ upload      (HTTPS, M1)  │──┼─▶ ingest.Pipeline ─▶ store.Put ─▶ blobs/sha256/ab/cd/<hash>
  │ syncfolder  (M2)         │──┘         │
  └──────────────────────────┘            ▼
                                     journal.Append ─▶ journal/000042.jsonl   (truth)
                                          │
                                          ▼ apply
                                     index.db (SQLite)        views/<folder>/<path>
                                     thumbnails/              (both rebuildable)

  M2 only:   evict ─▶ syncthing.Gate ─▶ delete hot copy ─▶ Syncthing propagates to devices
             restore ─▶ store.Open ─▶ copy into the Syncthing folder (blob stays)
```

Three rules hold everywhere:

1. **Bytes flow one way.** Only `store.Put` writes archive bytes. A blob is never modified,
   renamed or moved after it is linked in.
2. **The journal is written before the index.** The index can always be thrown away.
3. **The journal never names a blob that is not durably on disk.** The reverse (a blob no
   record names) is harmless and is what a crash leaves behind.

### On-disk layout

```
/srv/vault/coldstore/
  blobs/sha256/ab/cd/abcd…ef      immutable, mode 0444, one file per distinct content
  journal/000001.jsonl …          append-only segments, fsynced
  journal/LOCK                    held by the single writer
  tmp/                            Put staging; same filesystem as blobs/
  index.db                        rebuildable from journal/
  views/<folder>/<path>           hardlinks into blobs/; rebuildable; browsable with cp
  thumbnails/sha256/ab/cd/…       derived from bytes; rebuildable
  catalog.db, archive/, trash/    legacy, untouched until each folder is cut over
```

A complete backup is `blobs/` plus `journal/`. Everything else can be regenerated.

## 4. Abstractions

### 4.1 Blob and Store

```go
// internal/store
type Hash [sha256.Size]byte

type Store interface {
    // Put stages r under tmp/, fsyncs it, re-reads it from disk, and links it
    // in under its hash, then fsyncs the directory. If want is non-nil and the
    // bytes hash to something else, nothing is kept and ErrHashMismatch is
    // returned. Putting content the store already holds verifies the existing
    // blob and is otherwise a no-op.
    Put(ctx context.Context, r io.Reader, want *Hash) (Hash, int64, error)

    Open(h Hash) (io.ReadSeekCloser, error)

    // Verify re-hashes one blob from disk.
    Verify(ctx context.Context, h Hash) error

    // Walk visits every blob file present, whether or not the journal names it.
    Walk(ctx context.Context, fn func(h Hash, size int64) error) error
}
```

SHA-256 because photo-export's manifests and Syncthing both already use it.

The store knows nothing about folders, items or Syncthing. All filesystem calls go through
a small `fsx` interface so tests can inject faults (section 8).

### 4.2 Journal

One JSON object per line. Each record carries a sequence number, a timestamp, the actor,
and a checksum of the line so a torn or damaged record is detectable.

```jsonl
{"seq":9119,"at":"2026-11-13T22:01:07Z","by":"import","t":"folder_declared","folder":"Photos","kind":"collection"}
{"seq":9120,"at":"…","by":"import","t":"blob_added","hash":"abcd…","size":4211034}
{"seq":9121,"at":"…","by":"import","t":"item_created","item":"01JCX…","hash":"abcd…"}
{"seq":9122,"at":"…","by":"import","t":"placement_set","folder":"Photos","path":"2019/trip/IMG_1.HEIC","item":"01JCX…","mtime":"2019-06-02T10:11:12Z"}
{"seq":9140,"at":"…","by":"jen@…","t":"item_tagged","item":"01JCX…","add":["tokyo"]}
{"seq":9200,"at":"…","by":"tjmisko@…","t":"item_trashed","item":"01JCX…"}
```

```go
// internal/journal
type Journal interface {
    // Append assigns the next sequence number, writes and fsyncs. It returns
    // only after the record is durable.
    Append(ctx context.Context, recs ...Record) (lastSeq uint64, err error)

    // Replay calls fn for every intact record after fromSeq, in order. A torn
    // final record is dropped and reported; damage anywhere else is an error.
    Replay(ctx context.Context, fromSeq uint64, fn func(Record) error) error
}
```

What is journaled: anything a person or a device decided. Folder declarations and policy,
blobs, items, versions, placements, tags, descriptions, hand-edited locations, pins, pairs,
trash and purge, and (in M2) hot-copy changes.

What is not: anything derivable from bytes. Thumbnails, EXIF capture time, GPS, detected
type, scrub timestamps.

There is exactly one writer. The daemon holds `journal/LOCK`. `coldstore import` is a client
that asks the running daemon to do the work. Offline commands (`rebuild-index`, offline
scrub) take the lock themselves and refuse to run while the daemon holds it.

### 4.3 Index

SQLite, derived. It records the last journal sequence it applied.

```go
// internal/index
func (ix *Index) Apply(rec journal.Record) error // one transaction; advances applied_seq

// At startup, and for `coldstore rebuild-index` from an empty file:
journal.Replay(ctx, ix.AppliedSeq(), ix.Apply)
```

Write path everywhere: `journal.Append`, then `index.Apply`. A crash between the two is
healed by the replay at startup.

Core tables (shape, not final DDL):

```sql
blobs      (hash PRIMARY KEY, size, added_seq, verified_at, bad)   -- verified_at, bad: operational
items      (id PRIMARY KEY, current_hash, trashed_at, description, …)
versions   (item_id, hash, superseded_seq)
placements (folder, path, item_id, mtime, hot, pinned, PRIMARY KEY (folder, path))
tags, item_tags, pairs, folders
```

It is a separate file from the legacy `catalog.db`, so "delete the index and rebuild" is
true from the first day, while legacy folders keep their own catalog until cutover.

### 4.4 Item and Placement

```go
// An Item is one logical photo or file. Its ID never changes.
type Item struct {
    ID       ItemID       // tags, description, pair, trash state hang here
    Current  store.Hash
    Versions []store.Hash // earlier bytes, kept
}

// A Placement says: this folder path currently means this Item.
type Placement struct {
    Folder, Path string
    Item         ItemID
    Hot          bool // a copy exists in the Syncthing folder (always false in a collection)
    Pinned       bool // never evict this path
}
```

How an arrival at `(folder, path)` with hash `h` resolves:

| Situation | Result |
|---|---|
| Path already placed, Item's current hash is `h` | Nothing to do |
| Path already placed and hot, different hash | Edit in place: same Item, new version |
| Path not placed; `h` is current for an Item whose old path in this folder has gone | Rename: same Item, placement moves, tags kept |
| Path not placed; `h` is current for an Item still at its old path | Copy: same Item, second Placement |
| Otherwise | New Item |

A Folder has a kind: `sync` (backed by a Syncthing folder, has a budget) or `collection`
(archive only, no device home, no budget). Uploads and imports go into collections.

### 4.5 Source and Pipeline

```go
// internal/ingest
type Arrival struct {
    Folder, Path string
    Mtime        time.Time
    Open         func() (io.ReadCloser, error)
    Want         *store.Hash // set when the sender supplied a hash
}

type Source interface {
    Arrivals(ctx context.Context) <-chan Arrival
}

func (p *Pipeline) Ingest(ctx context.Context, a Arrival) (ItemID, error) {
    r, err := a.Open()
    if err != nil {
        return "", p.fail(a, err)
    }
    defer r.Close()

    h, size, err := p.store.Put(ctx, r, a.Want)
    if err != nil {
        return "", p.fail(a, err) // source untouched; event recorded; retried with backoff
    }
    recs, item := p.resolve(a, h, size) // the table in 4.4, as journal records
    if _, err := p.journal.Append(ctx, recs...); err != nil {
        return "", err
    }
    return item, p.index.ApplyAll(recs)
}
```

No source ever deletes what it read. Import leaves the directory alone; upload has nothing
to delete; in M2 the Syncthing folder keeps its file until eviction decides otherwise.

The three sources:

- **importdir (M1).** `coldstore import <dir> --collection <name>`. Walks a local directory.
  Re-running is safe and resumes: content already held is a no-op. Unreadable files are
  reported, not fatal. Because content is addressed by hash, importing `~/photos` from the
  failing disk and then importing the rescue copy fills any gaps and proves the two agree.
- **upload (M1).** HTTPS through the existing Tailscale Serve front, which is all Jen's
  grant allows.

  ```
  HEAD /api/v1/blobs/{sha256}                      200 if held, 404 if not (skip the upload)
  PUT  /api/v1/collections/{name}/files/{path}     body = bytes
       X-Coldstore-Sha256: <hex>                   required; mismatch => 422, nothing kept
       X-Coldstore-Mtime:  <RFC 3339>
  POST /api/v1/collections/{name}/receipt          body = [hashes]
       => for each: held, and re-verified from disk just now
  ```

  The receipt is what photo-export's `verify` step consumes before anything is marked
  deletable in Photos.
- **syncfolder (M2).** Watches a Syncthing folder: Syncthing's event stream names changed
  paths, and a periodic walk is the fallback. Also reports hot copies that have gone, which
  become "not on devices" placements, never archive deletions.

### 4.6 Scrub, download and rebuild (M1)

- `scrub` walks `blobs/`, re-hashes each file and compares it to its name. It also
  cross-checks the index: a blob the index names that is missing or wrong is marked `bad`
  and raises an event; a blob nothing names is reported as an orphan. It is throttled, since
  the host has two slow cores.
- A blob marked `bad` is never served. A download hashes as it streams and aborts the
  response on a mismatch, so a client sees a failed transfer, not wrong bytes.
- `rebuild-index` replays the journal into an empty `index.db`, then regenerates `views/`.

### 4.7 Syncthing client, Gate, eviction, restore (M2)

```go
// internal/syncthing: a thin REST client for Syncthing 2.x.
type Gate interface {
    // SafeToDelete reports nil only when a local delete of path will propagate
    // as a delete and cannot race an incoming change:
    //   the folder exists, is Send & Receive and is not paused;
    //   the folder is idle with nothing left to pull;
    //   the local version of path is the global version.
    // Syncthing unreachable is an error, so nothing is evicted.
    SafeToDelete(ctx context.Context, folderID, path string) error
}
```

```go
// internal/evict
func (e *Evictor) Evict(ctx context.Context, pl Placement) error {
    item := e.index.Item(pl.Item)
    if err := e.store.Verify(ctx, item.Current); err != nil {
        return err // the archive must hold these bytes, read back from disk, now
    }
    if err := e.hotMatches(pl, item.Current); err != nil {
        return err // the hot file changed: it is an arrival, not an eviction
    }
    if err := e.gate.SafeToDelete(ctx, pl.FolderID, pl.Path); err != nil {
        return err
    }
    if _, err := e.journal.Append(ctx, hotCleared(pl, "evicted")); err != nil {
        return err
    }
    return os.Remove(pl.HotPath()) // Syncthing propagates; phones free the space
}
```

The journal record goes first. If the process dies before the `Remove`, the next walk sees
the file still present and records it hot again. If it dies after, the record is already
right. Neither order can lose bytes, because eviction never touches the archive.

Selection policy is unchanged in spirit: per-folder budget, oldest first, pins and pair
groups respected. What changes is what happens to a chosen file.

Restore is `store.Open` copied into the Syncthing folder, hash-checked as it is written,
and pinned. The blob stays. If the path is occupied by the same bytes it is a no-op; by
different bytes, that file is simply the next arrival.

The legacy failure modes this removes: the only copy in motion, set-aside
`.coldstore-superseded` files, conflict rows, recovery deleting a source on size and mtime
(review A2), and a Receive Only folder voiding the product (A3). A misconfigured or stopped
Syncthing now means space is not freed, and nothing else.

## 5. Milestones

**M1: archive.** `store`, `journal`, `index`, `ingest` with `importdir` and `upload`,
`views/`, scrub, `rebuild-index`, collections in the API and UI (browse, thumbnails,
download, tags, trash). Two small items from the review ride along because they are
operator-error data-loss paths: strict TOML decoding (A5) and a format-version check so an
older binary refuses a newer journal or index (A1). The legacy daemon keeps running the
Syncthing folders exactly as today.

Done when: `~/photos` is imported and scrubbed, photo-export can ship a batch and get a
receipt, and the test bar in section 8 passes in CI.

**M2: Syncthing.** `syncthing` client and Gate, `syncfolder` source, `evict`, restore as
copy-out, per-folder cutover from the legacy catalog (rows become journal records; the few
archived files are re-ingested as blobs), then deletion of the legacy mover, offloader and
recovery code.

## 6. Legacy coexistence

During M1 the binary runs both models. Folders in `[[folders]]` are legacy and use
`catalog.db`. Collections in a new `[[collections]]` table use the journal and `index.db`.
The API reads from whichever owns the folder. Nothing in M1 writes to a Syncthing folder
or changes legacy behaviour.

## 7. Out of scope

- Replicating to a second disk or off-site, and repairing from a replica.
- Automatic expiry of old versions.
- Public sharing, roles, or any access change beyond the existing Tailscale allowlist.
- Receive Only or Send Only server folders.
- Moving hot folders off the OS disk, and the OS reinstall.
- The Mac's overlapping Syncthing folders (a configuration fix, not a Coldstore change).

## 8. Test bar for M1

All four tiers run in CI. Tests are named "should [expected behavior] when [condition]".

| Tier | Method | Expected result |
|---|---|---|
| Crash at every step | Named crash points between every pair of filesystem and journal operations in Put, import and upload; the harness kills the process at each one and restarts | Every blob the journal names verifies. No record names a missing blob. The source is never removed or altered. Re-running the import finishes with no duplicate Items. |
| Injected disk faults | The `fsx` seam returns EIO, short reads, short writes, fsync failure and ENOSPC at each call site | Put returns an error. Nothing is linked under a hash. The failure is an event with backoff. The source is untouched. |
| Corruption and scrub | Flip a byte in a blob, truncate one, delete one | Scrub reports exactly those hashes. A download of a damaged blob fails. Nothing else is flagged. |
| Index loss and rebuild | Delete `index.db`; corrupt it; cut the journal's last record mid-line | The rebuilt index equals the original (items, versions, placements, tags, pins, trash). A torn final record is dropped with a warning. `views/` is regenerated identically. |

M2 adds a live tier against real Syncthing: an eviction propagates to a second device, and
the Gate refuses when the folder is Receive Only, mid-sync, paused, or Syncthing is down.

## 9. Deferred details

Settled during planning; none changes the architecture above.

- How `views/` names two Items that have held the same path.
- Whether `hotMatches` re-hashes the hot file or trusts size, mtime and inode from ingest.
- Where restore stages its temp file so Syncthing never sees a partial.
- Resumable uploads for multi-gigabyte videos.
- Journal segment size, group commit during bulk import, and compaction.
- Scrub throttling and schedule on the N40L.
- The per-folder cutover procedure and its rollback.
- UI changes for collections, versions and "not on devices".
