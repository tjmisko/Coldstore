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
| T10 | Metadata reading is **hybrid**: pure Go for the bulk capture-time and GPS path, `ffprobe` for QuickTime, and `exiftool` **as a subprocess** — never as an import — for the Live Photo still probe only. |
| T11 | Tagging registers `t` / `T` as operators in the existing grammar. Because an operator carries a **range but not an argument**, there are three tagging paths: picker, repeat, and leader-prefixed quick slots. |

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
  control with a **Location** toggle (added in iOS 15). With it off, the exported copy has
  its **GPS tags removed** while `DateTimeOriginal`, the device model and the camera
  settings all survive — it strips location, not metadata in general. A photo that arrives
  with a date and no location has very often been through this. Reportedly AirDrop, email
  attachments and direct file transfers preserve everything including GPS, and the toggle
  binds only for share targets that honour it. *Unverified against a device; treat the
  behaviour as the likely explanation for missing GPS, not as a rule to code against.*
- **Two settings are routinely confused, and they are not the same setting.**
  - *Settings → Camera → Formats* controls what the camera **records**: **High Efficiency**
    writes `.HEIC` + HEVC `.MOV`; **Most Compatible** writes `.JPG` + H.264 `.MOV`.
  - *Settings → Photos → Transfer to Mac or PC* controls **USB export**: **Automatic**
    transcodes HEIC→JPEG and HEVC→H.264; **Keep Originals** hands over the original bytes.

  There is no "Most Compatible" under *Transfer to Mac or PC*. The distinction matters
  beyond pedantry: **Keep Originals is the setting that preserves Live Photo pairing**, and
  Automatic is a common reason Live Photos arrive as bare videos with no still beside them
  (see 1.6). A transcode does preserve EXIF including GPS, but it is a re-encode and it may
  renumber or rename.

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
added by EXIF 2.31 (2016) precisely to repair it. **[UNVERIFIED]** how long iOS has written
them: "iOS 13 and later" is repeated widely in secondary sources with no authoritative Apple
documentation behind it, so this document does not state it. What *is* **[VERIFIED]** is the
negative — an iOS 5.1.1 original carries `DateTimeOriginal` and no `OffsetTime*`, no GPS and
no maker note — and that is the case the design has to survive anyway. **Absent offset is a
first-class case at every iOS version**, and no code may branch on an assumed one.

For QuickTime containers the equivalents are:

- `com.apple.quicktime.creationdate` — an ISO 8601 string **with an offset**. This is the
  good one, and it has a trap: **Apple writes the offset without a colon** —
  `2026-03-14T15:09:26-0700`, not `-07:00`. `time.RFC3339` will **not** parse that. Use the
  layout `"2006-01-02T15:04:05-0700"`, and accept the colon form as a second attempt for
  non-Apple writers.
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

**The Go trap that makes this decision load-bearing rather than merely tidy.** EXIF readers
hand back a `time.Time`, and when the file recorded no offset they default it to UTC. That
is the right default — it is host-timezone independent — but it means an absent offset and a
genuine `+00:00` are **indistinguishable in the resulting `time.Time`**. Worse, the
`*time.Location` such a reader exposes for `OffsetTimeOriginal` is correctly `nil` when the
tag is absent, and **a nil `*time.Location` prints as `"UTC"`**, because Go maps nil to UTC
in `String()`. Logging it will tell you the photo was taken in UTC when the truth is that
nobody knows. Test the pointer with `== nil`; carry the nil through to a NULL column; never
let the formatted string decide.

The UI is required to render an unknown offset as unknown — "18:32, timezone not recorded" —
and never to render a naive time in the browser's local zone, which is the default behaviour
of `new Date(...)` and would be a lie for every photo taken away from home.

`SubSecTimeOriginal` is appended as `.sss` when present. It matters more than it looks: a
burst writes ten frames inside one second, and without sub-seconds the only tiebreak is `id`,
which is scan order, not shutter order.

**It is a string of fractional digits, not a number, and leading zeros are significant.**
`"05"` means `.05 s` and `"537"` means `.537 s`. Parse it as an integer and `"05"` becomes 5,
which appended as `.5` is ten times wrong and reorders a burst. Concatenate the digits after
the decimal point; never round-trip through a numeric type. (exiftool with `-n` compounds
this by returning `537` as a float64 and `"05"` as a string on the same field, so a fixture
generator has to handle both.)

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
`com.apple.quicktime.location.ISO6709`. **This is not a fixed-width format and an earlier
draft of this document was wrong to say it was.** ISO 6709 admits three encodings of the same
coordinate, and all three occur in files coldstore will actually see:

```
+37.7749-122.4194+52.300/          decimal degrees, unpadded  (exiftool writes this)
+41.1971+019.1686+089.373/         decimal degrees, zero-padded  (iOS writes this)
+372410.339-1220038.701+4.35CRSWGS_84/   degrees-minutes-seconds, in the wild
```

A parser that assumes `±DD.dddd±DDD.dddd` fails on the third form outright and mis-splits
the second. Follow ExifTool's reference parser and try **three regexps in this order**,
first match wins — the order is load-bearing, because the DMS pattern will happily match a
decimal-degrees string and read the wrong digits:

1. decimal degrees — `±DD(.ddd)?±DDD(.ddd)?`
2. degrees + decimal minutes — `±DDMM(.mmm)?±DDDMM(.mmm)?`
3. degrees, minutes, seconds — `±DDMMSS(.sss)?±DDDMMSS(.sss)?`

The rules that hold across all three:

- Every field is **sign-prefixed**, including the first. The sign *is* the field delimiter;
  there is no separator character.
- Latitude precedes longitude, and longitude carries **one more** integer degree digit than
  latitude. That digit count is what disambiguates the three forms.
- The altitude field is **optional**. `+37.7749-122.4194/` is legal.
- A CRS suffix may follow, e.g. `CRSWGS_84`, with or without an intervening `/`. Ignore it,
  but do not choke on it.
- The trailing `/` is mandatory in the standard and present in Apple's output; accept its
  absence anyway rather than rejecting the tag.

Table-driven tests must cover all seven shapes: the three encodings, altitude present and
absent, a CRS suffix, a missing trailing slash, and a southern-hemisphere / below-sea-level
coordinate. Validate the expectations against `exiftool` offline (1.8).

**Two different atoms, both worth reading.** ffprobe exposes
`com.apple.quicktime.location.ISO6709` from the QuickTime **Keys/`mdta`** atom — this is
Apple's — and a plain `location` (plus `location-eng`) from the **`udta` `©xyz`** atom, which
is what Android and other cameras write. They are not two spellings of one tag. Read the
Apple key first and fall back to `location`; a folder that has been through more than one
phone will contain both.

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

