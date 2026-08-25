# Private FFmpeg build for Coldstore

This runbook installs a pinned FFmpeg build for Coldstore on the Debian 12
microserver without replacing Debian's `/usr/bin/ffmpeg` or
`/usr/bin/ffprobe`. It is written for the measured production host: an HP
ProLiant MicroServer with a two-core AMD Turion II Neo N40L and 3.7 GiB of RAM.

The intended result is:

```text
coldstore.service
  -> service-only PATH
     -> /opt/coldstore-ffmpeg/8.1.2/bin/{ffmpeg,ffprobe}
        -> FFmpeg's statically linked libav* libraries
        -> Debian 12 libc, pthreads and zlib

interactive shells and other services
  -> /usr/bin/{ffmpeg,ffprobe} from Debian
```

This is a thumbnail-decoding change. It does not transcode videos for browser
playback and it does not change the original media files.

## Why this build is needed

The production evidence collected on 2026-08-24 shows two different failures:

| Check | Measured result | Meaning |
|---|---|---|
| Host | Debian 12 `amd64`, GCC 12.2, two N40L cores, 3.7 GiB RAM, 4 GiB swap | A native build is feasible, but should use one make job. |
| Installed FFmpeg | Debian FFmpeg 5.1.9 | Keep it as the known-good fallback. |
| 3024×4032 HEIC | ExifTool identifies a valid `image/heic`; FFmpeg reports `moov atom not found` | The old CLI cannot parse this HEIC container. |
| 1920×1080 MOV | `ffprobe` identifies H.264 High, `yuv420p` | The installed decoder and MOV demuxer work. |
| Coldstore HEIC command | Timed out after 35 seconds | Container support and performance both remain to be proven with the new build. |
| Coldstore MOV command | Timed out after 35 seconds | The representative-frame `thumbnail` filter is too slow on this sample with the current build. |

Coldstore gives each FFmpeg process 30 seconds. Therefore a successful compile
is not a successful deployment: the candidate must parse the HEIC and both
representative files must complete Coldstore's actual pipelines inside that
deadline.

