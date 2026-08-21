# Coldstore Tagging, Capture Metadata and Rename — Authoritative Technical Design

Status: **authoritative** for the tagging work. Where it contradicts `docs/architecture.md`
or `docs/react-guide.md`, this document wins for the surfaces it names and defers to them
everywhere else. It does **not** supersede `docs/control-plane-design.md`; that document's
global invariants (section 0) still hold, and section 0 below states the three of them this
work comes closest to breaking.

It builds to nine decisions:

| ID | Decision |
|----|----------|
| T1 | Tags are a normalised `tags` / `file_tags` pair, with hierarchy stored as a **materialised path** in `tags.name` and no `parent_id`. |
| T2 | Descriptions and derived capture metadata are **columns on `files`**, not a side table. `files` rows are never deleted, so there is nothing to orphan. |
| T3 | Capture time is stored as the **naive local wall clock the camera wrote**, plus a separately nullable UTC offset. No derived UTC epoch is stored. |
| T4 | Extraction runs in the **background derived-data pass**, alongside but not inside the thumbnail pass, with its own queue predicate and its own attempt budget. |
| T5 | Coldstore **never writes into a Syncthing folder**. No XMP sidecars, no in-place EXIF edits. Portability is served by an export to cold storage. |
| T6 | Search extends the existing `q` predicate and adds an index-backed `tag=` filter. **No FTS5** — it would change the build tags of every CI job for a corpus that does not need it. |
| T7 | Tag writes do **not** take the `Mover` mutex, because a tag changes nothing quota enforcement reads. The moment a tag gates offloading, that stops being true. |
| T8 | "Rename a folder" means renaming a **directory inside** a Syncthing folder. It rewrites three trees and two columns and runs as a durable job. The coldstore folder **label** is not renameable; a cosmetic `display_name` is offered instead. |
| T9 | Live Photos are a first-class **pair group**. A member is never archived, restored or renamed alone. |

---

## 0. The three invariants this work comes closest to breaking

Read `docs/control-plane-design.md` §0 first. These three deserve restating because the
obvious implementation of this feature violates each of them.

1. **One SQLite writer** (§0.1). `db.SetMaxOpenConns(1)`. Every handler serialises with the
   quota tick and the job worker. A bulk tag of 4,000 photos implemented as 4,000 requests,
   or as one request that runs 4,000 statements, holds the only connection in the process
   for the duration and stalls enforcement behind it. **Every bulk write in this document is
   chunked at 500 ids per statement**, the same shape `SetPinnedBulk` already uses.

2. **One mover** (§0.2). Every write to `files.pinned` goes through `daemon.Mover`, because a
   pin changes what enforcement may touch. **Tag and description writes deliberately do
   not** (T7). Routing them through the mover would block a caption behind an in-flight
   cross-device restore for no safety benefit. The rename job **does** take the mover mutex,
   because it moves bytes.

3. **Prefer keeping bytes** (§0.9). Every ambiguous state below resolves toward keeping data
   and writing a loud event. The rename job never overwrites an occupant; the extraction
   pass never overwrites a value the owner typed by hand.

And one new invariant this work adds:

4. **Coldstore never creates a file inside a Syncthing folder.** It moves files out, moves
   them back in, and renames them. It does not write sidecars, thumbnails, journals or
   exports there. Deletion propagates through Syncthing — and so does creation. Section 1.7
   is this invariant applied to XMP.

---

## 1. Metadata — what actually survives an iPhone

This is the subtlest part of the feature and the part most likely to be got wrong quietly,
so it is settled first and the schema follows from it.

### 1.1 The path a file takes

```
iPhone camera ─► Photos library ─► an app that copies the ORIGINAL out
                                    ─► a Syncthing folder on the phone
                                    ─► Syncthing ─► this box ─► coldstore
```

Two facts about that path matter.

**Syncthing does not touch content and does preserve mtime.** It transfers blocks verbatim
and explicitly syncs modification times; a file that arrives here is byte-identical to the
file that left the phone, and its mtime is the phone's mtime, not the transfer time. That
is what makes `max(mtime, atime)` a usable offload score today, and it is what makes mtime a
*plausible* but still wrong capture date (see 1.3). Syncthing's own scratch names —
`.stfolder`, `.stversions`, `.stignore`, `.syncthing.NAME.tmp`, `~syncthing~NAME` — are
already excluded by `skipScanEntry` in `internal/daemon/scanner.go`.

**What iOS gives the syncing app is not always the original.** There are two distinct
stripping events, and they are frequently confused:

- **The share sheet.** Sharing photos through the iOS share sheet exposes an *Options*
  control with a **Location** toggle. With it off — and it defaults off for some share
  targets — the exported copy has its **GPS IFD removed entirely** while
  `DateTimeOriginal` survives. A photo that arrives with a date and no location has very
  often been through this.
- **Transfer/format conversion.** *Settings → Camera → Formats* chooses what the camera
  **writes**: "High Efficiency" produces `.HEIC` + HEVC `.MOV`, "Most Compatible" produces
  `.JPG` + H.264 `.MOV`. Separately, *Settings → Photos → Transfer to Mac or PC* chooses
  whether a transfer is allowed to transcode HEIC to JPEG ("Automatic") or must hand over
  the original ("Keep Originals"). A transcode preserves EXIF including GPS, but it is a
  re-encode; and it may renumber or rename.

**Consequence for the design:** coldstore must treat *absent* metadata as the normal case,
not the error case. A photo with no capture date and no location is not a failure to
extract — it is a photo whose metadata was removed before it ever reached this box. The
schema therefore records **why** a value is missing (`captured_source = ''` and
`meta_scanned_at` set) so the UI can say "this file carries no capture date" rather than
"not scanned yet", and so the owner can filter for exactly that set and fix it by hand.

### 1.2 Formats coldstore will actually see

| Extension | Container | Metadata home |
|---|---|---|
| `.heic`, `.heif` | ISOBMFF (HEIF) | An `Exif` item located through `iinf`/`iloc`; Apple maker note inside it |
| `.jpg`, `.jpeg` | JFIF | `APP1` EXIF segment; sometimes a second `APP1` XMP segment |
| `.png`, `.webp` | — | Rarely carries EXIF; treat as unlikely to have any |
| `.mov` | QuickTime | `moov/meta` `keys`+`ilst` reverse-DNS tags; `mvhd` creation_time |
| `.mp4` | ISOBMFF | Same, plus `©xyz` in some writers |
| `.dng` | TIFF | Standard EXIF IFDs |
| `.aae` | XML plist | An **edit sidecar**, not a photo — see 1.6 |

`.HEIC` is the format the whole feature has to work on, because it is the default an
unmodified modern iPhone writes. Any library that only handles JPEG is not a candidate.

### 1.3 Capture datetime — five tags, one answer

| Tag | ID | What it means |
|---|---|---|
| `DateTimeOriginal` | 0x9003 | When the **shutter fired**. This is the capture time. |
| `DateTimeDigitized` / `CreateDate` | 0x9004 | When the image was **digitised**. Equal to the above for a digital camera; differs for a scan. |
| `DateTime` / `ModifyDate` | 0x0132 | When the **file** was last written. An edit updates it. |
| `SubSecTimeOriginal` | 0x9291 | Fractional seconds for 0x9003, as a string of digits. |
| `OffsetTimeOriginal` | 0x9011 | The **UTC offset in force** when 0x9003 was written, e.g. `"+02:00"`. |

The three datetime tags are `YYYY:MM:DD HH:MM:SS` ASCII and are **naive** — they carry no
timezone at all. That is not a defect of the format; it is a deliberate property. The offset
tags (0x9010 `OffsetTime`, 0x9011 `OffsetTimeOriginal`, 0x9012 `OffsetTimeDigitized`) were
added by EXIF 2.31 (2016) precisely to repair it, and iOS has written them for several major
versions — but not forever, and not for files that have been through a transcoder that
dropped them.

For QuickTime containers the equivalents are:

- `com.apple.quicktime.creationdate` — an ISO 8601 string **with an offset**, e.g.
  `2023-07-14T18:32:07+0200`. This is the good one.
- `creation_time` from the `mvhd` box — seconds since 1904-01-01, **UTC**, with no offset
  recorded. ffprobe surfaces it as an RFC 3339 UTC string. Usable, but it tells you the
  instant and not the wall clock, which is the wrong half (see 1.4).

**Precedence, in order, first hit wins:**

1. `DateTimeOriginal` (+ `SubSecTimeOriginal`, + `OffsetTimeOriginal`) → `captured_source = 'exif'`
2. `com.apple.quicktime.creationdate` → `captured_source = 'quicktime'`
3. `DateTimeDigitized` (+ its offset) → `captured_source = 'exif_digitized'`
4. `mvhd creation_time`, converted to wall clock using the **GPS-derived or ISO6709-derived
   offset if one is available, otherwise left as UTC with the offset recorded as `0`** →
   `captured_source = 'quicktime_mvhd'`
5. Nothing. `captured_local = ''`, `captured_source = ''`.

**`ModifyDate` is never used.** It is a file-write time wearing a capture time's clothes, and
an in-place edit — a crop on the phone, a metadata rewrite by any tool — overwrites it. A
value that changes when the photo does not is not a capture date.

**File mtime is never written into `captured_local`.** This is the single most tempting
shortcut in this document and it is rejected in section 10. The mtime of a file that came
off a phone is close to the capture time; the mtime of a file restored from a backup, copied
off a card, or written by coldstore's own cross-device move path is not. `crossDeviceMove`
does restore mode and mtime, but a file that has been through a copy on another machine has
not. Storing a guess in the same column as a fact makes the column useless for the one
question the owner asked it: *when was this taken?*

### 1.4 Timezone — the decision, and why it goes the way it does

An EXIF datetime is a wall clock without a place. There are two ways to store it.

**Option A: normalise to a UTC instant.** Requires an offset. When `OffsetTimeOriginal` is
present this is exact. When it is absent it requires guessing — from GPS coordinates via a
timezone-shape database, from the server's own timezone, or from `GPSDateStamp`/`GPSTimeStamp`
(which *are* UTC) differenced against the naive local time.

**Option B: store the naive wall clock verbatim, and the offset separately when known.**

**Coldstore stores B.** Three reasons, in order of weight:

1. **It is what a photographer means by "when".** A photo taken at 09:00 in Tokyo and one
   taken at 09:00 in London on the same day belong next to each other in a "morning of the
   14th" sense, and 8 hours apart in an instant sense. Photo Mechanic, Lightroom's library
   module and digiKam all sort and group by the naive capture time by default, and they are
   right: the day you experienced is the organising unit of a photo library, not the
   absolute instant.