They are bound together by an Apple UUID written into both, and **the UUID is the authority
— the shared stem is only a hint**:

- In the `.MOV`, the QuickTime tag `com.apple.quicktime.content.identifier`. **[VERIFIED]**
  present in `ffprobe`'s `format.tags`, which the daemon already runs. This half is free.
- In the `.HEIC`, `ContentIdentifier` inside the **Apple maker note** (`MakerApple` tag
  0x0011) in the EXIF. ExifTool's own `Apple.pm` documents 0x0011 with the comment *"if
  defined, there is a live photo associated with the video"*; Apple's internal name for it is
  `AssetIdentifier`. **[VERIFIED on real data] No pure-Go library reads it.** On a JPEG whose
  genuine identifier is `D4122CE4-…`, ExifTool reads it and `evanoberholster/imagemeta`
  returns `""`. This is not a gap that might be patched around: that library parses
  **embedded** maker-note values (≤4 bytes, inline) correctly and fails every **offset-based**
  one, and a UUID string is always offset-based (1.8). Its own upstream test for this area
  would fail on genuine iPhone output and passes only because it skips.

**So the still half needs `exiftool`, and the design pays for it deliberately and narrowly.**

| Half | Reader | Cost |
|---|---|---|
| `.MOV` | `ffprobe`, already running | free |
| `.HEIC` / `.JPG` of a **Live Photo candidate only** | `exiftool` via `-stay_open` | ~3 ms/file resident |
| Every other still | pure Go | ~18 µs/file |

`-stay_open` is what makes this affordable: a spawned-per-file `exiftool` costs 58.7 ms, of
which ~55 ms is interpreter startup, so a resident process is twenty times cheaper and feeding
it one file at a time costs the same as batching.

**Deploy implication, stated because this project is careful about its runbook:** `exiftool`
becomes a **conditional** runtime dependency, exercised on the small fraction of stills that
look like Live Photo candidates — not a blanket one, and not on the bulk capture-time and GPS
path. Where it is absent the daemon logs once per pass and pairing falls back to the
narrowing heuristic below, which is **degraded and known to be degraded**, not silently
equivalent.

Neighbouring Apple maker-note tags worth knowing about because they answer adjacent
questions: `BurstUUID` (0x000b) groups a burst, and `ImageUniqueID` (0x0015) identifies a
single capture.

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

**The stem is a candidate filter, not a pairing decision.** An earlier draft of this
document made basename matching the fallback *mechanism*, promoted to authority whenever a
content identifier was unavailable. That is withdrawn. Its job now is to decide **which
stills are worth an `exiftool` probe** — which is what keeps the conditional dependency
conditional — and the UUID decides the grouping. Only where `exiftool` is genuinely absent
does a stem-derived key get written, and `pair_source` records that so the UI and the
offloader both know the group is a guess.

**Candidate rules** (a `.MOV` and a still are worth probing together when):

- Same folder, same directory, same case-folded stem, and at least one member is a video
  extension or an `.aae`.
- `IMG_E1234` pairs with `IMG_1234` — strip a leading `E` after the `IMG_` prefix when
  matching, but keep both files' own stems for the key so an edited pair and its original
  form one group of up to four files.
- Capture times, when both are known, must be within 60 seconds. This is what stops
  `notes.txt` and `notes.mov` in a Documents folder being called a Live Photo.

A candidate pair is confirmed by comparing the `.MOV`'s
`com.apple.quicktime.content.identifier` against the still's `ContentIdentifier`. Equal is a
pair; unequal is **not** a pair, however similar the names, and that negative answer is the
whole point of doing the probe.

**What breaks basename matching, and why the content identifier is preferred:** an import
that renumbers on collision, a user rename of one member, a folder merge that puts two
devices' `IMG_1234` in one directory, and any tool that re-encodes the video to `.mp4` with a
different stem. The content identifier survives all of these; the stem survives none of them.

The renumbering case is worth spelling out, because it does not merely fail to pair — **it
pairs the wrong files**. A documented real collision has the true pair being `IMG_1234.HEIC`
+ `IMG_1234(2).MOV`, with an unrelated `IMG_1234.MOV` also in the directory. Stem matching
confidently produces the wrong group, and the consequence is that archiving takes a still
away from its own video while protecting a stranger's. Basename matching is therefore a
cheap **first pass** to narrow candidates, never the decision: where a content identifier
exists on either half it wins, `pair_source` records which rule fired, and the UI says so.

**A transcoding export breaks pairing at the source.** *Transfer to Mac or PC → Automatic*
(1.1) re-encodes HEVC to H.264 and can deliver the video without its still, so a folder can
legitimately contain a bare `.MOV` that never had a partner. An unpaired video is not
evidence of a bug in this code.

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

**Decision: a pure-Go reader for the bulk path, `bep/imagemeta` as a cross-check, `ffprobe`
for QuickTime containers, and `exiftool` as a subprocess for the Live Photo still probe
only.** Everything below was measured or read from source on a developer machine. Claims are
marked **[VERIFIED]** (someone ran it), **[SOURCE]** (read from the library's or ExifTool's
own code) or **[UNVERIFIED]** (secondary sources only). That distinction is kept visible
rather than flattened into prose, because the unverified items are exactly the ones that
should not be coded against.