FFmpeg 7.0 introduced HEIF/AVIF still and tiled-image support, and FFmpeg 8.1
added tiled HEIF support to the `ffmpeg` CLI. This runbook pins 8.1.2, released
2026-06-17, which is the latest point release in the 8.1 branch at the time of
writing. See the official [FFmpeg changelog](https://github.com/FFmpeg/FFmpeg/blob/n8.1.2/Changelog)
and [download page](https://ffmpeg.org/download.html).

The newer 9.x line is not required for this fix. Pinning 8.1.2 gives this host
a specific, repeatable artifact to test and retain for rollback.

## Boundaries and acceptance criteria

The build is deliberately broad in native FFmpeg components and narrow in
external dependencies. It includes the native H.264 and HEVC decoders, MOV/MP4
and image-pipe formats, PNG and MJPEG encoders, and the `thumbnail` and `scale`
filters. It does not enable network protocols, `ffplay`, or third-party codec
libraries.

In particular, `libx264-dev` is not needed. Coldstore decodes H.264 and writes
JPEG thumbnails; it does not encode H.264. Add x264 only if a separate
server-side video-transcoding feature is designed later.

Do not switch the service until all of these are true:

- the release signature is good and its primary fingerprint is
  `FCF986EA15E6E293A5644F10B4322F04D67658D8`;
- the candidate reports version 8.1.2 and the required codecs, encoders,
  filters, and formats;
- the representative HEIC can be probed and decoded;
- the exact HEIC thumbnail subprocess finishes in less than 30 seconds;
- a cheap first-frame MOV extraction succeeds, proving basic decoding; and
- the exact MOV `thumbnail,scale` pipeline finishes in less than 30 seconds.

If the long capability test succeeds but a 30-second test times out, FFmpeg is
functionally correct but the Coldstore algorithm or timeout is still unsuitable
for this CPU. Stop before service integration and address that application
constraint separately.

## 1. Set the build variables

Run this block in the shell that will perform the build. Re-run it after opening
a new SSH session. Replace the last two paths with representative files; keep
the quotes because paths may contain spaces.

```bash
ffmpeg_version='8.1.2'
ffmpeg_release_fingerprint='FCF986EA15E6E293A5644F10B4322F04D67658D8'
ffmpeg_work_root="$HOME/src/coldstore-ffmpeg"
ffmpeg_download_dir="$ffmpeg_work_root/downloads"
ffmpeg_keyring_dir="$ffmpeg_work_root/release-keyring"
ffmpeg_source_dir="$ffmpeg_work_root/source/ffmpeg-$ffmpeg_version"
ffmpeg_build_dir="$ffmpeg_work_root/build/ffmpeg-$ffmpeg_version"
ffmpeg_prefix="/opt/coldstore-ffmpeg/$ffmpeg_version"
heic_test_file='/absolute/path/to/representative.heic'
mov_test_file='/absolute/path/to/representative.mov'
```

Sanity-check the two inputs before doing any lengthy work:

```bash
test -r "$heic_test_file" && printf 'HEIC is readable\n'
test -r "$mov_test_file" && printf 'MOV is readable\n'
```

## 2. Install the build prerequisites

Keep Debian's `ffmpeg` package installed. It remains the rollback binary.

```bash
sudo apt update
sudo apt install -y --no-install-recommends build-essential pkg-config nasm zlib1g-dev ca-certificates curl xz-utils gnupg
```

On the measured host this adds six packages and does not upgrade or remove
anything. The `file` package is optional and is not a build dependency.

Verify the important tools and resources:

```bash
gcc --version | sed -n '1p'
make --version | sed -n '1p'
pkg-config --version
nasm -v
/sbin/swapon --show
free -h
df -hT "$HOME" /opt
```

The N40L lacks SSSE3, SSE4.1, SSE4.2, and AVX. Do not set `--cpu`,
`-march=native`, or hand-written `-m...` compiler flags. FFmpeg's normal x86-64
build retains runtime CPU detection and is the safe choice for this machine.

## 3. Download and authenticate the release

FFmpeg signs release archives. The following uses a dedicated keyring under the
build tree, so it does not modify the operator's normal GnuPG keyring.

```bash
mkdir -p "$ffmpeg_download_dir" "$ffmpeg_keyring_dir"
chmod 700 "$ffmpeg_keyring_dir"
curl --fail --location --output "$ffmpeg_download_dir/ffmpeg-$ffmpeg_version.tar.xz" "https://ffmpeg.org/releases/ffmpeg-$ffmpeg_version.tar.xz"
curl --fail --location --output "$ffmpeg_download_dir/ffmpeg-$ffmpeg_version.tar.xz.asc" "https://ffmpeg.org/releases/ffmpeg-$ffmpeg_version.tar.xz.asc"
curl --fail --location --output "$ffmpeg_download_dir/ffmpeg-devel.asc" https://ffmpeg.org/ffmpeg-devel.asc
```

Run the fingerprint and signature checks in a subshell. A failed check exits
only the subshell, not the SSH session.

```bash
(
    set -eu
    actual_fingerprint=$(gpg --batch --homedir "$ffmpeg_keyring_dir" --with-colons --import-options show-only --import "$ffmpeg_download_dir/ffmpeg-devel.asc" 2>/dev/null | awk -F: '$1 == "fpr" { print $10; exit }')
    printf 'Expected fingerprint: %s\n' "$ffmpeg_release_fingerprint"
    printf 'Actual fingerprint:   %s\n' "$actual_fingerprint"
    test "$actual_fingerprint" = "$ffmpeg_release_fingerprint"
    gpg --batch --homedir "$ffmpeg_keyring_dir" --import "$ffmpeg_download_dir/ffmpeg-devel.asc"
    gpg --batch --homedir "$ffmpeg_keyring_dir" --verify "$ffmpeg_download_dir/ffmpeg-$ffmpeg_version.tar.xz.asc" "$ffmpeg_download_dir/ffmpeg-$ffmpeg_version.tar.xz"
)
```

Do not continue unless the fingerprints are identical, `gpg` says `Good
signature`, and the block exits successfully. The fingerprint and verification
procedure are published on FFmpeg's [official download page](https://ffmpeg.org/download.html#releases).

## 4. Extract and configure an isolated build

The following refuses to reuse an existing source or build directory. This
prevents stale object files or flags from contaminating a rebuild. If a prior
attempt exists, rename it with `mv` and then run the block again.

```bash
(
    set -eu
    test ! -e "$ffmpeg_source_dir"
    test ! -e "$ffmpeg_build_dir"
    mkdir -p "$(dirname "$ffmpeg_source_dir")" "$(dirname "$ffmpeg_build_dir")"
    tar -C "$(dirname "$ffmpeg_source_dir")" -xf "$ffmpeg_download_dir/ffmpeg-$ffmpeg_version.tar.xz"
    mkdir "$ffmpeg_build_dir"
)
```

Configure from the separate build directory:

```bash
(
    set -eu
    set -o pipefail
    cd "$ffmpeg_build_dir"
    "$ffmpeg_source_dir/configure" \
        --prefix="$ffmpeg_prefix" \
        --extra-version=coldstore \
        --toolchain=hardened \
        --enable-static \
        --disable-shared \
        --disable-doc \
        --disable-ffplay \
        --disable-network \
        --disable-autodetect \
        --enable-pthreads \
        --enable-zlib \
        | tee configure-summary.txt
)
```

Why these flags:

- static FFmpeg libraries make the two CLI binaries independent of a private
  runtime `libav*` search path; normal Debian libraries such as libc and zlib
  remain dynamically linked;
- disabling autodetection prevents whichever optional `-dev` packages happen
  to be installed from silently changing the build;
- pthreads and zlib are the two explicitly permitted system dependencies;
- networking and `ffplay` are unnecessary for local thumbnail extraction; and
- no CPU minimum is forced, so runtime detection remains enabled.

Do not use `--disable-everything` here. Tiled HEIF handling crosses the CLI,
container, codec, filter, scaling, and image-output layers; an aggressively
minimal component list is easy to make subtly incomplete.

The configure summary should list `ffmpeg` and `ffprobe` as programs, `zlib` as
an external library, and x86 assembly enabled. If configure fails, read
`$ffmpeg_build_dir/ffbuild/config.log` before changing flags.

## 5. Build conservatively

The service can remain online during an unprivileged build. On the two-core
N40L, use one make job and a lower scheduler priority:

```bash
(
    set -eu
    cd "$ffmpeg_build_dir"
    time nice -n 10 make -j1
)
```

There is no reliable duration estimate until this exact host has completed one
build. The 4 GiB swap is useful insurance, but sustained swap activity means the
build will be dramatically slower. Do not use `-j$(nproc)` merely because it is
a common recommendation on newer machines.

## 6. Inspect the candidate before using real media

```bash
candidate_ffmpeg="$ffmpeg_build_dir/ffmpeg"
candidate_ffprobe="$ffmpeg_build_dir/ffprobe"
"$candidate_ffmpeg" -hide_banner -version
"$candidate_ffmpeg" -hide_banner -buildconf
ldd "$candidate_ffmpeg"
```

`ldd` must not show private `libavcodec`, `libavformat`, `libavfilter`,
`libavutil`, `libswscale`, or `libswresample` shared libraries. Seeing normal
Debian libraries such as libc, libm, libpthread, or libz is expected.

Check the required components:

```bash
"$candidate_ffmpeg" -hide_banner -decoders | grep -E '(^|[[:space:]])(h264|hevc)([[:space:]]|$)'
"$candidate_ffmpeg" -hide_banner -encoders | grep -E '(^|[[:space:]])(png|mjpeg)([[:space:]]|$)'
"$candidate_ffmpeg" -hide_banner -filters | grep -E '[[:space:]](scale|thumbnail)[[:space:]]'
"$candidate_ffmpeg" -hide_banner -demuxers | grep -E '[[:space:]]mov'
"$candidate_ffmpeg" -hide_banner -muxers | grep -E '[[:space:]]image2(pipe)?[[:space:]]'
```

There may not be a demuxer literally named `heif`; real-file decoding is the
authoritative HEIC test.

## 7. Test the representative HEIC and MOV

Create a private temporary output directory:

```bash
ffmpeg_test_output=$(mktemp -d /tmp/coldstore-ffmpeg-test.XXXXXX)
chmod 700 "$ffmpeg_test_output"
```

First verify that the new `ffprobe` parses both containers:

```bash
"$candidate_ffprobe" -v error -select_streams v:0 -show_entries 'stream=codec_name,profile,pix_fmt,width,height' -of default=noprint_wrappers=1 -- "$heic_test_file"
"$candidate_ffprobe" -v error -select_streams v:0 -show_entries 'stream=codec_name,codec_long_name,profile,pix_fmt,width,height' -of default=noprint_wrappers=1 -- "$mov_test_file"
```

### HEIC capability and Coldstore deadline

The first command allows three minutes. It distinguishes an unsupported file
from a correct decoder that is merely too slow. The second applies Coldstore's
actual 30-second subprocess deadline and the same decode/output arguments.

```bash
time timeout --kill-after=2s 180s "$candidate_ffmpeg" -hide_banner -v error -i "$heic_test_file" -vframes 1 -f image2pipe -vcodec png - > "$ffmpeg_test_output/heic-capability.png"
printf 'HEIC 180-second capability exit: %d\n' "$?"
time timeout --kill-after=2s 30s "$candidate_ffmpeg" -hide_banner -v error -i "$heic_test_file" -vframes 1 -f image2pipe -vcodec png - > "$ffmpeg_test_output/heic-coldstore.png"
printf 'HEIC 30-second Coldstore exit: %d\n' "$?"
stat -c '%n bytes=%s' "$ffmpeg_test_output"/heic-*.png
```

### MOV decoder and representative-frame deadline

The cheap first-frame test proves that the MOV container and H.264 decoder
work without asking FFmpeg to scan frames for a representative image. The
second command matches Coldstore's expensive `thumbnail,scale` filter and JPEG
output.

```bash
time timeout --kill-after=2s 30s "$candidate_ffmpeg" -hide_banner -v error -i "$mov_test_file" -vf "scale='min(400\\,iw):-1'" -frames:v 1 -f image2pipe -vcodec mjpeg - > "$ffmpeg_test_output/mov-first-frame.jpg"
printf 'MOV first-frame exit: %d\n' "$?"
time timeout --kill-after=2s 30s "$candidate_ffmpeg" -hide_banner -v error -i "$mov_test_file" -vf "thumbnail,scale='min(400\\,iw):-1'" -frames:v 1 -y "$ffmpeg_test_output/mov-coldstore.thumb.jpg"
printf 'MOV 30-second Coldstore exit: %d\n' "$?"
stat -c '%n bytes=%s' "$ffmpeg_test_output"/*.jpg
```

Interpret the results before installing anything:

| Result | Decision |
|---|---|
| All commands exit 0 and output files are non-empty | Candidate passes; proceed. |
| HEIC probe still reports `moov atom not found` | Stop. Confirm this is really the 8.1.2 binary and inspect the build configuration. |
| HEIC 180-second test passes but 30-second test exits 124 | Decoder support is fixed, but the N40L cannot meet Coldstore's deadline for this image. Do not switch yet. |
| MOV first-frame passes but `thumbnail,scale` exits 124 | MOV support exists; representative-frame selection is too expensive. A newer binary alone is not the fix. |
| A command exits 124 | `timeout` killed it; this is a performance failure, not a missing codec message. |
| A command exits nonzero with an FFmpeg diagnostic | Treat the diagnostic as a format, build, or decoder failure. |

The shell timeout and Coldstore both use 30 seconds, but they are different
implementations. Leave a little performance headroom; a result that only barely
finishes is likely to fail when Syncthing or a catalog scan is busy.

After reviewing the files, remove only the known test outputs and directory:

```bash
rm -f -- "$ffmpeg_test_output/heic-capability.png" "$ffmpeg_test_output/heic-coldstore.png" "$ffmpeg_test_output/mov-first-frame.jpg" "$ffmpeg_test_output/mov-coldstore.thumb.jpg"
rmdir -- "$ffmpeg_test_output"
```

## 8. Install the passing candidate under `/opt`

This is the first privileged filesystem change. It still does not affect the
running service.

```bash
sudo install -d -o root -g root -m 0755 "$ffmpeg_prefix/bin" "$ffmpeg_prefix/share/build"
sudo install -o root -g root -m 0755 "$candidate_ffmpeg" "$ffmpeg_prefix/bin/ffmpeg"
sudo install -o root -g root -m 0755 "$candidate_ffprobe" "$ffmpeg_prefix/bin/ffprobe"
sudo install -o root -g root -m 0644 "$ffmpeg_build_dir/ffbuild/config.log" "$ffmpeg_prefix/share/build/config.log"
sudo install -o root -g root -m 0644 "$ffmpeg_build_dir/configure-summary.txt" "$ffmpeg_prefix/share/build/configure-summary.txt"
sudo install -o root -g root -m 0644 "$ffmpeg_source_dir/COPYING.LGPLv2.1" "$ffmpeg_prefix/share/COPYING.LGPLv2.1"
```

Verify the installed copy directly:

```bash
"$ffmpeg_prefix/bin/ffmpeg" -hide_banner -version
"$ffmpeg_prefix/bin/ffprobe" -hide_banner -version | sed -n '1p'
stat -c '%n owner=%U:%G mode=%a size=%s' "$ffmpeg_prefix/bin/ffmpeg" "$ffmpeg_prefix/bin/ffprobe"
sha256sum "$ffmpeg_prefix/bin/ffmpeg" "$ffmpeg_prefix/bin/ffprobe"
findmnt -T "$ffmpeg_prefix/bin/ffmpeg" -o TARGET,SOURCE,FSTYPE,OPTIONS
systemctl --no-pager --full show coldstore -p NoExecPaths -p ExecPaths
```

Record the two hashes with the maintenance log. They identify the exact local
binaries; they are not a substitute for the upstream tarball's PGP signature.
The measured `/opt` path is on the executable root filesystem and the Coldstore
unit does not set `NoExecPaths`. Stop here if either a `noexec` mount option or a
systemd execution-path restriction would block the new binary.

## 9. Give only Coldstore the private FFmpeg

The normal systemd binary search path does not include `/opt`. Use a late
service drop-in and a separate, non-secret environment file. This preserves the
Debian binary for interactive shells and other software.

The existing unit already loads `/etc/coldstore/coldstore.env`. Environment
files can override `Environment=` values, so the new file is appended by a
lexically late drop-in instead of putting `PATH` directly in the unit.

Create the non-secret path file:

```bash
printf '%s\n' "PATH=$ffmpeg_prefix/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" | sudo tee /etc/coldstore/ffmpeg-path >/dev/null
sudo chown root:root /etc/coldstore/ffmpeg-path
sudo chmod 0644 /etc/coldstore/ffmpeg-path
```

Create the late drop-in:

```bash
sudo install -d -o root -g root -m 0755 /etc/systemd/system/coldstore.service.d
printf '%s\n' '[Service]' 'EnvironmentFile=/etc/coldstore/ffmpeg-path' | sudo tee /etc/systemd/system/coldstore.service.d/zz-ffmpeg-path.conf >/dev/null
sudo chown root:root /etc/systemd/system/coldstore.service.d/zz-ffmpeg-path.conf
sudo chmod 0644 /etc/systemd/system/coldstore.service.d/zz-ffmpeg-path.conf
sudo systemd-analyze verify /etc/systemd/system/coldstore.service
```

The filename starts with `zz-` so it follows the host's existing drop-ins. Do
not create symlinks in `/usr/local/bin` and do not replace `/usr/bin/ffmpeg`.

### Maintenance-window restart

The following reload and restart are the cutover. Run them only after the media
acceptance gate above passes and when no archive or restore is being initiated:

```bash
cutover_started=$(date -Is)
sudo systemctl daemon-reload
systemctl --no-pager --full show coldstore -p FragmentPath -p DropInPaths -p EnvironmentFiles
sudo systemctl restart coldstore
systemctl is-active coldstore
```

Verify the environment of the actual running process without printing any
other environment variables:

```bash
coldstore_main_pid=$(systemctl show coldstore -p MainPID --value)
tr '\0' '\n' < "/proc/$coldstore_main_pid/environ" | grep '^PATH='
```

The printed value must begin with
`/opt/coldstore-ffmpeg/8.1.2/bin`. Then inspect only the new service log entries:

```bash
journalctl -u coldstore --since "$cutover_started" --no-pager
```

If the process PATH is wrong, roll back before rescanning. Do not assume that a
successful direct `/opt/.../ffmpeg` test proves which binary the service uses.

## 10. Requeue failed thumbnails and verify in Coldstore

Use the owner UI's manual **Rescan** action for the affected folder. A manual
rescan intentionally clears recorded thumbnail and metadata failure caps before
requeueing files; a periodic catalog scan does not. Do not edit `catalog.db`
directly.

Follow the service log while the representative files are retried:

```bash
journalctl -u coldstore -f
```

Success means the HEIC and MOV receive usable thumbnails, Coldstore stays
active, and the log does not report FFmpeg timeouts. Keep
`thumbnail_workers = 1` on this host.

`coldstore --status` is an offline catalog inspection. When the daemon is
running with an uncheckpointed SQLite WAL, it can report `catalog_unreadable`
even though the service is healthy. That message is unrelated to FFmpeg. Use
`systemctl is-active`, the UI, and the journal during this live validation; run
offline status only after a clean, planned daemon stop.

## Rollback

Rollback does not require rebuilding or uninstalling anything. Disable the
drop-in, reload systemd, and restart the service:

```bash
sudo mv /etc/systemd/system/coldstore.service.d/zz-ffmpeg-path.conf /etc/systemd/system/coldstore.service.d/zz-ffmpeg-path.conf.disabled
rollback_started=$(date -Is)
sudo systemctl daemon-reload
sudo systemctl restart coldstore
systemctl is-active coldstore
journalctl -u coldstore --since "$rollback_started" --no-pager
```

The unused `/etc/coldstore/ffmpeg-path` file and versioned `/opt` tree can remain
in place for diagnosis. An interactive shell should still resolve the Debian
binary:

```bash
command -v ffmpeg
/usr/bin/ffmpeg -hide_banner -version | sed -n '1p'
```

To re-enable the candidate, move the drop-in back to its `.conf` name, run
`systemd-analyze verify`, reload systemd, and restart Coldstore.

## Updating the private build

A source build is outside APT's FFmpeg update lifecycle. Check the official
[release page](https://ffmpeg.org/download.html) and
[security page](https://ffmpeg.org/security.html) during normal maintenance.
For each update:

1. choose a specific point release and verify its signed archive;
2. use new source, build, and `/opt/coldstore-ffmpeg/VERSION` directories;
3. repeat the component and representative-media tests before installation;
4. update `/etc/coldstore/ffmpeg-path` to the new explicit version;
5. restart during a maintenance window and verify the running process PATH;
6. retain at least the previous passing prefix for immediate rollback; and
7. record the version, date, configure flags, and installed binary hashes.

Do not point the service at an unversioned `current` symlink. An explicit path
makes every cutover visible in system configuration and prevents an accidental
symlink change from altering the next restart.

## Troubleshooting quick reference

| Symptom | Likely cause and next check |
|---|---|
| `file: command not found` in a collector | Optional diagnostic only. ExifTool and FFmpeg results remain valid; install `file` only if desired. |
| `moov atom not found` for the HEIC after cutover | Usually the Debian 5.1 binary is still being selected. Check the candidate version and the running service's PATH. |
| HEIC works with 180 seconds but exits 124 at 30 seconds | Correct decoder, insufficient application-time budget on the N40L. Do not call this a successful deployment. |
| MOV first frame works but `thumbnail,scale` exits 124 | Representative-frame scanning is the bottleneck. Consider a cheaper seek/first-frame policy or a separately configurable timeout in Coldstore. |
| `png` encoder is absent | Confirm `zlib1g-dev` was installed and inspect `ffbuild/config.log`; configure must show zlib enabled. |
| Build fails with an assembler error | Confirm NASM 2.x is installed and remove any custom CPU/compiler flags. |
| Candidate dies with `Illegal instruction` | It was built with an inappropriate CPU minimum. Rebuild without `--cpu`, `-march=native`, or custom `-m...` flags. |
| Service logs `Permission denied` for `/opt/.../ffmpeg` | Check the mount's `noexec` option and the unit's `NoExecPaths`/`ExecPaths` properties. |
| SSH still finds `/usr/bin/ffmpeg` | Expected. The override is intentionally service-only. |
| Service restart fails | Inspect `systemctl status coldstore` and the journal, then perform the drop-in rollback. Debian's FFmpeg remains intact. |
| Offline status reports an uncheckpointed WAL | The daemon is active. This is not an FFmpeg failure and must not be repaired by copying or editing the live SQLite files. |

## Related project evidence

- [`scripts/collect-ffmpeg-host-info.sh`](../scripts/collect-ffmpeg-host-info.sh)
  collects the read-only OS, toolchain, service, and media baseline.
- [`scripts/collect-ffmpeg-media-info.sh`](../scripts/collect-ffmpeg-media-info.sh)
  repeats the compact representative-file checks. Prepend a candidate directory
  to `PATH` to test it without installing it.
- [`internal/thumbnail/thumbnail.go`](../internal/thumbnail/thumbnail.go) contains
  the HEIC and video command arguments.
- [`internal/thumbnail/subprocess.go`](../internal/thumbnail/subprocess.go) defines
  the 30-second external-tool deadline.
- The upstream [FFmpeg 8.1.2 configure script](https://github.com/FFmpeg/FFmpeg/blob/n8.1.2/configure)
  documents the build switches, including runtime CPU detection and the warning
  that a forced CPU minimum may crash older processors.
- The upstream [installation instructions](https://github.com/FFmpeg/FFmpeg/blob/n8.1.2/INSTALL.md)
  require GNU Make 3.81 or newer; Debian 12's Make 4.3 satisfies that requirement.