2. **Guessing is silently destructive.** A wrong guess of a few hours reorders photos across
   a midnight boundary and moves them into the wrong day group. The failure is invisible —
   there is no error, just a photo filed on the wrong date — and it is unrecoverable once
   the naive value has been discarded.
3. **Sorting is free.** `YYYY-MM-DD HH:MM:SS[.sss]` sorts lexicographically in exactly
   chronological order, so `ORDER BY captured_local` needs no conversion and no function
   index.

So: `captured_local TEXT` holds the wall clock, `captured_offset_minutes INTEGER NULL`
holds the offset **when it was actually recorded** and NULL when it was not, and **no derived
UTC epoch is stored**. Anything that needs an instant computes it in Go from the pair, and
refuses to compute one when the offset is NULL rather than substituting a default.

The UI is required to render an unknown offset as unknown — "18:32, timezone not recorded" —
and never to render a naive time in the browser's local zone, which is the default behaviour
of `new Date(...)` and would be a lie for every photo taken away from home.

`SubSecTimeOriginal` is appended as `.sss` when present. It matters more than it looks: a
burst writes ten frames inside one second, and without sub-seconds the only tiebreak is `id`,
which is scan order, not shutter order.

### 1.5 GPS

**EXIF GPS IFD.** Latitude and longitude are each a triplet of unsigned RATIONALs —
degrees, minutes, seconds — with the sign carried separately in `GPSLatitudeRef` (`"N"` /
`"S"`) and `GPSLongitudeRef` (`"E"` / `"W"`).

```
decimal = deg + min/60 + sec/3600 ;  negate for "S" or "W"
```

`GPSAltitude` is a single unsigned RATIONAL in metres with `GPSAltitudeRef` as a **byte**:
`0` = above sea level, `1` = below. Negating on ref `1` is required; a library that returns
altitude without consulting the ref will put a Dead Sea photo 400 m in the air.
`GPSHorizontalPositionError` (0x001F, metres) is written by recent iOS and is worth keeping —
it is what lets the UI say "within 12 m" instead of implying survey precision.
`GPSDateStamp` + `GPSTimeStamp` are **UTC** and are the fallback that can recover an offset
when `OffsetTimeOriginal` is missing: `offset ≈ round(naive_local − gps_utc)` to the nearest
15 minutes.

**Apple QuickTime.** Videos and Live Photo movies carry
`com.apple.quicktime.location.ISO6709`, an ISO 6709 string in Apple's fixed-width form:

```
+37.7749-122.4194+010.000/
±DD.dddd±DDD.dddd±AAA.aaa/
```

Parsing rules that a naive `strings.Split` gets wrong:

- Every field is **sign-prefixed**, including the first. The sign is the field delimiter;
  there is no separator character.
- Latitude precedes longitude, and longitude carries **three** integer digits where latitude
  carries two.
- The altitude field is **optional**. `+37.7749-122.4194/` is legal.
- A CRS suffix may follow the `/`, e.g. `CRSWGS_84`. Ignore it, but do not choke on it.
- The trailing `/` is mandatory in the standard and is present in Apple's output; accept its
  absence anyway rather than rejecting the tag.

Implement this as a single regexp with named groups and a table-driven test over the four
shapes above plus a southern-hemisphere and a below-sea-level case. ffprobe surfaces the raw
string in `format.tags` under both `com.apple.quicktime.location.ISO6709` and, for some
writers, a normalised `location` key; read the reverse-DNS one first.

**Storage.** `gps_lat REAL`, `gps_lon REAL`, `gps_altitude_m REAL`, `gps_accuracy_m REAL`,
all NULL when unknown, all WGS84 signed decimal degrees. REAL, not TEXT: a bounding-box
query is `gps_lat BETWEEN ? AND ? AND gps_lon BETWEEN ? AND ?` against an index, and float64
resolves to well under a millimetre at these magnitudes. Do not store a geohash — it buys
nothing a two-column index does not, and it is a second representation to keep consistent.

**Reverse geocoding is rejected.** See section 10.

### 1.6 Live Photos, `.AAE` sidecars, and the pairing problem

A Live Photo exported as originals is **two files sharing a stem**:

```
IMG_1234.HEIC     the still
IMG_1234.MOV      ~3 seconds of video, HEVC, silent-ish
```

They are bound together by an Apple UUID written into both:

- In the `.MOV`, the QuickTime tag `com.apple.quicktime.content.identifier`.
- In the `.HEIC`, `ContentIdentifier` inside the **Apple maker note** (`MakerApple` tag
  0x0011) in the EXIF.

Related iOS naming conventions that appear in the same folders:

- `IMG_E1234.HEIC` — the **edited** version of `IMG_1234.HEIC`. Both may be present.
- `IMG_1234.AAE` — an XML property list describing non-destructive edits. It is not a photo;
  it is meaningless without its original, and it is worthless on its own.

**Why coldstore has to care.** Archiving one member of a pair and not the other is a real
bug, not a cosmetic one. The archive is a Syncthing deletion: the `.MOV` disappears from the
phone while the `.HEIC` stays, and the Photos app shows a still that has silently stopped
being a Live Photo. The reverse — archiving the `.HEIC` and keeping the `.MOV` — leaves an
orphan video clip. And an `.AAE` archived without its original strands an edit that can never
be applied again.

**T9: pairs are first class.** Two columns on `files`:

- `pair_key TEXT NOT NULL DEFAULT ''` — namespaced so the two sources cannot collide:
  - `cid:<uuid>` when a content identifier was read from the file.
  - `stem:<dir>/<stem>` when the pairing was inferred from the filename.
- `pair_source TEXT NOT NULL DEFAULT ''` — `''` | `'content_id'` | `'basename'`.

**Basename inference rules** (used only when no content identifier is available):

- Same folder, same directory, same case-folded stem, and at least one member is a video
  extension or an `.aae`.
- `IMG_E1234` pairs with `IMG_1234` — strip a leading `E` after the `IMG_` prefix when
  matching, but keep both files' own stems for the key so an edited pair and its original
  form one group of up to four files.
- Capture times, when both are known, must be within 60 seconds. This is what stops
  `notes.txt` and `notes.mov` in a Documents folder being called a Live Photo.

**What breaks basename matching, and why the content identifier is preferred:** an import
that renumbers on collision (`IMG_1234 (1).HEIC`), a user rename of one member, a folder
merge that puts two devices' `IMG_1234` in one directory, and any tool that re-encodes the
video to `.mp4` with a different stem. The content identifier survives all of these; the
stem survives none of them. Basename matching is the fallback, it is marked as such in
`pair_source`, and the UI says which it used.

**The rule the rest of the system obeys:** `ExpandPairGroups(ids []int64) []int64` is a
single catalog primitive, and **every** entry point that selects files for a move calls it:
`OffloadCandidates` (so the byte arithmetic counts the whole group), the archive and restore
job enqueue path, and the rename job. Additionally, `Mover.archiveLocked` writes a
`pair_split` warning event if it is ever asked to archive a file whose group contains a
`synced` sibling — belt and braces, because a single primitive that one caller forgets to
call is the failure mode this feature has.

### 1.7 XMP sidecars — read never, write never, export yes (T5)

XMP is the standard way photo tools carry keywords and descriptions outside a file:

| Field | Meaning |
|---|---|
| `dc:subject` | Flat keyword bag |
| `lr:hierarchicalSubject` | `Trips|Iceland|Reykjavik` — hierarchy with `|` separators |
| `dc:description` | The caption |
| `exif:UserComment` | An older caption field, still read by some tools |

Sidecars are conventionally named either `photo.xmp` (replacing the extension — Lightroom's
form for raw files) or `photo.jpg.xmp` (appending — darktable, digiKam). Neither convention
survives a rename of the original: the sidecar becomes an orphan silently.

**Coldstore writes nothing back to originals, and writes no sidecars into a synced folder.**
Four reasons, in descending order of severity:

1. **Writing into the folder propagates.** Invariant 4. A sidecar written next to a photo is
   a new file Syncthing pushes to the phone, where nothing reads it, which coldstore then
   catalogues, counts against the folder's budget, and eventually archives — creating the
   pairing problem of 1.6 for a file that carries no photographic content at all.
2. **It resets every grace window and every offload score.** Writing to an existing file
   changes its mtime. `max(mtime, atime)` is the offload score and mtime is the grace-period
   input, so a tagging pass over 4,000 photos makes the entire folder ineligible for
   offloading for `grace_minutes` and reorders the offload queue to put the most recently
   *tagged* files last. The owner's tagging session would silently defer the archiving the
   product exists to do.
3. **Rewriting an original risks the only copy.** For most of these files there is exactly
   one. An in-place EXIF write on a HEIC means re-serialising an ISOBMFF box structure; the
   failure mode is a corrupt photo, and coldstore's whole design (§0.9, §0.10) is built
   around never being in a position to do that.
4. **It is not needed for the feature.** Nothing in the owner's request requires the tag to
   live in the file. It requires the tag to be fast to apply and reliable to find.

**Portability is served by export, not by sync.** `coldstore tags export` (and
`POST /api/v1/tags/export`) writes XMP sidecars — `dc:subject`, `lr:hierarchicalSubject`,
`dc:description`, plus the coldstore-derived capture and GPS values — into
`{cold_storage_path}/sidecars/{folder}/{relative_path}.xmp`, a tree that mirrors
`archive/` and `thumbnails/` and is **outside every Syncthing folder**. The catalog itself
lives on the same pool, which is checksummed and snapshotted. That gets the owner a
directory they can hand to Lightroom or digiKam, with none of the four costs above.

**Reading sidecars that are already there** is out of scope for v1 and is *not* the same
question. An `.xmp` in a synced folder today is catalogued as an ordinary file; the pairing
rules of 1.6 should treat it like `.aae` so it is at least never archived away from its
photo. Importing its keywords is a later issue.

### 1.8 Library selection

Constraints: the target is a low-power 2-core home server with a few gigabytes of RAM;
`ffmpeg`/`ffprobe` are already required dependencies (`internal/thumbnail`
shells out to ffmpeg for HEIF decode and video frames); the binary is a single static
artifact and CGO is already in play via `mattn/go-sqlite3`.

**Decision: a pure-Go EXIF reader for still images, `ffprobe` for QuickTime containers.**