| Candidate | Licence | Pure Go | Maintained | HEIC | Apple 0x0011 | Verdict |
|---|---|---|---|---|---|---|
| `github.com/evanoberholster/imagemeta` | MIT | yes | yes | yes **[VERIFIED]** | **no** | **Adopt for the bulk path.** Verified correct on a genuine iPhone 12 Pro HEIC — GPS, `DateTimeOriginal`, `OffsetTimeOriginal` and `SubSecTimeOriginal` all matching exiftool exactly — and fast (18.4 µs/file). Two known defects, both handled below and neither disqualifying: it drops all EXIF from a HEIC whose `iloc` precedes `iinf`, and it parses only **embedded** Apple maker-note values, failing every offset-based one. Treat 0x0011 as unavailable from pure Go. |
| `github.com/bep/imagemeta` | MIT | yes | yes | yes **[VERIFIED]** | no | **Adopt as the cross-check, not the primary** — no Apple maker notes, and it returns **unsigned** GPS with a separate ref (`GPSLongitude = 122.4194`, `GPSLongitudeRef = "W"`), so the caller applies the sign. **[VERIFIED]** it reads HEIC files whose `iloc` precedes `iinf`, which is precisely what the primary drops. The two libraries' HEIC failure modes are **complementary**, which makes a 15 µs re-probe on an empty result cheap insurance rather than belt-and-braces. |
| `github.com/dsoprea/go-exif/v3` | MIT | yes | **no** (last push 2024) | **not alone** | unverified | **Reject.** Parses EXIF/TIFF blobs only. HEIC needs `go-heic-exif-extractor` (last push 2021), JPEG needs `go-jpeg-image-structure` (2022): a three-repo pipeline whose container halves are four and five years stale. |
| `github.com/rwcarlsen/goexif` | BSD-2 | yes | **no** (2022, self-described alpha) | no | no | **Reject, and it is disqualifying rather than merely dated. [VERIFIED]** `DateTime()` returns the value in `time.Local` — the *server's* zone — so one file yields three different instants under three `TZ` values. It also cannot see `OffsetTimeOriginal` at all (its tag table predates EXIF 2.31) and reports the tag absent when it is present. Both failures are silent. |
| `github.com/barasher/go-exiftool` | **GPL-3.0** | no | yes | yes | yes | **Do not import it. Drive the `exiftool` binary as a subprocess instead** — see below. |
| `exiftool` as a subprocess (`-stay_open True -@ -`) | — | conditional runtime dep | — | yes **[VERIFIED]** | **yes [VERIFIED]** | **Adopt for the Live Photo still probe only** (1.6), and offline as the fixture oracle. 2.7–3.1 ms/file resident. |
| `ffprobe -v quiet -print_format json -show_format -show_streams` | — | already a dependency | n/a | container only | n/a | **Adopt for `.mov`/`.mp4`, and for nothing else. [VERIFIED]** exposes `com.apple.quicktime.content.identifier`, `com.apple.quicktime.creationdate`, `com.apple.quicktime.location.ISO6709`, `location`/`location-eng` and `creation_time` in `format.tags`; ffmpeg passes every `mdta`/Keys atom through verbatim. Reuse `internal/thumbnail`'s timeout, process group and cancel func rather than writing a second `exec.CommandContext` call site. |
| `trimmer-io/go-xmp` | — | — | — | — | — | **Not needed.** Only the export path emits XMP, and that is a small fixed template. |

**Why `exiftool` is a subprocess and never an import.** `barasher/go-exiftool` is **GPL-3.0**,
the only non-permissive licence in this table. Importing it into the Go module would make the
built binary a derivative work under GPL-3.0. The project currently ships no `LICENSE` at
all, so that is not an immediate legal problem — it is worse than one: it would **silently
foreclose ever releasing coldstore under a permissive licence**, an irreversible decision
taken as a side effect of picking a metadata library.

It is also unnecessary, because **this codebase already has the right pattern**.
`internal/thumbnail/thumbnail.go:162` invokes `ffmpeg` — itself GPL/LGPL — through
`exec.CommandContext` as a separate process, which does not make the binary a derivative
work. Do exactly the same for exiftool: drive `exiftool -stay_open True -@ -` over
stdin/stdout using the existing harness — `Setpgid`, process-group `SIGKILL` on cancel,
`WaitDelay`, per-invocation deadline. That is on the order of a hundred lines, it reuses a
harness that is already tested, it keeps the licence posture exactly where it is, and it
preserves the measured 2.7–3.1 ms/file resident cost.

Two gotchas for that wrapper: pass `-n` (`NoPrintConversion`) or GPS comes back as
`37 deg 46' 29.64" N` instead of a number; and with `-n`, `SubSecTimeOriginal` arrives as a
**float64 for `537` but a string for `"05"`** on the same field, so the decoder must accept
both types — and must preserve the leading zero, per 1.4.

**Resolved: the GPS `0,0` regression does not reproduce on real files.** An earlier revision
of this document carried it as a blocker gating the library choice. That was wrong, and the
retraction is recorded rather than quietly deleted, because the way it was wrong is
instructive.

**[VERIFIED on a genuine iPhone 12 Pro HEIC, iOS 16.7.1, originals never re-encoded]**
`evanoberholster/imagemeta` reads it correctly and exactly:

```
imagemeta   GPS 41.84340277777778, 3.0662277777777778
exiftool -n GPS 41.8434027777778,   3.06622777777778     <- exact match
            DateTimeOriginal 2023-10-22 09:39:48.445 +0200
```

GPS, capture time, `OffsetTimeOriginal` and `SubSecTimeOriginal` are all right, on a file with
a real 44-entry Apple maker note.

**Why the synthetic result looked so convincing.** A clean bisect over eight synthetic
maker-note variants implicated the *type* of tag `0x0001 MakerNoteVersion`: `SHORT` parsed,
`int32s` (SLONG) lost GPS. Type was isolated from value, and a no-maker-note control ruled out
the rewriting tool. Then the real file's tag `0x0001` was confirmed to be type 9 — **Apple
really does write `int32s`** — so "real files will break" followed from a verified premise and
a verified mechanism. It is still false: the real 44-entry maker note does not trigger the
mis-seek, and the minimal three- and four-entry ones do. The fault is layout-sensitive in a
way the reduction did not preserve.

The lesson worth carrying: **a bisect can be internally valid, mechanistically explained, and
still not generalise from a reduced fixture to a real one.** "Verified on synthetic input" is
a weaker claim than it feels like while you are holding the bisect, and this document's
`[VERIFIED]` markers should be read with the fixture in mind, not just the verb.

**What survives, and it is enough to keep the cross-check.** `bep/imagemeta` returned correct
GPS on **all 17** synthetic variants and read 56 tags cleanly from the real HEIC, so the
re-probe specified below for the `iloc` bug **would have covered the GPS bug too, for free**.
Keep the trigger widened to "empty result **or** `0,0` from a file carrying an Apple maker
note": it costs tens of microseconds on a rare path, it is already being built, and `0,0` is
worth distrusting on its own merits — it is simultaneously a plausible bug signature and a
real coordinate in the Gulf of Guinea.

Two secondary results from the same work, neither container-specific:

- The synthetic variants behave **identically in JPEG and HEIC**, so nothing here was ever a
  container problem. One asymmetry is worth knowing: a maker note with no `0x0001` at all
  hard-errors on JPEG (`data is not long enough`) but silently returns `0,0` on HEIC. A loud
  failure is the better of the two.
- A real Apple-maker-note **JPEG carrying GPS** was not obtainable and is strictly
  **[UNVERIFIED]**. Given container-independence in the synthetic set and correctness on the
  real HEIC, the risk is low.

**The Apple maker note is a different story, and it got worse on real data.**
**[VERIFIED on the real iPhone HEIC and on a JPEG carrying a genuine `ContentIdentifier`]**
`evanoberholster/imagemeta` does not merely miss the odd tag — it fails a whole *class*:

| Value storage | Example | Result |
|---|---|---|
| **Embedded** (≤4 bytes, inline in the IFD entry) | `ImageCaptureType` | correct |
| **Offset-based** (every string, every UNDEF blob) | `ContentIdentifier`, `BurstUUID`, `ImageUniqueID`, `RunTime` | wrong or empty |

Concretely, on a file whose real identifier is `D4122CE4-…`, ExifTool reads it and imagemeta
returns `""`. On the real iPhone HEIC, `MakerNoteVersion` comes back as `2` — a hardcoded
default — where the file says `14`, and `RunTime` comes back `0/0`.

**`ContentIdentifier` is a string, so it is always offset-based, so it is always broken.**
That is not an edge case to work around; it is the entire mechanism Live Photo pairing would
have depended on (1.6), and it is why the still half goes through `exiftool`.

A signal about how load-bearing that library's maker-note support is: its own
`TestAppleMakerNoteFixture` asserts `RunTime.Value != 0`, and on genuine iPhone output
`RunTime` is `0/0` — **the upstream test would fail on real data.** It passes today only
because the fixture is not committed and the test skips.

**One incidental gotcha with real consequences for this design.** `imagemeta` attempts the
Apple maker note **only when IFD0 `Make == "Apple"`**. Handed a file with an intact maker note
but no `Make` tag, it returned nothing at all — no maker note, and no Make/Model either.
ExifTool reads it regardless. Section 1.1 is a catalogue of the ways iOS export paths strip
things, so a file that has lost IFD0 while keeping its maker note is not hypothetical here,
and it fails silently. This is one more reason the pairing probe is exiftool's job.

**ffprobe reads no EXIF from stills at all.** Verified against JPEG and HEIC files carrying
full EXIF and GPS: it returns nothing for JPEG, and for HEIC it opens the container with the
mov/mp4 demuxer and reports only `major_brand`, `minor_version` and `compatible_brands`.
This is worth stating because "we already shell out to ffmpeg for HEIF thumbnails, so let us
use ffprobe for HEIC metadata too" is the obvious shortcut and it silently yields nothing.

**The HEIC box-order bug, and the design's answer to it.** `evanoberholster/imagemeta`
**silently drops all EXIF from a HEIC when the `iloc` box precedes `iinf`** inside `meta`.
It returns no error — just an empty result. The cause is that its `iloc` reader matches
against an item id that only the `iinf` reader populates. ISO 14496-12 does not mandate box
order: Apple and Canon write `iinf` first and work; libheif- and Nokia-encoded files write
`iloc` first and break. This was reproduced by swapping only those two boxes in one file,
which took it from zero tags to full EXIF and GPS.

The consequence for coldstore is specific and bad. Section 1.1 makes "absent metadata" a
first-class, honestly-recorded state, and section 3.2 gives the owner an `undated=1` filter
built on it. This bug would file a photo that *has* a capture date under "we looked and
there was nothing", permanently, with no error anywhere. So:

> **An empty extraction result from a HEIC is suspicious, not authoritative.** When the
> primary reader returns no tags at all for a `.heic`/`.heif`, re-probe with
> `bep/imagemeta` before writing the empty result. If the fallback finds tags, use them and
> record `captured_source` normally; if both come back empty, the file really has nothing.

That is one extra parse — tens of microseconds — on the small minority of HEICs that return
nothing, and it is the difference between a correct catalog and a quietly wrong one.

**Cost budget, measured.** Per file, 200 JPEGs, warm cache, on a fast ARM machine; multiply
by three to ten for the low-power target:

| Path | Cost per file |
|---|---|
| `bep/imagemeta` | 14.9 µs |
| `evanoberholster/imagemeta` | 18.4 µs |
| `rwcarlsen/goexif` | 44.4 µs |
| `exiftool` via `-stay_open` | 2.7–3.1 ms |
| `exiftool` spawned per file | 58.7 ms (≈55 ms of it is interpreter startup) |

Pure-Go extraction is **two to three orders of magnitude cheaper than an exiftool
subprocess**, and cheaper than the earlier estimate in this document by a similar margin: a
50,000-file backfill is single-digit seconds of CPU for the stills, not minutes. The video
half dominates, because each `ffprobe` is a process spawn. The pass is still batched and
cancellable (1.9) — but for disk-seek and courtesy reasons, not because the parsing is
expensive.

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

## 2. Schema — migration 7 and migration 8

Follow `internal/catalog/migrations.go` exactly: append-only, ascending, a version number
assigned when the migration lands and never reserved in advance, `schemaVersion` bumped in
the same commit, and **migrations 2 and up written bare** so a runner bug that replays one
fails loudly.

Two migrations, not one, because they have different shapes: **7 is all additive** (`CREATE
TABLE` and `ALTER TABLE ADD COLUMN`, fast, safe, no table rewrite) and **8 rebuilds the
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

### 2.4 Migration 7, in full

```sql
-- migration7TagsAndMetadata

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

### 2.5 Migration 8 — widening the `jobs.action` CHECK

`jobs` carries `CHECK(action IN ('restore','archive','scan'))`. SQLite cannot alter a CHECK
constraint; adding a `rename` action requires the documented twelve-step table rebuild. This
is the one migration in this document that is not additive, and it is why it is separate.

```sql
-- migration8RenameJobs
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
only consumer. Migration 7's fifteen columns are appended to it, in DDL order, and to
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
	MetaLastAttempt  *int64     `json:"meta_last_attempt,omitempty"`
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
        "undo": { "changes": [
          { "ids": [ …actually changed… ], "add": [], "remove": ["Trips/Iceland"] }
        ] } }