| Candidate | Verdict |
|---|---|
| `github.com/evanoberholster/imagemeta` (pure Go) | **Leading candidate for stills**, on the grounds that it is the current pure-Go option that reads HEIF/HEIC as well as JPEG/TIFF/DNG, which is the requirement HEIC imposes. **Confirm before adopting**: its license, its HEIC path against a real fixture, and whether it exposes Apple maker notes — the last of those decides whether Live Photo pairing gets content identifiers or falls back to stem matching (1.6). Single-maintainer risk is real and is mitigated by keeping every call behind one interface (1.9), so a replacement is one file. |
| `github.com/dsoprea/go-exif/v3` (MIT, pure Go) | **Reject as primary.** JPEG and TIFF only without the companion `-image-structure` packages, and the HEIC extractor lives in a separate repo in a family that has had availability problems. Acceptable as a second opinion in a fixture test. |
| `github.com/rwcarlsen/goexif` | **Reject.** Unmaintained for years, JPEG/TIFF only, no HEIC. |
| `github.com/barasher/go-exiftool` (wraps Phil Harvey's perl `exiftool`) | **Reject as the runtime path, adopt as the fixture oracle.** exiftool is the reference implementation and it is right about everything, including Apple maker notes. But it is a perl runtime dependency on a box that has none, and a cold invocation costs on the order of 100–200 ms; `-stay_open` amortises that to single-digit milliseconds but adds a long-lived subprocess to supervise. Use it **offline**, on a developer machine, to generate the expected values in `internal/metadata/testdata/`. That gets exiftool's correctness into the test suite without putting perl on the server. |
| `ffprobe -v quiet -print_format json -show_format -show_streams` | **Adopt for `.mov`/`.mp4`.** Already installed, already wrapped with a timeout, a process group and a cancel func in `internal/thumbnail/thumbnail.go` — reuse that machinery rather than writing a second `exec.CommandContext` call site. It is expected to expose `format.tags` including `com.apple.quicktime.creationdate`, `com.apple.quicktime.location.ISO6709`, `com.apple.quicktime.content.identifier` and `creation_time`; **the content-identifier key in particular must be proven against a real fixture**, because the pairing design leans on it. |
| `trimmer-io/go-xmp` | **Not needed.** Only the export path emits XMP, and that is a small fixed template; a dependency to write four elements is not worth it. |

**Cost budget.** A pure-Go EXIF parse reads the first tens of kilobytes and walks a few
hundred IFD entries: low single-digit milliseconds of CPU, dominated by the disk seek. An
`ffprobe` spawn is tens of milliseconds of process setup plus the parse. For a 50,000-file
camera roll that is roughly a couple of minutes of CPU for the stills and a few minutes for
the videos, spread across a batched, cancellable, two-worker pass — the same shape and the
same budget as the existing thumbnail backlog, which is already known to be survivable on
this hardware.

**The seam.** Everything above sits behind one interface in a new `internal/metadata`
package:

```go
type Extracted struct {
    CapturedLocal    string   // "2024-06-14 18:32:07.412", "" when unknown
    CapturedOffset   *int     // minutes east of UTC, nil when unrecorded
    CapturedSource   string   // "exif" | "quicktime" | "exif_digitized" | "quicktime_mvhd" | ""
    Lat, Lon         *float64
    AltitudeM        *float64
    AccuracyM        *float64
    GPSSource        string   // "exif" | "quicktime" | ""
    ContentID        string   // Apple ContentIdentifier, "" when absent
}

type Extractor interface {
    Extract(ctx context.Context, path, ext string) (Extracted, error)
}
```

One interface, two implementations behind a dispatch on extension, and a `Extracted` that is
all-pointers-for-optional so "absent" and "zero" are different values. `0.0, 0.0` is a real
coordinate in the Gulf of Guinea, and a non-pointer float cannot say "no location".

### 1.9 Where extraction runs (T4)

**Not in the scanner.** `Scanner.scanFolder` walks every file in every managed folder on the
catalog-sync tick, holds the scan mutex for the whole pass, and does one `UpsertFile` per
entry. Adding a file open and a header parse to that loop means re-reading every file's
header on every tick — 50,000 opens against a spinning pool every thirty minutes — and it
would do it while holding the lock that a manual rescan needs. The scanner is cheap
per-file *by design* and must stay that way.

**In the derived-data pass, beside the thumbnail pass but not inside it.**
`Daemon.generateThumbnails` already has every property this needs: it runs off the select
loop under a `CompareAndSwap` guard so passes cannot overlap, it pulls bounded batches
(`thumbnailBatchSize = 200`, `maxThumbnailBatchesPerPass = 500`), it resolves source paths
for archived files out of cold storage as well as on-device files, it is cancellable, and it
has a three-strikes failure budget (`thumb_attempts`, `RecordThumbnailFailure`,
`ResetThumbnailFailures` on a manual rescan).

Add a sibling `Daemon.extractMetadata` with:

- Its own queue predicate: `Catalog.FilesNeedingMetadata(exts, limit)` selecting
  `meta_scanned_at IS NULL AND meta_attempts < 3 AND status IN ('synced','archived')`.
- Its own attempt budget: `meta_attempts`, `meta_last_attempt`, `meta_error`, cleared by the
  same manual-rescan path that clears thumbnail failures.
- The same worker-pool helper, at the same `thumbnail_workers` concurrency.
- **Metadata first, thumbnails second**, in the same background goroutine under the same
  `CompareAndSwap` guard. Metadata is what the UI sorts and groups by, it is two orders of
  magnitude cheaper than a thumbnail, and a fresh install should get dates before it gets
  pictures.

**Why two passes and not one fused pass**, given that fusing would open each file once: the
two operations have different eligible extension sets (`.dng` and `.aae` have metadata and no
thumbnail; a `.pdf` has neither), different queue predicates, and — decisively — different
failure profiles. A video whose ffmpeg thumbnail times out after 30 seconds would, fused,
burn a metadata attempt on the same tick; three timeouts and the file has neither a
thumbnail nor a capture date, even though its metadata read costs four kilobytes and cannot
fail. One saved `open()` is not worth coupling two failure budgets.

**Extraction never overwrites a value the owner typed.** `SetExtractedMetadata` writes only
where `captured_source <> 'manual'` and `gps_source <> 'manual'`, exactly as `UpsertFile`'s
`ON CONFLICT` list deliberately omits `pinned`. A rescan that clears a hand-entered capture
date is the same class of bug as a rescan that clears a pin.

---

## 2. Schema — migration 6 and migration 7

Follow `internal/catalog/migrations.go` exactly: append-only, ascending, a version number
assigned when the migration lands and never reserved in advance, `schemaVersion` bumped in
the same commit, and **migrations 2 and up written bare** so a runner bug that replays one
fails loudly.

Two migrations, not one, because they have different shapes: **6 is all additive** (`CREATE
TABLE` and `ALTER TABLE ADD COLUMN`, fast, safe, no table rewrite) and **7 rebuilds the
`jobs` table** to widen a `CHECK` constraint, which is a different risk profile and should be
landable and revertable on its own.

### 2.1 `tags` and `file_tags` (T1)

```sql
CREATE TABLE tags (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,          -- display form: "Trips/Iceland/Reykjavik"
	name_fold  TEXT NOT NULL UNIQUE,   -- casefolded: "trips/iceland/reykjavik"
	created_at INTEGER NOT NULL
);

CREATE TABLE file_tags (
	file_id    INTEGER NOT NULL,
	tag_id     INTEGER NOT NULL,
	applied_at INTEGER NOT NULL,
	PRIMARY KEY (file_id, tag_id)
) WITHOUT ROWID;

CREATE INDEX idx_file_tags_tag ON file_tags(tag_id, file_id);
```

**`WITHOUT ROWID`** on `file_tags` for the same reason `job_items` has it: the composite key
is nearly the whole row, and it gives free deduplication when a bulk request names the same
file twice.

**No `REFERENCES` clause**, matching migration 4's comment: foreign keys are off per
connection by default and the DSN does not enable them, so the clause would parse, be
stored, and do nothing. Referential integrity is maintained explicitly — and there is very
little to maintain, because `files` rows are **never deleted** (a vanished file becomes
`status = 'missing'`; the only `DELETE FROM` statements in the tree are on
`persistent_paths`, `jobs` and `job_items`). Deleting a tag deletes its `file_tags` rows in
the same transaction.

**Two name columns.** `name` is what the owner typed and what the UI renders.
`name_fold` is `strings.ToLower` of it and carries the UNIQUE constraint, so `Iceland` and
`iceland` are one tag. Case folding happens **in Go**, not with SQLite's `COLLATE NOCASE`,
which is ASCII-only and would treat `Ísland` and `ísland` as different tags.

**Normalisation on the way in**, applied to every segment before folding: trim surrounding
whitespace, collapse internal runs of whitespace to one space, reject empty segments, reject
control characters, reject a leading or trailing `/`, cap each segment at 64 characters and
the whole path at 255. A tag that normalises to something already present is that tag, not a
new one.

### 2.2 Hierarchy — materialised path, not adjacency (T1)

`tags.name` holds the full path with `/` separators. There is **no `parent_id`**.

- **Descendant filter is a prefix scan:** `name_fold = ?` OR `name_fold LIKE ? || '/%'`.
  Both are index-usable against the UNIQUE index on `name_fold`.
- **Renaming a subtree is one statement:**
  `UPDATE tags SET name = ? || substr(name, ?), name_fold = ? || substr(name_fold, ?)
   WHERE name_fold = ? OR name_fold LIKE ? || '/%'`.
- **The wire format matches Lightroom's** `lr:hierarchicalSubject`, modulo `/` for `|`, so
  the export of 1.7 is a separator swap rather than a tree walk.

Rejected alternative: an adjacency list with `parent_id`. Every descendant filter becomes a
recursive CTE, run on a single SQLite connection that the quota tick and the job worker are
queued behind, to answer a question a prefix scan answers with an index seek. The tree is a
few hundred nodes deep-by-two; it does not need a graph.

**Every ancestor is materialised.** Creating `Trips/Iceland/Reykjavik` creates `Trips` and
`Trips/Iceland` as rows too, in the same transaction, so autocomplete can offer the
intermediate levels and the tag manager can render a tree without inferring nodes.

**Applying a leaf does NOT apply its ancestors to the file.** `file_tags` gets exactly one
row, for the leaf. Ancestor matching is a *query-time* concern, handled by the prefix
predicate. The alternative — writing a row per ancestor — triples the write volume of a bulk
tag and produces the confusing state where removing `Trips` from a photo leaves
`Trips/Iceland` on it.

### 2.3 `files` — everything else (T2)

Descriptions and derived metadata are columns on `files`, not a side table. `files` rows are
never deleted, so a 1:1 side table would buy nothing except a `LEFT JOIN` on every listing
query. Tags are *not* a column, because they are genuinely many-to-many.

The `description` cap is **2000 characters**, enforced in the handler. That is a caption, not
an essay, and it bounds the worst case of a maximal page: `GET /files?limit=500` with every
row at the cap is about 1 MB of JSON, which is acceptable on a LAN and is not reachable in
practice.

### 2.4 Migration 6, in full

```sql
-- migration6TagsAndMetadata

CREATE TABLE tags (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	name_fold  TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
);

CREATE TABLE file_tags (
	file_id    INTEGER NOT NULL,
	tag_id     INTEGER NOT NULL,
	applied_at INTEGER NOT NULL,
	PRIMARY KEY (file_id, tag_id)
) WITHOUT ROWID;

CREATE INDEX idx_file_tags_tag ON file_tags(tag_id, file_id);

-- Description. TEXT NOT NULL DEFAULT '' rather than nullable: "" and NULL would
-- be two spellings of "no description" and every predicate would need both.
ALTER TABLE files ADD COLUMN description TEXT NOT NULL DEFAULT '';

-- Capture. captured_local is a naive wall clock, "YYYY-MM-DD HH:MM:SS[.sss]",
-- lexicographically sortable. captured_offset_minutes is NULL when the file did
-- not record one -- it is NOT 0, because 0 is UTC and that is a real answer.
-- No derived UTC epoch is stored (T3).
ALTER TABLE files ADD COLUMN captured_local           TEXT NOT NULL DEFAULT '';
ALTER TABLE files ADD COLUMN captured_offset_minutes  INTEGER;
ALTER TABLE files ADD COLUMN captured_source          TEXT NOT NULL DEFAULT '';

-- Location. WGS84 signed decimal degrees. NULL means unknown; 0.0 is a real
-- coordinate and must not be usable as a sentinel.
ALTER TABLE files ADD COLUMN gps_lat        REAL;
ALTER TABLE files ADD COLUMN gps_lon        REAL;
ALTER TABLE files ADD COLUMN gps_altitude_m REAL;
ALTER TABLE files ADD COLUMN gps_accuracy_m REAL;
ALTER TABLE files ADD COLUMN gps_source     TEXT NOT NULL DEFAULT '';

-- Live Photo / sidecar pairing (T9). pair_key is namespaced: "cid:<uuid>" or
-- "stem:<dir>/<stem>", so the two derivations cannot collide.
ALTER TABLE files ADD COLUMN pair_key    TEXT NOT NULL DEFAULT '';
ALTER TABLE files ADD COLUMN pair_source TEXT NOT NULL DEFAULT '';

-- Extraction bookkeeping, mirroring migration 5's thumbnail failure columns
-- exactly. meta_scanned_at is a nullable INTEGER epoch, not a DATETIME:
-- ALTER TABLE ADD COLUMN rejects NOT NULL DEFAULT CURRENT_TIMESTAMP.
ALTER TABLE files ADD COLUMN meta_scanned_at   INTEGER;
ALTER TABLE files ADD COLUMN meta_attempts     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN meta_last_attempt INTEGER;
ALTER TABLE files ADD COLUMN meta_error        TEXT NOT NULL DEFAULT '';

-- The extraction queue. Leading column is the null-check target, so the
-- partial-looking scan stays an index range.
CREATE INDEX idx_files_meta_queue ON files(meta_scanned_at, meta_attempts, status);

-- Sorting and grouping by capture date, and the "no capture date" filter.
CREATE INDEX idx_files_captured ON files(folder_label, captured_local, id);

-- Bounding-box location queries.
CREATE INDEX idx_files_gps ON files(gps_lat, gps_lon);

-- Pair-group expansion. Folder-scoped: a stem key is only meaningful inside one
-- folder, and a content id from two devices could in principle repeat.
CREATE INDEX idx_files_pair ON files(folder_label, pair_key);

-- A cosmetic folder name that is not a join key (T8, section 6.5).
ALTER TABLE folder_configs ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
```

### 2.5 Migration 7 — widening the `jobs.action` CHECK

`jobs` carries `CHECK(action IN ('restore','archive','scan'))`. SQLite cannot alter a CHECK
constraint; adding a `rename` action requires the documented twelve-step table rebuild. This
is the one migration in this document that is not additive, and it is why it is separate.

```sql
-- migration7RenameJobs
--
-- The twelve-step ALTER recipe, minus the steps that do not apply: foreign keys
-- are off (migration 4), there are no triggers and no views on jobs, and the
-- indexes are recreated explicitly below. legacy_alter_table is not needed
-- because nothing REFERENCES jobs.

CREATE TABLE jobs_new (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	action       TEXT NOT NULL CHECK(action IN ('restore','archive','scan','rename')),
	folder_label TEXT NOT NULL DEFAULT '',
	state        TEXT NOT NULL DEFAULT 'pending'
		CHECK(state IN ('pending','running','done','failed')),
	total        INTEGER NOT NULL DEFAULT 0,
	done         INTEGER NOT NULL DEFAULT 0,
	failed       INTEGER NOT NULL DEFAULT 0,
	error        TEXT NOT NULL DEFAULT '',
	payload      TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	started_at   INTEGER,
	finished_at  INTEGER
);

INSERT INTO jobs_new (id, action, folder_label, state, total, done, failed, error,
                      payload, created_at, started_at, finished_at)
	SELECT id, action, folder_label, state, total, done, failed, error,
	       '', created_at, started_at, finished_at FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

CREATE INDEX idx_jobs_state ON jobs(state, id);
```

`payload` is a JSON blob and is used **only** by the `rename` action, which stores
`{"from":"…","to":"…"}`. It is `TEXT NOT NULL DEFAULT ''` rather than nullable so no reader
has to handle two spellings of empty. `jobColumns` gains it and `scanJob` scans it, which is
the two-edit change `fileColumns` was refactored to make possible.

Because a `rename` job's payload changes what the job *does*, `ResumeInterruptedJobs` must be
able to re-read it — which it can, since it comes from the row. The rename operation is
idempotent (section 6.3), so a resumed rename that finds the work already done is a no-op
success, per invariant 12.

### 2.6 `fileColumns`, `FileRecord`, and the tag hydration rule

`fileColumns` is the single select list every `SELECT` on `files` uses, and `scanFile` is its
only consumer. Migration 6's fifteen columns are appended to it, in DDL order, and to
`scanFile` and `FileRecord` in the same commit.

`FileRecord` gains:

```go
	Description      string     `json:"description"`
	CapturedLocal    string     `json:"captured_local"`            // "" when unknown
	CapturedOffsetMin *int      `json:"captured_offset_minutes"`   // null, never omitted
	CapturedSource   string     `json:"captured_source"`
	GPSLat           *float64   `json:"gps_lat"`                   // null, never omitted
	GPSLon           *float64   `json:"gps_lon"`
	GPSAltitudeM     *float64   `json:"gps_altitude_m,omitempty"`
	GPSAccuracyM     *float64   `json:"gps_accuracy_m,omitempty"`
	GPSSource        string     `json:"gps_source"`
	PairKey          string     `json:"pair_key"`
	PairSource       string     `json:"pair_source"`
	MetaScannedAt    *int64     `json:"meta_scanned_at"`
	MetaAttempts     int        `json:"meta_attempts"`
	MetaError        string     `json:"meta_error,omitempty"`
	Tags             []string   `json:"tags"`                      // hydrated, not a column
```

Design section 6.0's serialisation rules apply: the four fields that answer a *question*
(`captured_offset_minutes`, `gps_lat`, `gps_lon`, `meta_scanned_at`) are `null` and never
omitted, because "absent from the JSON" and "we do not know" must not be the same wire
state. Altitude and accuracy are decorative and may be omitted.

**The tag hydration rule.** `Tags` is never selected per row. `ListFiles` runs **one**
additional query per page:

```sql
SELECT ft.file_id, t.name
FROM file_tags ft JOIN tags t ON t.id = ft.tag_id
WHERE ft.file_id IN (<= 500 placeholders)
ORDER BY ft.file_id, t.name_fold
```

and stitches the result in Go. One page, two queries, regardless of page size. `Tags` is
always non-nil — `[]` for an untagged file, never `null`, so the frontend never writes
`file.tags?.map`.

---

## 3. Search and filtering — and why not FTS5 (T6)

Today `FileQuery.Search` is `AND filename LIKE '%' || ? || '%'`. The interview settled that
the fuzzy finder should cover filenames, paths, folder, extension and status, and that tags
and descriptions are the new fields that need to exist.

### 3.1 The extended `q` predicate

```sql
AND ( filename      LIKE ?
   OR relative_path LIKE ?
   OR description   LIKE ?
   OR EXISTS (SELECT 1 FROM file_tags ft JOIN tags t ON t.id = ft.tag_id
              WHERE ft.file_id = files.id AND t.name_fold LIKE ?) )
```

`folder` and `status` and `extension` are **not** folded into `q`: they already have exact,
index-backed parameters (`folder=`, `status=`, `ext=`), and the fuzzy finder composes them
by setting those parameters rather than by matching them as text. A finder that matches the
literal string "archived" against a filename called `archived-notes.txt` is a finder that
lies about what it found.

`LIKE` with a leading `%` cannot use an index; this is a full scan of `files`. That is
acceptable and the numbers say why: SQLite scans on the order of a million simple rows per
second even on modest ARM hardware, the catalog is tens of thousands of rows, and the query
is serialised behind the quota tick anyway — a 20 ms scan is invisible next to a 400 ms
`os.Rename` across a spinning pool. The escape hatch is named in section 10.

### 3.2 New filter parameters

| Parameter | Predicate |
|---|---|
| `tag=trips/iceland` | `EXISTS (… t.name_fold = ? OR t.name_fold LIKE ? \|\| '/%')` — **includes descendants**, because a hierarchy the filter does not respect is a hierarchy nobody uses. |
| `tag=a&tag=b` | Repeated parameter is **AND** — one `EXISTS` per value. Union is what `q` is for; the tag filter is for narrowing. Capped at 8 values. |
| `untagged=1` | `NOT EXISTS (SELECT 1 FROM file_tags WHERE file_id = files.id)`. **This is the parameter that makes the whole feature usable** — a tagging pass is defined by what is not tagged yet. |
| `described=0` / `described=1` | `description = ''` / `description <> ''`. |
| `captured_from=YYYY-MM-DD` | `captured_local <> '' AND captured_local >= ?`. Lexicographic comparison on the naive string; no conversion. |
| `captured_to=YYYY-MM-DD` | `captured_local <> '' AND captured_local <= ? \|\| ' 23:59:59.999'`. |
| `undated=1` | `captured_local = ''`. Distinct from *unscanned*: `meta_scanned_at IS NULL` is "we have not looked yet", `captured_local = ''` with `meta_scanned_at` set is "we looked and there was nothing". |
| `near=lat,lon,km` | Bounding box in SQL (`gps_lat BETWEEN … AND gps_lon BETWEEN …`, using `idx_files_gps`), then an exact haversine filter in Go over the box's rows. The box is a superset of the circle, so the Go pass only ever removes rows. |
| `sort=captured` | `ORDER BY CASE WHEN captured_local = '' THEN 1 ELSE 0 END, captured_local DESC, id ASC` — undated files last in both directions, which is what a photographer wants and what an infinite scroll needs (every sort still ends in `id ASC`, per the existing total-order rule). |

Every one of these goes in `FileQuery.filter()`, so `GET /files` and `GET /files/ids` accept
identical filters and "select all matching" resolves the same set the grid is showing.

### 3.3 Rejecting FTS5

FTS5 is the obvious answer and it is the wrong one here, for four reasons in descending
weight:

1. **It changes the build of the whole project.** `mattn/go-sqlite3` compiles FTS5 only under
   a build tag. That tag would have to be added to `make build`, `make dev`, `make test`,
   `go test -tags e2e`, `go test -tags e2e_live`, the Dockerfile, and all six CI jobs —
   including `fresh-clone-build`, whose entire purpose is to run the deployment runbook's
   build command **unmodified**. A feature that edits that job's command has broken the thing
   the job exists to protect.
2. **FTS5 is bad at the query this needs.** A fuzzy finder wants infix and subsequence
   matching. FTS5 does tokens and prefixes (`abc*`); it does not do infix. Half the queries
   the owner will type would need the `LIKE` fallback anyway, so the FTS index would be a
   second code path that handles some of the traffic.
3. **An external-content FTS table needs triggers on `files`.** Triggers interact with
   `UpsertFile`'s `ON CONFLICT` clause and with the migration recipe, and they are invisible
   at the call site — exactly the property this codebase spends its comments avoiding.
4. **The corpus does not need it.** Tens of thousands of short rows.

**The escape hatch, stated so nobody has to re-derive it:** if the catalog passes roughly
200,000 rows *or* the extended `q` scan is measured above 100 ms on the target hardware,
introduce a contentless FTS5 table over `(filename, relative_path, description, tag_names)`
maintained explicitly by the write paths (not by triggers), behind the **same** `q`
parameter, so no client changes. The API contract is designed to make that swap invisible.

---

## 4. API contract

Conventions from `docs/control-plane-design.md` §6.0 hold: `null` never omitted for fields
that answer a question, `writeError` for every failure, an unmatched `/api/` path is a JSON
404, and no handler scans, moves or blocks on the filesystem.

### 4.1 `GET /api/v1/files` and `GET /api/v1/files/ids` — extended

New query parameters exactly as section 3.2. Both endpoints parse them through the shared
`parseFileFilters`, so they cannot drift. Response records gain the fields of 2.6.

### 4.2 `GET /api/v1/tags`

```
GET /api/v1/tags?folder=CameraRoll&prefix=ice&limit=50
```

```json
[ { "id": 12, "name": "Trips/Iceland", "count": 431, "descendant_count": 512 } ]
```

- `count` is files carrying **exactly** this tag; `descendant_count` includes the subtree.
  Both come from **one** `GROUP BY` over `file_tags`, joined in Go — not from a stored
  counter. This mirrors `PersistentPath.MatchCount`, which is computed per request for the
  same reason: a denormalised counter is a second source of truth that drifts, in exchange
  for an index scan that is already cheap.
- `prefix` is a case-folded prefix match backing type-ahead. It is the only parameter the
  tag prompt sends, and it is index-backed against `name_fold`.
- `folder` restricts the counts (and the returned set) to one folder, so a Books folder's
  prompt does not autocomplete camera-roll tags.
- Ordered by `count DESC, name_fold ASC` — most-used first, which is what makes the first
  `<Tab>` usually right.

### 4.3 `POST /api/v1/files/{id}/tags` and `DELETE /api/v1/files/{id}/tags`

```json
POST { "add": ["Trips/Iceland", "2024"], "remove": ["Untriaged"] }
→ 200 { "file": <FileRecord with tags[]>, "created": ["Trips/Iceland"] }
```

`created` names the tags this request brought into existence, so the UI can say `+ new tag`
rather than silently accepting `icelnad` next to `iceland`. A tag name that fails
normalisation is a `400` naming the offending name — never a silently-dropped element.

### 4.4 `POST /api/v1/files/tags` — bulk

```json
{ "ids": [ … ], "add": ["Trips/Iceland"], "remove": [] }
→ 200 { "updated": 431, "created": ["Trips/Iceland"],
        "undo": { "add": [], "remove": ["Trips/Iceland"], "ids": [ … ] } }
```

**A request, not a job.** Pins are already a request at 5,000 ids; a tag write is strictly
cheaper (no mover mutex, no filesystem, no folder aggregate to recompute). Caps: **5,000
ids** (the same constant shape as `maxBulkPinIDs`, and for the same reason — the request is
cheap but there is no progress reporting to hide a long one behind) and **16 tag names**.

**One transaction, chunked at 500.** The implementation shape, which is the shape
`SetPinnedBulk` already uses so the codebase has one bulk idiom and not two:

1. Normalise and fold every name; resolve them to ids with one `INSERT OR IGNORE` per
   distinct name (including materialised ancestors) plus one `SELECT … WHERE name_fold IN (…)`.
   Bounded by the request's tag count, not by its id count.
2. For each tag, for each chunk of ≤500 ids: one
   `INSERT OR IGNORE INTO file_tags (file_id, tag_id, applied_at) VALUES (?,?,?), (?,?,?) …`.
3. For each removed tag, for each chunk: one `DELETE … WHERE tag_id = ? AND file_id IN (…)`.
4. Commit.

For the worst legal request — 5,000 ids × 16 tags — that is 160 statements, not 80,000. It
is explicitly **not** implemented with `json_each`: that would work on the bundled SQLite,
but it would be the only place in the codebase that depends on the JSON extension being
compiled in, and the chunked form costs nothing.

**The `undo` block** is the exact inverse of what was applied — the ids that actually
changed, not the ids that were requested. It is what lets `u` in the UI be a single POST
with no client-side bookkeeping and no undo stack in the daemon, and it is what makes it
safe for a tag operation to apply without a confirmation dialog (section 5.7).

### 4.5 `PATCH /api/v1/files/{id}` — description and manual capture metadata

```json
{ "description": "…", "captured_local": "1998-07-04 00:00:00", "gps_lat": 64.1, "gps_lon": -21.9 }
→ 200 <FileRecord>
```

Every field optional; an omitted field is untouched (the `UpdateFolderPolicy` pointer
pattern). Writing `captured_local` or the GPS pair sets the corresponding `_source` to
`'manual'`, which is what makes the extraction pass leave it alone forever after. Writing
`""` to `captured_local` clears both the value and the source, returning the file to the
extraction queue.

Validation: `description` ≤ 2000 characters; `captured_local` must match
`YYYY-MM-DD HH:MM:SS` optionally `.sss`, and must be a real date (`time.Parse`, not a
regexp alone — `2024-02-30` matches the shape); `gps_lat` ∈ [−90, 90], `gps_lon` ∈
[−180, 180], and **both or neither**.

### 4.6 Tag lifecycle — `PUT` and `DELETE /api/v1/tags/{name}`

```
PUT    /api/v1/tags/Trips%2FIcland   { "name": "Trips/Iceland" }   → rename, or MERGE
DELETE /api/v1/tags/Trips%2FIceland?cascade=1                       → delete + descendants
```

- **Rename is a subtree rename**, using the single `substr` statement of 2.2.
- **Rename onto an existing name is a merge**, and the response says so
  (`{"merged": true, "files_affected": 431}`). This is the correct behaviour and the one the
  owner will want the first time they notice `Iceland` and `iceland` — sorry, `Iceland` and
  `Icland`. `INSERT OR IGNORE` into the survivor's `file_tags` rows, delete the loser's rows
  and the loser, all in one transaction.
- **Delete without `cascade=1` refuses** (`409`) when the tag has descendants, naming them.
  Deleting `Trips` and silently taking `Trips/Iceland/Reykjavik` with it is not something to
  do on an unqualified DELETE.
- Deleting a tag deletes its `file_tags` rows. It never touches `files`.

### 4.7 `POST /api/v1/folders/{label}/rename` — see section 6

### 4.8 `PUT /api/v1/folders/{label}` — one new field

`display_name` joins `budget_bytes`, `grace_minutes` and `enabled` as a UI-owned field.
`syncthing_id`, `syncthing_path` and `folder_label` remain TOML-owned and are still ignored.

### 4.9 Caps, in one table

| Thing | Cap | Why |
|---|---|---|
| `description` | 2000 chars | A caption. Bounds a maximal page at ~1 MB. |
| Tag segment | 64 chars | |
| Tag full path | 255 chars | |
| Tag depth | 6 segments | A deeper hierarchy is a filesystem, and there is one of those already. |
| `tag=` values per query | 8 | Each is an `EXISTS` subquery. |
| ids per bulk tag request | 5000 | Same shape as `maxBulkPinIDs`. |
| tag names per bulk request | 16 | |
| `GET /tags?limit=` | 500, default 50 | |
| `near=` radius | 500 km | Beyond that the bounding box stops being a useful prefilter. |

---

## 5. The keyboard-first tagging UX

The hard requirement is: *"I want to use this without touching my mouse at all, ever."* The
keyboard layer being built in parallel provides a real vim operator-pending grammar —
counts, motions, operators — with leader = space. This section specifies how tagging **fits
into** that grammar rather than bolting a second keymap beside it.

### 5.1 The composition rule

An operator-pending grammar is `[count] operator [count] motion`, where the operator applies
to everything the motion covers. That is exactly the right shape for tagging, because the
unit of work in a culling session is "these photos, that tag".

Three obligations follow, and they are the whole of this section's discipline:

1. **Tagging contributes operators and text objects, not one-off chords.** If tagging needs
   a bespoke `Ctrl+Alt+T`, the grammar has failed.
2. **Every operator is repeatable with `.`** and undoable with `u`. Vim users press both
   without thinking, and a grammar where they do nothing is a costume.
3. **A text input is a MODE, not a focus state.** The current `Gallery.tsx` guards its window
   shortcuts by checking `target.tagName === "INPUT"`. That check is right for what it does
   and is not enough here: it cannot distinguish "the owner entered insert mode on purpose"
   from "a re-render moved focus into an input". The keyboard layer must own an explicit
   mode variable, and the tagging surfaces must read it.

### 5.2 The tag operator

| Keys | Effect |
|---|---|
| `t{motion}` | Tag every file the motion covers. Opens the tag prompt. |
| `tt` | Doubled operator = "this one", exactly as `dd` and `yy` |
| `3tj` / `t3j` | Tag the current file and the three below |
| `tG` | Tag from here to the end of the loaded set |
| `t}` | Tag to the end of the current capture-day group |
| `T{motion}` | **Untag.** The prompt lists only tags actually present on the covered set. |
| `.` | Repeat the last tag operation on the current target — the fastest path in a culling run |
| `u` | Undo the last tag write (section 5.7) |

**On claiming `t` and `T`.** In vim these are the till-motions. A photo grid has no
character line to move within, so the till-motions have no meaning here and are not
implemented; the document says so rather than leaving a user to discover it. Tagging is the
single most frequent action in a photo-vault session and it earns a home-row key. **If the
parallel keyboard layer has already spent `t`, the fallback is `gt`/`gT`** — a `g`-prefixed
custom operator is the idiomatic way to add one without a collision (`gc`, `gq`, `gU`), and
vim's own `gt`/`gT` mean "next/previous tab", which this app does not have. That choice
belongs to the keyboard layer, not to this document; both spellings are acceptable and the
issue tracking this names both.

**Motions tagging adds** (they are motions, so every operator gets them for free):

| Keys | Motion |
|---|---|
| `}` / `{` | Next / previous **capture-day boundary** |
| `]t` / `[t` | Next / previous file sharing **any tag** with the current one |
| `]u` / `[u` | Next / previous **untagged** file — the culling workhorse |
| `]p` / `[p` | Next / previous file in a different **pair group** (skips a Live Photo's `.MOV`) |

### 5.3 The tag prompt

`t{motion}` opens a **single-line prompt at the bottom of the viewport**, styled as a vim
command line, not a modal dialog. A dialog steals focus, needs a close button, traps `Tab`,
and is the thing a keyboard-first user is trying to escape.

```
 tag 47 files ▸ trips/ice▏          trips/iceland (431)  ·  + new tag
```

Rules:

- **It states the count before you type.** `tag 47 files`, never a bare cursor. A bulk
  operation must never look like a single one.
- `<Tab>` / `<C-n>` / `<C-p>` cycle completions from `GET /tags?prefix=&folder=`, debounced
  at 120 ms and aborted with an `AbortController` on every keystroke (the generation-counter
  pattern `useFiles` already uses — a stale completion list is worse than none).
- `<CR>` commits. `<Esc>` cancels **the whole operation**; nothing is written.
- `,` or `;` commits the current token and keeps the prompt open, so
  `iceland,reykjavik,2024<CR>` is three tags in one prompt without three round trips.
- `/` builds hierarchy as you type; the completion list shows the subtree.
- A name with no match shows `+ new tag` **before** you commit. Silently creating `icelnad`
  next to `iceland` is how a vocabulary rots, and the fix is one word of feedback.
- `<C-w>` deletes the previous word, `<C-u>` clears the line. These are free if the prompt is
  a real input; they are worth asserting in a test because a custom key handler can eat them.

`T{motion}` uses the same prompt with an inverted completion source: only tags that appear on
the covered set, each with the count of covered files carrying it
(`removing "Untriaged" from 47 of 47`).

### 5.4 Tag mode — the culling surface

`<Space>t` from the gallery enters **Tag mode**, the analogue of Photo Mechanic's
contact-sheet-plus-preview and Lightroom's Library Loupe. Dark-only, no chrome:

```
┌──────────────────────────────────────────┬──────────────────────┐
│                                          │ IMG_1234.HEIC        │
│                                          │ 14 Jun 2024 18:32    │
│              large preview               │ +02:00 · EXIF        │
│                                          │ 64.1466, -21.9426    │
│                                          │ ─────────────────    │
│                                          │ trips/iceland        │
│                                          │ 2024                 │
│                                          │ ─────────────────    │
│                                          │ description…        │
├──────────────────────────────────────────┴──────────────────────┤
│ ▸ 231 / 4,812   ·   47 untagged ahead   ·   [,] carry: on       │
└─────────────────────────────────────────────────────────────────┘
```

| Keys | Effect |
|---|---|
| `j` / `k` | Next / previous file. The only navigation that matters. |
| `J` / `K` | Next / previous **untagged** file (equivalent to `]u` / `[u`) |
| `gg` / `G` | First / last |
| `{` / `}` | Previous / next capture day |
| `i` | Enter the description editor (5.5) |
| `t` / `T` | Tag / untag the current file — the operator, with an implicit `tt` |
| `1`–`9` | Apply the Nth tag from the **carry set** without opening the prompt |
| `,` | Toggle **carry-forward** |
| `<Space><Space>` | Toggle selection of the current file (feeds 5.6) |
| `<Esc>` | Leave Tag mode, returning to the same file in the gallery |

**Carry-forward** is the single largest speed win in a real culling run, and it is what Photo
Mechanic's stationery pad and Lightroom's "sync metadata" are for. With `,` on, the tags you
applied to the previous file become a *pending set* on the next one; `<CR>` accepts it, any
other key discards it. It is off by default and its state is on screen at all times, because
a sticky write the owner has forgotten about is how 400 photos end up tagged `iceland`.

**Prefetching.** Tag mode must never wait on the network to advance. It prefetches the full
image for `index+1` and `index+2` and holds the previous two, and it calls `loadMore()` when
within five of the end — the rule the Lightbox already follows.

### 5.5 The description editor, and the key-swallowing rule

This is the specific failure the owner named. The answer is a mode, and an explicit exit that
also advances.

- `i` on the focused file enters **description insert**. A `<textarea>` takes focus and the
  keyboard layer's mode becomes `insert`. From that moment `j` types the letter j — which is
  **correct**, and is the behaviour a vim user expects, because they typed `i` on purpose.
- `<Esc>` commits and returns to normal mode.
- **`<C-j>` commits and advances to the next file, staying in insert.** `<C-k>` does the same
  backwards. This is the professional-tool behaviour and it is the whole reason the mode is
  usable: you caption a frame, press `<C-j>`, and you are captioning the next one without
  ever leaving the keyboard or leaving insert.
- `<C-t>` opens the tag prompt **from inside** the description editor, commits the tag, and
  returns to insert at the same cursor position. Tagging and captioning are one pass, not two.
- **Autosave**: on `<Esc>`, on `<C-j>`/`<C-k>`, on blur, and on a 1.5 s idle debounce. A
  caption lost to a closed tab is a caption that gets retyped once and then never written
  again.
- **Never bind bare `j`/`k` while mode is `insert`.** Not "while an input has focus" — while
  the *mode* is insert. The target-tag check in `Gallery.tsx` stays as a second line of
  defence, but the mode variable is the contract.
- `<C-c>` and `<Esc>` both leave; `<C-c>` **discards** uncommitted text and says so in the
  status line. Two ways out with different semantics is vim; one way out that sometimes
  discards is a bug.

### 5.6 Multiselect and bulk tagging

Selection already exists (`useSelection`, ids not indices, anchored ranges). The keyboard
layer maps onto it:

| Keys | Effect |
|---|---|
| `v` | Enter visual selection at the cursor |
| `v{motion}` | Extend — `vG`, `v}`, `v10j` |
| `V` | Select the whole current capture-day group |
| `<Space>a` | Select **all matching the current filter** — `GET /files/ids`, up to 20,000 |
| `<Space><Space>` | Toggle the file under the cursor |
| `<Esc>` | Clear the selection (already bound) |
| `t` / `T` in visual mode | Apply the tag operator to the selection |

**Selection wins over the cursor.** When a selection is non-empty, `t` applies to the
selection and the prompt says `tag 431 files`, not `tag 1 file`. There is no separate
"bulk tag" key, because a second key for the same operator is exactly the kind of one-off
chord rule 1 of 5.1 forbids.

**Above 20,000 the ids endpoint truncates** and reports `truncated: true`; the prompt must
say `tag 20,000 of 47,331 files (the rest will not be tagged)` rather than implying it
covered everything. That is the same honesty rule as the restore preflight.

### 5.7 Undo, and why tagging never confirms

An archive is a Syncthing deletion on every device, so it confirms. **A tag write moves no
bytes and is perfectly reversible, so it never confirms** — a confirmation dialog on a
reversible action is a keystroke tax that trains the owner to hit `<CR>` without reading,
which is what makes the *irreversible* dialogs stop working.

What makes that safe is that undo is real:

- The bulk endpoint returns `undo`: the exact inverse of what it actually applied (4.4).
- The client holds the **last one**, and `u` posts it. One level, no stack, no daemon state.
- The toast reports the truth — `tagged 4,231 files · u to undo` — and stays for the 20
  seconds `ToastProvider` already gives errors.
- After `u`, `<C-r>` re-applies (the inverse of the inverse, which the same endpoint returns).

One level is deliberate. A deeper stack needs invalidation rules for "someone else changed
this file", and there is no someone else on a single-owner LAN daemon — but there *is* an
extraction pass and a quota tick, and a five-deep undo that silently no-ops because a row
moved is worse than an undo that only ever promises one step.

### 5.8 The `data-*` selector contract

The browser tier drives the embedded UI through `data-*` attributes
(`web/e2e/helpers.ts`). Tagging adds:

| Attribute | On |
|---|---|
| `data-tag-mode` | The Tag mode root, `"on"` / absent |
| `data-tag-prompt` | The prompt container |
| `data-tag-prompt-count` | Text is the count the operator will affect |
| `data-tag-completion="<name>"` | Each completion row |
| `data-tag-chip="<name>"` | Each tag chip on a tile or in the panel |
| `data-description-editor` | The textarea |
| `data-capture-date` | The rendered capture time; attribute value is the raw `captured_local` |
| `data-capture-source` | `"exif"` / `"quicktime"` / `"manual"` / `""` |
| `data-pair-group="<pair_key>"` | Tiles belonging to a pair group |
| `data-keymode` | The keyboard layer's current mode — `"normal"` / `"insert"` / `"visual"` / `"prompt"` |

`data-keymode` is not decorative: it is the only way a browser test can assert that `j` does
not navigate while a caption is being typed, which is the regression this whole section
exists to prevent.

---

## 6. Renaming (T8)

### 6.1 Three different things are called "rename"

| | What it is | Verdict |
|---|---|---|
| **(a)** A directory **inside** a Syncthing folder: `CameraRoll/2024/01` → `CameraRoll/2024/Iceland` | A real filesystem rename that Syncthing propagates to every device | **Build it.** This is what the owner asked for. |
| **(b)** The coldstore **folder label**: `CameraRoll` → `Photos` | A join key in five tables and a path component in two on-disk trees | **Refuse it.** Offer `display_name` instead (6.5). |
| **(c)** The folder's **path on disk**: `/home/u/CameraRoll` → `/srv/photos` | Owned by Syncthing and by `coldstore.toml` | **Already works, needs no code.** (6.6) |

### 6.2 What a directory rename has to rewrite

`relative_path` is the coordinate everything else is expressed in. Renaming a directory
changes it for every file beneath, and that coordinate appears in **three trees and two
columns**:

| Where | Shape |
|---|---|
| The device | `{syncthing_path}/{relative_path}` |
| Cold storage | `{cold_storage_path}/archive/{folder_label}/{relative_path}` |
| Thumbnails | `{cold_storage_path}/thumbnails/{folder_label}/{relative_path}.thumb.jpg` |
| `files.relative_path` | the coordinate itself |
| `files.thumbnail_path` | `{folder_label}/{relative_path}.thumb.jpg`, stored as text |

**The hazard that makes this non-optional.** Suppose the owner renames the directory on the
device by hand, over SSH or from another synced machine. The next scan finds the files at
their new paths and inserts new rows; the old `synced` rows are marked `missing`. That is
survivable. But **archived rows are not swept** — `MarkFilesMissingBeforeGeneration` only
touches `status = 'synced'`, correctly, because an archived file is not supposed to be on
the device. So every archived file under the renamed directory keeps a `relative_path`
pointing at a directory that no longer exists, and the day the owner restores one,
`Mover.RestoreByID` recreates the **old** directory next to the new one and drops the photo
into it. The folder now has `2024/01/` and `2024/Iceland/` and the owner has to work out
why.

That is the argument for making rename a first-class operation rather than something the
owner does with `mv`.

### 6.3 The algorithm

`POST /api/v1/folders/{label}/rename` with `{"from": "2024/01", "to": "2024/Iceland"}`
enqueues a **`rename` job** (migration 7) with `total = 1`, no `job_items`, and the from/to
in `jobs.payload`. It is a job and not a synchronous request for exactly one reason: the
existing `ResumeInterruptedJobs` machinery then gives it crash durability for free, and this
operation has a window in which the filesystem and the catalog disagree.

The worker runs it under the **mover mutex**, so no move of any file under `from` can be in
flight.

**Validation, before anything is touched:**

1. `from` and `to` are cleaned, are relative, contain no `..` segment, and do not escape the
   folder root. A rename is a path-traversal primitive if this is skipped.
2. `to` must not already exist in **any** of the three trees. Invariant 10: never overwrite.
3. `from` must exist as a directory in at least one tree. (It may be absent from the device
   if every file beneath it is archived — that is a legal and expected state.)
4. No file under `from` is in a transient status (`archiving`, `restoring`). A rename racing
   a move is a `409` that says which file, not a coin flip.
5. `to` must not be a descendant of `from`.

**Execution, in this order.** Every step is a no-op when its source is absent and its
destination is present, which is what makes the whole job idempotent and therefore safe to
resume (invariant 12):

1. `os.Rename(device/from, device/to)`
2. `os.Rename(archive/label/from, archive/label/to)`
3. `os.Rename(thumbs/label/from, thumbs/label/to)`
4. One transaction:
   ```sql
   UPDATE files
      SET relative_path  = ? || substr(relative_path, ?),
          thumbnail_path = CASE WHEN thumbnail_path = '' THEN ''
                                ELSE ? || substr(thumbnail_path, ?) END
    WHERE folder_label = ?
      AND (relative_path = ? OR relative_path LIKE ? || '/%')
   ```
5. Recompute `pair_key` for the affected rows whose `pair_source = 'basename'` — a
   stem-derived key embeds the directory, and 4,000 stale keys is 4,000 broken pair groups.
6. `LogEvent("folder_renamed", label, from, "renamed to " + to)`.

**Three renames and two statements, not 4,000 row updates.** `os.Rename` on a directory is
atomic within a filesystem, and the catalog rewrite is one `UPDATE`. That is why this is
cheap enough to feel instant even on 4,000 files, and why a per-item job with `job_items`
would be strictly worse.

**Crash between steps.** Each step is independently idempotent, so a resumed job re-runs the
whole sequence and each completed step is skipped. The one genuinely bad interleaving —
filesystem renamed, catalog not — is exactly what `ResumeInterruptedJobs` exists to close,
and the job's payload is on the row so the resumed worker knows what it was doing.

**`persistent_paths` are NOT rewritten.** A glob is something the owner wrote, and silently
editing it is a surprise in a file that decides what never gets archived. Instead the
response carries a warning naming every pattern that started with the old prefix and now
matches nothing, and the config panel already renders a per-pattern `match_count` so a
pattern protecting 0 files is visible. The owner fixes it, having been told.

**Single-file rename** is the same endpoint with a `from` that resolves to a file. Two extra
rules: `files.filename` and `files.extension` are rewritten too, and **if the extension
changes**, `CanThumbnail` may change, so the row's `thumbnail_path` is cleared,
`thumb_attempts` and `meta_*` are reset, the old thumbnail is deleted, and both derived-data
passes pick the file up again. And renaming one member of a pair group whose `pair_source`
is `'basename'` **breaks the pairing** — so a single-file rename inside such a group either
renames the whole group (keeping the shared stem) or is refused with a `409` explaining
which files it would separate. Renaming the whole group is the default.

### 6.4 Why the label is not renameable, and a bug that exists today

`folder_label` is the join key in `files`, `folder_configs`, `persistent_paths`, `jobs` and
`events.folder`, and it is a path component in `archive/{label}/` and `thumbnails/{label}/`.
It is also **TOML-owned**: `ReconcileFolderFromTOML` upserts by label at every startup and
`MarkFoldersAbsentFromTOML` flags anything not declared.

So a label rename in the UI would survive exactly until the next restart, at which point the
TOML label would insert a **new** `folder_configs` row and the renamed one would be flagged
`present_in_toml = 0`, orphaning its budget, its globs and every file, pin, tag and
description under it.

**And this footgun exists right now, without any UI for it.** If the owner edits `label` in
`coldstore.toml` — a plausible thing to do; nothing warns against it — coldstore sees one
folder that disappeared and one that appeared. The old folder's rows are kept (correctly:
`MarkFoldersAbsentFromTOML` never deletes) and stay browsable, and the new label re-scans
**the same directory tree**, producing a complete duplicate catalog: two rows per path under
two labels, two thumbnail trees, and two budgets against one directory. `config.Validate`
already rejects two folders sharing a path *within one TOML file* (`seenPath`), but nothing
checks across runs.

**The guard**, which is a small change and worth landing before any of the rest: during
startup reconciliation, if a declared folder's `syncthing_path` matches an existing
`folder_configs` row under a **different** label, and that row is about to lose
`present_in_toml`, refuse to start with a message naming both labels and telling the owner
that renaming a folder label is not supported and what to do instead. Failing to start is
the right severity: the alternative is a silently duplicated catalog that gets worse every
tick.

### 6.5 `display_name` — the safe version of what was asked for

`folder_configs.display_name TEXT NOT NULL DEFAULT ''`, UI-owned like `budget_bytes`,
editable through `PUT /api/v1/folders/{label}`, rendered in the sidebar, the headings and the
folder switcher. It joins nothing, appears in no path, and is empty by default (in which case
the label is shown). Renaming it is instant, free and cannot corrupt anything — which is the
whole point.

### 6.6 Renaming the folder's path on disk

Already supported and needs no code: move the tree (with its `.stfolder`), update `path` in
`coldstore.toml`, restart. `ReconcileFolderFromTOML` refreshes `syncthing_path` on an
existing row without touching policy, relative paths are unchanged, and the cold-storage and
thumbnail trees are keyed by *label*, not path, so they are unaffected. Worth stating because
it looks like it should be dangerous and is not.

---

## 7. Frontend plan

Everything stays inside the existing constraints: React 19 + Vite + Tailwind 4, no router,
no state library, no data-fetching library, `lib/api.ts` the only place that calls `fetch`,
zero `eslint-disable` in `web/src`, dark only.

**New modules**

| Path | What |
|---|---|
| `web/src/hooks/useTags.ts` | `GET /tags` with a folder-scoped cache, `prefix` completion with abort-on-keystroke, and the mutation wrappers |
| `web/src/hooks/useTagOperator.ts` | The operator state machine: pending operator, count, motion, target resolution, prompt open/closed |
| `web/src/components/TagPrompt.tsx` | The command-line prompt of 5.3 |
| `web/src/components/TagMode.tsx` | The culling surface of 5.4 |
| `web/src/components/DescriptionEditor.tsx` | The insert-mode textarea of 5.5 |
| `web/src/components/TagChips.tsx` | Chips on tiles, in the lightbox, in the Tag mode panel |
| `web/src/components/CaptureMeta.tsx` | Capture time + offset + source + coordinates, with the "timezone not recorded" wording |

**Changed**

- `lib/api.ts`: `tags`, `fileTags`, `bulkTags`, `patchFile`, `renamePath`; `fileFilterParams`
  grows `tag[]`, `untagged`, `described`, `captured_from`, `captured_to`, `undated`, `near`.
  It stays the shared function for `/files` and `/files/ids`.
- `types/api.ts`: the fields of 2.6, plus `Tag`, `BulkTagResponse`, `TagUndo`.
- `useFileWorkspace.ts`: `actions` grows `addTags`, `removeTags`, `setDescription`,
  `setCaptureDate`, `undoLastTag`. Same shape as the existing actions — through `apiFetch`,
  write the returned record back with `patchFile`, report failures as toasts. Two
  implementations of "tag this row" is how one of them ends up without the `res.ok` check.
- `FilterBar.tsx`: two new presets, **Untagged** and **No capture date**, with hints. These
  are the two entry points to a tagging session and they belong where the other six filters
  are.
- `Gallery.tsx`: tag chips on tiles (collapsed to `+3` past two), a `sort=captured` option,
  and capture-day group headers that `{`/`}` navigate between.
- `Lightbox.tsx`: the metadata panel gains capture time, source, coordinates and tags, and
  keeps `file_mtime` **separately labelled** — never merged into one "date" row.

**Tests**

- `make test-web`: prompt key handling (`,` commits a token, `<Esc>` writes nothing,
  `<Tab>` cycles), the count shown before commit, the undo round trip, the query-building
  for every new filter, and — the important one — that `j` typed in the description editor
  produces the letter `j` and does not navigate.
- `make test-playwright`: `scripts/seed-demo` gains a tagged folder, an untagged folder, a
  Live Photo pair, a file with EXIF, a file with GPS, a file with **no** metadata, and a
  file with a manual capture date. Without those fixtures half of this UI is unreachable —
  which is the lesson `scripts/seed-demo` was written to record.

---

## 8. Implementation order

Each step leaves the tree green (`go test ./...`, `make test-web`, `npm run lint`).

| # | Step | Issue | Depends on |
|---|---|---|---|
| 1 | The label-rename guard (6.4). Small, independent, closes a live footgun | #13 | — |
| 2 | `internal/metadata`: the `Extractor` interface, EXIF + ffprobe, the ISO 6709 parser, the precedence rules. Pure package, exiftool-generated fixtures, no schema and no daemon changes | #1 | — |
| 3 | Migration 6, plus `fileColumns`, `scanFile`, `FileRecord`, `types/api.ts`. Schema only | #2 | — |
| 4 | The extraction pass: `FilesNeedingMetadata`, `SetExtractedMetadata`, the failure budget, the manual-rescan reset | #3 | #1, #2 |
| 5 | Pair grouping: `ExpandPairGroups`, wired into `OffloadCandidates` and the job enqueue path, plus the `pair_split` event | #6 | #1, #2, #3 |
| 6 | Tag catalog primitives and CRUD API: normalise/fold, resolve-or-create with ancestors, subtree rename and merge, the counts query, hydration | #7 | #2 |
| 7 | Capture date as a product surface: `sort=captured`, the range filters, day grouping, honest display, manual entry | #4 | #2, #3 |
| 8 | Location: display, `near=`, manual coordinates | #5 | #2, #3 |
| 9 | Descriptions: the `PATCH` endpoint, `described=`, the insert-mode editor | #8 | #2 |
| 10 | Search and filter: the extended `q`, `tag=`, `untagged=` | #9 | #2, #7 |
| 11 | Frontend, non-keyboard first: chips, the prompt, the tag manager, the filter presets. Everything works with a mouse before anything works without one, so a broken key binding is isolated to the keyboard layer | #10 | #7, #9 |
| 12 | Multiselect tagging: the bulk endpoint, the caps, the undo contract, `SelectionBar` | #12 | #7, #9, #10 |
| 13 | Migration 7 + the rename job, and `display_name` | #14 | #2, #6, #13 |
| 14 | The keyboard layer: the operator, the motions, Tag mode, visual-mode bulk, undo. Last, because it depends on every other layer being correct | #11 | #4, #6, #8, #9, #10 |
| 15 | Export: XMP sidecars into `{cold_storage_path}/sidecars/` | not yet filed | #7 |

---

## 9. Edge cases, named

- **A photo with no metadata at all** is the normal case, not an error. `meta_scanned_at` set
  and `captured_local = ''` means "we looked and there was nothing"; `meta_scanned_at IS NULL`
  means "not yet". The UI must render these differently and the `undated=1` filter must select
  only the first.
- **A manual capture date must never be overwritten** by a later extraction pass, and a
  manual GPS pair likewise. Same rule as pins surviving a rescan.
- **`0.0, 0.0` is a real coordinate.** Every optional numeric is a pointer end to end — Go
  struct, JSON, TypeScript — and no code path uses zero as a sentinel.
- **A burst shares one second.** `SubSecTimeOriginal` is why `captured_local` carries `.sss`,
  and `id ASC` is the tiebreak below that.
- **An archived file still gets metadata.** The extraction queue includes `status='archived'`
  and resolves the source out of cold storage, exactly as the thumbnail pass does.
- **A restored file keeps its tags.** Tags key on `files.id`, and a restore does not change
  the id. Archiving, restoring, going missing and coming back all preserve tags — this is a
  consequence of the schema, not a feature to implement, but it is worth a test.
- **A conflicted file (§4.1b) has one row and therefore one tag set.** Resolving in the
  device's favour keeps them; resolving in the archive's favour also keeps them, because both
  resolutions keep the same row.
- **A tag applied to a file that is mid-move** is fine and needs no mutex: the tag write
  touches `file_tags`, and no move reads it (T7). **If that ever stops being true** — if a
  tag is ever allowed to gate offloading, e.g. "never archive `Portfolio`" — then tag writes
  acquire the mover, and this is the sentence that says so.
- **A tag named `2024` and a folder named `2024`** are unrelated namespaces and must not be
  conflated by the fuzzy finder's ranking.
- **Renaming a directory that contains only archived files** succeeds: step 1 finds nothing
  on the device and skips, steps 2–4 do the work. This is the case that motivated the whole
  section.
- **Renaming into a path an `.stignore` covers** would make Syncthing stop syncing the files
  without any error. Out of scope to detect, worth a line in the deployment docs.
- **A `.HEIC` whose `.MOV` was archived before this feature existed** has no `pair_key` until
  extraction runs. The backfill pass computes keys for the whole catalog, and until it
  completes, pair expansion is simply a no-op — degraded, not wrong.
- **Two tags differing only by case, both already in the catalog**, cannot happen after
  migration 6 (UNIQUE on `name_fold`) but *can* be introduced by a future import. The merge
  behaviour of 4.6 is the remedy and it should be reachable from the tag manager.

---

## 10. Reviewed and rejected

**1. Deriving `captured_local` from file mtime when EXIF is absent.**
Rejected. It is one line and it makes every downstream feature wrong in a way nothing can
detect: a "capture date" that is really a copy date sorts a restored backup into the wrong
decade, groups it under the wrong day, and does so silently. Keeping mtime in its own column
and leaving `captured_local` empty costs one `CASE` in one `ORDER BY` and preserves the
distinction the owner explicitly asked for. The UI may *display* mtime as a fallback, clearly
labelled as the file date; the column stays empty.

**2. Storing a derived UTC epoch alongside the naive wall clock.**
Rejected. It is a second source of truth for the same fact, it is undefined for the majority
of files (no `OffsetTimeOriginal`), and populating it by guessing a timezone reintroduces
exactly the silent misfiling that section 1.4 exists to prevent. Every consumer that needs an
instant can compute one from `(captured_local, captured_offset_minutes)` and must refuse when
the offset is NULL.

**3. Writing tags back into the originals, or as XMP sidecars in the Syncthing folder.**
Rejected — section 1.7. The strongest single reason is not corruption risk but propagation:
coldstore's whole mechanism is that a change to a synced folder reaches every device, so a
tagging session becomes a 4,000-file re-sync to the phone and a 4,000-file reset of the
grace window that defers the archiving the product exists to do. Portability is served by
`{cold_storage_path}/sidecars/`, which is outside every synced tree.

**4. Reverse geocoding coordinates to place names.**
Rejected. An online service means 50,000 outbound requests describing exactly where the owner
has been — a privacy leak of precisely the data they said they cared about — and rate limits
that make a backfill take days. An offline dataset means shipping hundreds of megabytes to a
box with a few gigabytes of RAM. The feature that actually delivers the value is **location tags**
plus the `near=` filter: cluster by proximity, select the cluster, tag it
`places/iceland/reykjavik` once. Human-readable, offline, searchable, and the owner's own
vocabulary rather than a gazetteer's.

**5. FTS5 for search.**
Rejected — section 3.3, with a named escape hatch. The decisive reason is that it changes the
build command of the `fresh-clone-build` CI job, whose entire purpose is to run the
deployment runbook's build command unmodified.

**6. A `parent_id` adjacency list for tag hierarchy.**
Rejected — section 2.2. Recursive CTEs on the process's single SQLite connection to answer a
question a prefix index answers with a seek.

**7. A denormalised `usage_count` on `tags`.**
Rejected. A second source of truth that drifts under any write path that forgets it, in
exchange for a `GROUP BY` over an index that is already cheap. `PersistentPath.MatchCount` is
the in-repo precedent: computed per request, never stored.

**8. Routing tag writes through the `Mover`.**
Rejected for now, with the trigger for revisiting stated in section 9. Invariant 2 exists
because a pin changes what quota enforcement may touch. A tag does not. Serialising a
4,000-file tag behind an in-flight cross-device restore buys no safety and costs the owner
the responsiveness that is the entire point of a culling mode.

**9. A confirmation dialog on bulk tagging.**
Rejected — section 5.7. A confirmation on a reversible action trains the owner to dismiss
confirmations, which is what makes the irreversible ones (bulk archive, conflict resolution)
stop working. Real undo plus an honest count in the prompt is the correct trade.

**10. Star ratings and colour labels.**
Rejected as a schema feature, not as an idea. The owner did not ask for them, and if they
are wanted later they are a **reserved tag namespace** — `rating/3`, `flag/reject` — costing
zero schema change and inheriting the filter, the prompt, the bulk path and undo for free.
Adding a `rating INTEGER` column now would be a second, parallel classification system with
its own filter, its own UI and its own migration.

**11. Making the directory rename a per-file job with `job_items`.**
Rejected. A directory rename is three `os.Rename` calls and two SQL statements; expressing it
as 4,000 items would make it slower, non-atomic, and would replace an operation that either
happened or did not with one that can be 63% complete.

**12. Allowing the coldstore folder label to be renamed.**
Rejected — section 6.4. It is a join key in five tables and a path component in two on-disk
trees, and it is TOML-owned, so any UI rename is undone at the next restart in the worst
possible way. `display_name` gives the owner what they actually wanted with none of it.