```

The ordinary request is the compact uniform form above. The same endpoint also accepts
`{ "changes": [ { "ids": […], "add": […], "remove": […] }, … ] }`; the two forms are
mutually exclusive. `undo` uses this grouped form because a uniform inverse is not exact
when the requested files began with different tag sets. It is directly replayable as one
POST and still executes in one transaction.

**A request, not a job.** Pins are already a request at 5,000 ids; a tag write is strictly
cheaper (no mover mutex, no filesystem, no folder aggregate to recompute). Caps: **5,000
ids** (the same constant shape as `maxBulkPinIDs`, and for the same reason — the request is
cheap but there is no progress reporting to hide a long one behind) and **16 tag names**.
The id cap is applied after deduplication and existence filtering; the tag cap is applied
after normalisation/folding. Unknown file ids are ignored like the bulk-pin primitive and
never appear in `undo`; if every id is unknown, the request is a successful no-op and does
not create unused tag rows.

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

**The `undo.changes` block** is the exact inverse of what was applied — per tag and
direction, the ids that actually changed, not the ids that were requested. It is what lets
`u` in the UI be a single POST with no client-side bookkeeping and no undo stack in the
daemon, and it is what makes it safe for a tag operation to apply without a confirmation
dialog (section 5.9).

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
keyboard layer provides a real vim operator-pending grammar — counts, motions, operators —
with leader = space. This section specifies how tagging **fits into** that grammar rather
than bolting a second keymap beside it.

It has been rewritten against that layer's actual bindings and its actual command model. Two
things it originally assumed were wrong: `gt` / `gT` and `<Space>t` were already taken (5.1),
and **an operator carries a range but not an argument**, so `tt` is two steps rather than one
and a picker-per-photo cannot be the fast path (5.2). Both corrections shaped everything
below.

### 5.1 What the keyboard layer already owns

The grammar, the modes and most of the alphabet exist. This section specifies tagging as an
addition **inside** that layer, and the first job is to write down what is already spent so
nothing here quietly collides with it.

| Class | Bound |
|---|---|
| Motions | `h` `j` `k` `l` `0` `$` `gg` `G` `<C-d>` `<C-u>` `<C-f>` `<C-b>` |
| Operators | `d` archive · `r` restore · `p` pin · `u` unpin · `y` download |
| Selection | `x` toggle · `v` visual · `V` visual-line · `<C-a>` select all |
| Open / menu | `<CR>` `o` `<Menu>` |
| Lightbox | `q` `i` |
| Conflicts | `cd` `ca` |
| Navigation | `gt` / `gT` next / previous view · `]f` / `[f` |
| Overlays | `:` `?` `<F10>` `<Esc>` |
| Leader (`<Space>`) | `a` `c` `g` `b` `s` `o` `t` `m` `R` `fe` `?` |

Machine modes are `normal`, `visual` and `visual-line`. Everything else is a **scope** —
a keymap layer pushed with `useKeymapLayer(name, handler)` and resolved by `SCOPE_PRIORITY`,
optionally cutting off the layers beneath it via `EXCLUSIVE_SCOPES`. That distinction runs
through the rest of this section: Tag mode and the description editor are scopes, not modes.

**Tagging takes `t` and `T`.** They are unbound in every scope and semantically free: that
layer implements no till-motions, because `t` / `T` / `;` / `,` have no meaning when the unit
is a file rather than a character in a line. An earlier draft of this document offered
`gt` / `gT` as a fallback; **that was the actual collision** — `gt` and `gT` are
next / previous view — and `<Space>t`, this section's original Tag-mode key, is the filter
palette. The fallback is withdrawn rather than left in the document as a trap.

The three obligations that follow from working inside a real grammar:

1. **Tagging contributes operators, motions and scopes — never a one-off chord.** If tagging
   needs a bespoke `Ctrl+Alt+T`, it has failed.
2. **The operator is repeatable with `.`.** Vim users press it without thinking, and 5.3
   specifies what "repeat" has to mean here for it to do anything useful.
3. **Undo is `U`, not `u`.** `u` is the unpin operator in this app. This is a genuine cost —
   the single most reflexive key in vim is unavailable — and it is recorded here rather than
   discovered at the keyboard. `<C-r>` redoes.

### 5.2 The constraint that shapes everything: operators take a range, not an argument

This is the correction the rest of the section is built around.

An operator in this grammar resolves a **range** — from a count, a motion, or the current
selection — and hands those ids to a bulk action. There is no register concept, so **the
keystroke cannot carry *which tag***. `t` + motion resolves the range and then the
interpreter must ask.

Two consequences, and the second is the design problem:

- **`tt` is two steps, not one.** It resolves "this file", then opens a picker. Every
  arbitrary-tag path is at minimum operator + motion + picker + commit.
- **A culling pass cannot afford a picker per photo.** The premise of this whole section is
  moving quickly across hundreds of frames, and a modal question on every one of them is
  exactly the thing that makes people reach for the mouse instead.

So the design is not "one tagging path with a picker". It is **three paths, and the picker is
only the first of them**. The professional tools this feature is modelled on solved the same
problem the same way — Photo Mechanic applies a colour class on a bare keypress, not through
a dialog — and the escape from the picker therefore belongs in the keymap surface, not in a
follow-up issue.

### 5.3 The three tagging paths

| Path | Keys | When |
|---|---|---|
| **Picker** | `t{motion}` | An arbitrary or brand-new tag. Always available; the only one needed for correctness. |
| **Repeat** | `t{motion}<CR>`, or `.` on the cursor target | The same tag again, over a new range. The picker opens **pre-seeded with the last committed tag set**, selected, so `<CR>` alone reapplies it and any keystroke replaces it. One key of overhead, no new binding. |
| **Quick slots** | `<Space>1` … `<Space>9` | The handful of tags a given pass actually uses. Applies slot *N* immediately, **no picker**, to the current selection if there is one and to the file under the cursor otherwise. |

**`.` replays the resolved tag set, not the keystroke.** A naive `.` re-runs the operator and
reopens the picker, which is worse than useless — it looks like a repeat and asks a question.
`.` must replay `(operator, tag set)` against the current cursor target with no prompt. The
repeated case is the only case `.` exists for.

**Quick slots are `<Space>1`–`<Space>9`, not bare `1`–`9`.** Bare digits are **counts** in an
operator-pending grammar; binding them would break `3j` and `2t3j` everywhere they were live.
Leader-prefixed digits are unambiguous, are free in the leader namespace, and work identically
in the gallery and in Tag mode. Slot contents are configured in the tag manager (section 7)
and persist per folder; an unassigned slot is a no-op with a status-line note, never a silent
nothing.

### 5.4 The tag operator and its motions

| Keys | Effect |
|---|---|
| `t{motion}` | Tag the covered range |
| `tt` | Doubled operator = the current file, exactly as `dd` and `yy` |
| `3tj` / `t3j` / `2t3j` | Counts compose on both sides, as the grammar already provides |
| `tG` | To the end of the loaded set |
| `t}` | To the end of the current capture-day group |
| `t` in visual / visual-line | Applies to the selection |
| `T{motion}` | **Untag**, same grammar; the picker offers only tags present on the covered set |
| `.` | Replay the last tag operation on the cursor target (5.3) |
| `U` / `<C-r>` | Undo / redo the last tag write (5.9) |

Motions tagging adds — they are motions, so every operator gets them free, including the
existing `d` archive and `p` pin:

| Keys | Motion |
|---|---|
| `}` / `{` | Next / previous **capture-day boundary** |
| `]t` / `[t` | Next / previous file sharing **any tag** with the current one |
| `]u` / `[u` | Next / previous **untagged** file — the culling workhorse |
| `]p` / `[p` | Next / previous file in a different **pair group** (skips a Live Photo's `.MOV`) |

`]u` and `[u` must cross the loaded window: an untagged file beyond the last fetched page is
still the next untagged file, so the motion triggers `loadMore()` rather than stopping at the
edge.

### 5.5 The picker

A **single line at the bottom of the viewport**, styled as a command line — not a modal
dialog. A dialog steals focus, needs a close button, traps `Tab`, and is what a keyboard-first
user is trying to escape.

```
 tag 47 files ▸ trips/ice▏          trips/iceland (431)  ·  + new tag
```

It is pushed as an **exclusive scope** (`tag-prompt`), so the layers beneath it do not fire
and `<C-u>` means "clear the line" rather than "scroll half a page".

- **It states the count before you type.** `tag 47 files`, never a bare cursor. A bulk
  operation must never look like a single one.
- It opens **pre-seeded with the last committed tag set**, selected. `<CR>` alone is the
  repeat path of 5.3; typing anything replaces the seed.
- `<Tab>` / `<C-n>` / `<C-p>` cycle completions from `GET /tags?prefix=&folder=`, debounced at
  120 ms and aborted with an `AbortController` on every keystroke (the generation-counter
  pattern `useFiles` already uses — a stale completion list is worse than none).
- `,` or `;` commits the current token and keeps the prompt open, so
  `iceland,reykjavik,2024<CR>` is three tags in one prompt and one round trip.
- `/` builds hierarchy as you type; the completion list shows the subtree.
- A name with no match shows `+ new tag` **before** commit. Silently creating `icelnad` next
  to `iceland` is how a vocabulary rots, and one word of feedback is the fix.
- `<Esc>` cancels the **whole** operation — no partial tag, no created tag.
- `<C-w>` deletes the previous word, `<C-u>` clears the line. Free from a real input, and
  worth asserting, because a custom key handler eats them.

`T{motion}` uses the same component with an inverted completion source: only tags present on
the covered set, each with the count of covered files carrying it
(`removing "Untriaged" from 47 of 47`).

### 5.6 Tag mode — a scope, not a mode

`<Space>e` enters it. (`<Space>t` — the obvious key, and this document's original choice — is
the filter palette.) It adds `"tagging"` to `Scope` with a `SCOPE_PRIORITY` entry and is
**non-exclusive**, so motions fall through to the layer beneath exactly as the lightbox's do.
The machine mode stays `normal`.

Dark only, no chrome: a large preview, a metadata and tag panel on the right, a status line.

| Keys | Effect |
|---|---|
| `j` / `k` | Next / previous file — falls through to the layer beneath |
| `J` / `K` | Next / previous **untagged** file |
| `{` / `}` | Previous / next capture day |
| `i` | Enter the description editor (5.7) |
| `t` / `T` | Tag / untag — the operator, with an implicit `tt` |
| `<Space>1`–`<Space>9` | Quick slots (5.3) |
| `,` | Toggle **carry-forward** |
| `x` | Toggle selection of the current file — the layer's existing binding |
| `<Esc>` | Leave, returning to the same file in the gallery |

**`i` shadows the lightbox's `i`, deliberately. This was raised, reviewed and settled —
do not "fix" it.**

Two reasons, in order of weight:

1. **`i` enters insert mode, and the description editor *is* insert mode.** This is the most
   vim-correct binding in the whole design. Moving it would trade a correct idiom for a
   technicality.
2. **A scope-local override is exactly what scopes are for when the underlying binding is
   *meaningless* in that scope.** The lightbox's `i` toggles an info panel; Tag mode renders
   that panel permanently, so there is nothing left for it to toggle. Shadowing a binding
   whose behaviour is already satisfied costs the user nothing — no capability is lost, and
   no muscle memory is contradicted, because pressing `i` in Tag mode still produces the
   thing `i` was reaching for.

It is also a good illustration of why the **non-exclusive** scope choice for `tagging` is the
right one. An exclusive scope would have had to re-declare every motion it wanted to keep;
a non-exclusive one lets `j`, `k`, `gg` and `G` fall through untouched and overrides exactly
the one key whose meaning has changed. The override is small because the scope is porous.

The general rule this is an instance of, worth stating because the next shadow will not be
this clear-cut: **shadow only when the shadowed behaviour is already delivered by the scope
doing the shadowing.** A scope-local override that removes a capability is a collision
wearing a scope's clothes, and belongs in the spent-key table of 5.1 instead.

**Carry-forward** is the largest speed win in a real culling run — it is what Photo Mechanic's
stationery pad and Lightroom's sync-metadata are for. With `,` on, the tags applied to the
previous file become a *pending set* on the next one; `<CR>` accepts it, any other key
discards it. It is **off by default and its state is on screen at all times**, because a
sticky write the owner has forgotten about is how 400 photos end up tagged `iceland`. It
overlaps with quick slots and does not replace them: carry-forward propagates whatever you
just did, slots apply a tag you chose in advance.

**Prefetching.** Tag mode must never wait on the network to advance: prefetch the full image
for `index+1` and `index+2`, hold the previous two, and call `loadMore()` within five of the
end — the rule `Lightbox` already follows.

### 5.7 The description editor — an exclusive scope

This is the specific failure the owner named: a description field that swallows your
navigation keys.

The answer is **not** a mode variable that every binding remembers to check. It is an
**exclusive scope** (`description` in `EXCLUSIVE_SCOPES`), so the keymap layer itself cuts off
everything beneath and no individual binding can forget.

- `i` on the focused file enters it; a `<textarea>` takes focus and the `description` scope is
  pushed.
- While it is active, `j` types the letter j. That is **correct**, and is what a vim user
  expects, because they typed `i` on purpose.
- `<Esc>` commits and pops the scope. `<C-c>` **discards** uncommitted text and says so in the
  status line. Two exits with different semantics is vim; one exit that sometimes discards is
  a bug.
- **`<C-j>` commits and advances to the next file, staying in the editor.** `<C-k>` goes
  backwards. This is the professional-tool behaviour and the whole reason the surface is
  usable: caption a frame, `<C-j>`, caption the next one, never leaving the keyboard.
- `<C-t>` opens the tag picker from **inside** the editor, commits, and returns the cursor
  where it was. Tagging and captioning are one pass, not two.
- **Autosave** on `<Esc>`, on `<C-j>` / `<C-k>`, on blur, and on a 1.5 s idle debounce. A
  caption lost to a closed tab gets retyped once and then never written again.

The existing `target.tagName === "INPUT"` guard in `Gallery.tsx` stays as a second line of
defence, but it is no longer the contract — it cannot distinguish "the owner entered the
editor on purpose" from "a re-render moved focus into an input", and `EXCLUSIVE_SCOPES` can.

### 5.8 Multiselect

`useSelection` already holds **ids, never indices**, with an anchored range whose toggle
applies the anchor's state, and ids that scroll out of the loaded window stay selected. That
is exactly what bulk tagging needs and requires no change. The selection keys are the layer's
existing ones — `x`, `v`, `V`, `<C-a>` — and tagging adds none of its own.

**Selection wins over the cursor.** When the selection is non-empty, `t` applies to it and the
picker says `tag 431 files`. There is **no separate bulk-tag key**; a second key for the same
operator is exactly the one-off chord 5.1 forbids. The same rule governs `<Space>1`–`<Space>9`.

**Above the cap, say so.** `<C-a>` resolves through `GET /files/ids`, which truncates at
20,000 and reports `truncated: true`. The picker must then read
`tag 20,000 of 47,331 files (the rest will not be tagged)` rather than implying it covered
everything — the same honesty rule as the restore preflight, which names the folder, the
overage and the eviction count rather than asking "are you sure?".

### 5.9 Undo, and why tagging never confirms

An archive is a Syncthing deletion on every device, so it confirms. **A tag write moves no
bytes and is perfectly reversible, so it must not** — a confirmation on a reversible action
trains the owner to dismiss confirmations, which is what makes the irreversible ones stop
working.

What makes that safe:

- The picker shows the count **before** commit.
- The bulk endpoint returns `undo`: the exact inverse of what it actually applied (4.4).
- The client holds the **last one**; `U` posts it, `<C-r>` re-applies. One level, no stack, no
  daemon state.
- The toast reports the truth and the escape — `tagged 4,231 files · U to undo` — held for the
  20 seconds `ToastProvider` already gives errors.

One level is deliberate. A deeper stack needs invalidation rules for "something else changed
this file", and there *is* something else: the extraction pass and the quota tick. A five-deep
undo that silently no-ops because a row moved is worse than one that only ever promises a
single step.

### 5.10 What this costs the keyboard layer

Small, and worth recording so the estimate is not re-derived:

- **The operator is one row in the bindings array** —
  `{ id: "op.tag", keys: "t", command: { kind: "operator", operator: "tag" }, scopes: NAVIGABLE }`
  — which yields `tt`, `3tj`, `2t3j`, `tG`, visual-mode `t` and a generated cheatsheet row
  with no state-machine change. `T` is a second row.
- **Three typed seams take one line each**: `OperatorId`, `useBulkActions.execute()` and
  `describeBulk()`. All three are exhaustive switches, so the compiler names every site.
- **Two scope families**: `"tagging"` (non-exclusive), and `"description"` and `"tag-prompt"`
  (both in `EXCLUSIVE_SCOPES`) — each a `SCOPE_PRIORITY` entry and a `useKeymapLayer` call.
- **The motions** (`}` `{` `]t` `[t` `]u` `[u` `]p` `[p`) are ordinary motion rows, but each
  needs a resolver that can page — see 5.4.

### 5.11 The `data-*` selector contract

The browser tier drives the embedded UI through `data-*` attributes (`web/e2e/helpers.ts`).
Tagging adds:

| Attribute | On |
|---|---|
| `data-tag-scope` | The Tag mode root, `"on"` / absent |
| `data-tag-prompt` | The picker container |
| `data-tag-prompt-count` | Text is the count the operator will affect |
| `data-tag-completion="<name>"` | Each completion row |
| `data-tag-chip="<name>"` | Each tag chip on a tile or in the panel |
| `data-quick-slot="<n>"` | Each configured quick-slot indicator |
| `data-description-editor` | The textarea |
| `data-capture-date` | The rendered capture time; the value is the raw `captured_local` |
| `data-capture-source` | `"exif"` / `"quicktime"` / `"manual"` / `""` |
| `data-pair-group="<pair_key>"` | Tiles belonging to a pair group |
| `data-keymode` | The **machine mode**: `"normal"` / `"visual"` / `"visual-line"` |
| `data-keyscope` | The **top active scope**: `"gallery"` / `"lightbox"` / `"tagging"` / `"tag-prompt"` / `"description"` |

`data-keymode` and `data-keyscope` are two attributes because they are two things, and the one
that matters here is the scope: it is the only way a browser test can assert that `j` does not
navigate while a caption is being typed, which is the regression this section exists to
prevent.

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
enqueues a **`rename` job** (migration 8) with `total = 1`, no `job_items`, and the from/to
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
| `web/src/hooks/useTagOperator.ts` | Range resolution and the three paths of 5.3. **Not** a state machine: the keyboard layer already resolves the range, so this receives ids and decides picker / repeat / slot |
| `web/src/hooks/useQuickSlots.ts` | Per-folder slot assignments, persisted; an unassigned slot is a no-op with a status-line note |
| `web/src/components/TagPrompt.tsx` | The picker of 5.5, pre-seeded with the last committed tag set |
| `web/src/components/TagMode.tsx` | The culling scope of 5.6 |
| `web/src/components/DescriptionEditor.tsx` | The exclusive-scope textarea of 5.7 |
| `web/src/components/QuickSlots.tsx` | The `<Space>1`–`<Space>9` slot indicators, and the editor that assigns them (5.3) |
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
- `TagManager` (in `ConfigPanel`): assigns the nine quick slots per folder, beside the tag
  tree. A slot is the fast path of 5.3, so it needs somewhere to be chosen; leaving it to a
  config file would put the one keyboard-first affordance behind a text editor.
- The keyboard layer's bindings array: two operator rows (`t`, `T`), the eight motions of
  5.4, and three scope registrations (`tagging` non-exclusive; `tag-prompt` and
  `description` exclusive). Three typed seams — `OperatorId`,
  `useBulkActions.execute()`, `describeBulk()` — take one line each and are exhaustive
  switches, so the compiler names every site.
- `Lightbox.tsx`: the metadata panel gains capture time, source, coordinates and tags, and
  keeps `file_mtime` **separately labelled** — never merged into one "date" row.

**Tests**

- `make test-web`: prompt key handling (`,` commits a token, `<Esc>` writes nothing,
  `<Tab>` cycles), the count shown before commit, the seeded-repeat path (`<CR>` alone
  reapplies the last tag), `.` replaying without reopening the picker, the undo round trip
  under `U`, the query-building for every new filter, and — the important one — that `j`
  typed in the description editor produces the letter `j` and does not navigate.
- A test that **bare `3j` still counts** with the tag operator registered. Quick slots are
  leader-prefixed precisely so digits stay counts, and that is the assertion which proves it.
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
| 2 | `internal/metadata`: the `Extractor` interface, EXIF + ffprobe + the exiftool subprocess, the ISO 6709 parser, the precedence rules. Pure package, exiftool-generated fixtures, no schema and no daemon changes. **Unblocked** — the GPS `0,0` concern was a synthetic-fixture artifact (1.8) | #1 | — |
| 3 | Migration 7, plus `fileColumns`, `scanFile`, `FileRecord`, `types/api.ts`. Schema only | #2 | — |
| 4 | The extraction pass: `FilesNeedingMetadata`, `SetExtractedMetadata`, the failure budget, the manual-rescan reset | #3 | #1, #2 |
| 5 | Pair grouping: `ExpandPairGroups`, wired into `OffloadCandidates` and the job enqueue path, plus the `pair_split` event | #6 | #1, #2, #3 |
| 6 | Tag catalog primitives and CRUD API: normalise/fold, resolve-or-create with ancestors, subtree rename and merge, the counts query, hydration | #7 | #2 |
| 7 | Capture date as a product surface: `sort=captured`, the range filters, day grouping, honest display, manual entry | #4 | #2, #3 |
| 8 | Location: display, `near=`, manual coordinates | #5 | #2, #3 |
| 9 | Descriptions: the `PATCH` endpoint, `described=`, the insert-mode editor | #8 | #2 |
| 10 | Search and filter: the extended `q`, `tag=`, `untagged=` | #9 | #2, #7 |
| 11 | Frontend, non-keyboard first: chips, the prompt, the tag manager, the filter presets. Everything works with a mouse before anything works without one, so a broken key binding is isolated to the keyboard layer | #10 | #7, #9 |
| 12 | Multiselect tagging: the bulk endpoint, the caps, the undo contract, `SelectionBar` | #12 | #7, #9, #10 |
| 13 | Migration 8 + the rename job. (`display_name` already landed in migration 7.) | #14 | #2, #6, #13 |
| 14 | The keyboard layer: the operator, the motions, Tag mode, visual-mode bulk, undo. Last, because it depends on every other layer being correct | #11 | #4, #6, #8, #9, #10 |
| 15 | Export: XMP sidecars into `{cold_storage_path}/sidecars/` | not yet filed | #7 |

---

## 9. Edge cases, named

- **An empty extraction result from a HEIC is suspicious, not authoritative.** The primary
  reader silently drops all EXIF when `iloc` precedes `iinf` (1.8), so "no tags" and "a
  library bug" are the same observation. Re-probe with the cross-check reader before writing
  the empty result — the trigger is "empty **or** `0,0` from a maker-note file". This is the
  one place where the honest-absence design of 1.1 could be turned into a lie by a
  dependency.
- **`0,0` is a real coordinate that must never be a sentinel**, in Go, on the wire or in the
  UI — it is a legitimate point in the Gulf of Guinea. It is also cheap to distrust: the
  primary reader produced it on *synthetic* maker-note fixtures, and although that never
  reproduced on real hardware output (1.8), the cross-check triggers on it anyway because the
  re-probe is already there for the `iloc` bug and costs tens of microseconds.
- **A file with an intact maker note but no IFD0 `Make` tag reads as having no metadata at
  all.** The primary reader only attempts the Apple path when `Make == "Apple"`, and handed
  such a file it returns nothing — not even Make/Model (1.8). Section 1.1 catalogues the iOS
  export paths that strip things, so this is not hypothetical, and it fails silently. The
  empty-result re-probe is what catches it.
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
  migration 7 (UNIQUE on `name_fold`) but *can* be introduced by a future import. The merge
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
Rejected — section 5.9. A confirmation on a reversible action trains the owner to dismiss
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

**12. Basename matching as the *mechanism* for Live Photo pairing.**
Rejected — 1.6. An earlier draft promoted stem matching from a hint to the fallback
authority whenever a content identifier was unavailable. Verification showed no pure-Go
library reads Apple's `ContentIdentifier`, which would have made the stem the authority for
essentially every still — and the stem does not merely fail to pair, it **pairs the wrong
files** when an import renumbers on collision. The stem's job is now to choose which stills
are worth an `exiftool` probe; the UUID decides the grouping. Where `exiftool` is absent a
stem-derived key is written and `pair_source` says so, because a degraded group the system
knows is degraded is a different thing from one it believes.

**13. Importing `github.com/barasher/go-exiftool`.**
Rejected — 1.8. It is GPL-3.0, and importing it would make the built binary a derivative
work, silently foreclosing any future permissive licence for coldstore as a side effect of
choosing a metadata library. Driving the `exiftool` binary as a separate process carries no
such term, is what this codebase already does for `ffmpeg` at
`internal/thumbnail/thumbnail.go:162`, reuses a subprocess harness that is already tested,
and costs the same 2.7–3.1 ms/file.

**14. Bare `1`–`9` as quick-tag keys.**
Rejected — 5.3. Photo Mechanic applies a colour class on a bare number key, which is the
right ergonomics and the wrong grammar: in an operator-pending keymap bare digits are
**counts**, so binding them would break `3j` and `2t3j` wherever they were live. Leader-
prefixed digits (`<Space>1`–`<Space>9`) are unambiguous, free in the leader namespace, and
identical in the gallery and in Tag mode.

**15. `gt` / `gT` as the tag operator's fallback keys.**
Rejected — 5.1, and worth recording because the fallback was *more* dangerous than the
primary it was hedging. `gt` and `gT` are next / previous view in the keyboard layer, and
`<Space>t`, this document's original Tag-mode key, is the filter palette. Bare `t` and `T` —
the keys the fallback existed to avoid claiming — turned out to be unbound and semantically
free, because that layer implements no till-motions.

**16. Moving Tag mode's `i` to avoid shadowing the lightbox's `i`.**
Rejected — 5.6. `i` enters insert mode and the description editor is insert mode, so this is
the most vim-correct binding in the design; the lightbox's `i` toggles an info panel that Tag
mode renders permanently, leaving it nothing to toggle. Shadowing a binding whose behaviour
the shadowing scope already delivers removes no capability. Recorded here because a reader
scanning for collisions will find this one and try to be helpful.

**17. Allowing the coldstore folder label to be renamed.**
Rejected — section 6.4. It is a join key in five tables and a path component in two on-disk
trees, and it is TOML-owned, so any UI rename is undone at the next restart in the worst
possible way. `display_name` gives the owner what they actually wanted with none of it.
