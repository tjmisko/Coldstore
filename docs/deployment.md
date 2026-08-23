# Coldstore Deployment Runbook

**Deploying for the first time?** Read
[Security posture](#security-posture-explicit-authentication-boundary), then work straight
through [First deployment on a fresh Debian box](#first-deployment-on-a-fresh-debian-box).
Everything after that is reference material.

## Prerequisites

- **Go 1.25.7+.** `go.mod` requires it. **Debian's package is not enough on any
  current stable release**: bookworm ships 1.19, bookworm-backports tops out
  near 1.23, and trixie ships 1.24. Install the upstream tarball to
  `/usr/local/go` and put `/usr/local/go/bin` on `PATH`. A Go 1.21--1.24 with
  the default `GOTOOLCHAIN=auto` will silently download 1.25.7 on the first
  build, which needs outbound access to `proxy.golang.org` and fails outright
  under `GOTOOLCHAIN=local` or on an offline box.
- **Node 20.19+ or 22.12+** and npm, **on whichever machine builds the
  frontend**. Vite 7 refuses to run on anything older, so Debian bookworm's
  Node 18 cannot build `web/` at all -- see
  [Building on two machines](#building-on-two-machines) for the usual way out.
  Run `npm ci` in `web/` once per clone; `web/node_modules` is gitignored and
  `make build` cannot compile the frontend without it.
- **A C compiler** (`build-essential`) **on whichever machine builds the Go
  binary.** The catalog links `go-sqlite3`, which is CGo. This is also why
  there is no clean cross-compile: see [Building on two
  machines](#building-on-two-machines).
- **ffmpeg** -- required for HEIF/HEIC decoding and video thumbnail extraction.
  Nothing else uses it; see [What actually gets a
  thumbnail](#what-actually-gets-a-thumbnail) before assuming your library needs it.
- **poppler-utils** -- required for PDF thumbnails. The generator shells out to
  `pdftocairo`. Without it, PDFs get no thumbnail: the attempt is recorded as a
  failure per file, which the UI shows as a warning glyph rather than failing
  quietly. Everything else -- EPUB covers, plaintext preview, the in-browser
  readers -- works without it. It is ~716 kB installed.
- **Syncthing** -- running and configured with the folders you want to manage
- **A separate filesystem for cold storage.** btrfs and ZFS are both good
  choices and both are covered below; the requirement is checksums and, ideally,
  redundancy. See [Cold storage
  filesystem](#cold-storage-filesystem-btrfs-or-zfs) -- including
  [why the archive is the tier that needs
  redundancy](#why-the-archive-is-the-tier-that-needs-redundancy), which is the
  opposite of the usual hot/cold intuition.

## Building

```bash
cd web && npm ci && cd ..   # once per clone; make build does this for you too

# Build frontend + Go binary in one step
make build

# Then install the built binary to /usr/local/bin
sudo make install-binary
```

`make build` does three things in sequence:

1. Runs `npm ci` in `web/` if `web/node_modules` is missing (`make deps`).
2. Builds the React app (`cd web && npm run build`) and copies the output into `internal/api/dist/`.
3. Compiles the Go binary from `cmd/coldstore/`.

`make install` is `make build` followed by `make install-binary`. Prefer the two
separate commands under `sudo`: `sudo make install` builds as root and leaves
root-owned `web/dist/`, `internal/api/dist/` and `./coldstore` behind, which
breaks your next unprivileged `make build`.

The resulting `coldstore` binary embeds the frontend -- no separate static file serving is needed.

### Building on two machines

`make build` assumes one machine can do both halves. Frequently it cannot, and
the deployment target is the machine most likely to fail:

- The frontend needs Node 20.19+. Debian bookworm ships Node 18, so `npm run
  build` cannot run there without NodeSource or nvm.
- The Go binary cannot be cross-compiled cleanly, because `go-sqlite3` is CGo.
  Building `linux/amd64` from an aarch64 workstation needs a full x86-64 cross
  toolchain, which is more setup than the split below.
- npm and Vite are the memory-hungry half, and a small home server is exactly
  where that hurts.

So: **build the frontend on your workstation, the Go binary on the target.** The
frontend output is committed into the binary by `//go:embed all:dist`, so all
that has to cross the wire is `internal/api/dist/`.

```bash
# workstation
cd web && npm ci && npm run build && cd ..
rm -rf internal/api/dist && cp -r web/dist internal/api/dist
touch internal/api/dist/.gitkeep
rsync -a --delete internal/api/dist/ YOURUSER@TARGET:~/coldstore/internal/api/dist/

# target
cd ~/coldstore
go build -o coldstore ./cmd/coldstore     # NOT `make build` -- that re-runs npm
sudo install -m 755 coldstore /usr/local/bin/coldstore
```

`--delete` on the rsync is deliberate: without it, stale hashed asset files from
an earlier build stay in `dist/` and get embedded in the binary forever. The
`touch .gitkeep` before the rsync keeps the tracked stub in place on both sides
(see [build-and-test.md](build-and-test.md)).

Because the Go half is built with a plain `go build` on the target, nothing in
the checkout is ever built as root -- which makes the "build as yourself,
install as root" warning below moot on that machine.

**Do not set `GOAMD64`.** Go's default is `v1`, which is what pre-SSE4.1 CPUs
(AMD K10, early Atom) support. A binary built with `GOAMD64=v2` or `v3` dies
with `SIGILL` on those machines, at a point that looks nothing like a
configuration error. Check before assuming your box is modern:

```bash
grep -o 'sse4_2\|avx2' /proc/cpuinfo | sort -u   # empty means v1 only
```

### Development

```bash
make dev            # go run with local coldstore.toml
make dev-web        # React dev server with hot reload (port 5173)
make test           # go test ./...
```

---

## Security posture: explicit authentication boundary

Read this before you change `listen_addr`.

**The web UI is a control plane, not a viewer.** Whoever can reach it can pin,
archive, restore, run bulk jobs across thousands of files, and rewrite each
folder's budget and enabled flag. Archiving is not a local operation: coldstore
frees space by deleting the file from the Syncthing folder and letting Syncthing
propagate that deletion to every device you own. An unauthenticated
`POST /api/v1/jobs {"action":"archive"}` is a remote delete button for your phone's
camera roll.

The server supports three deliberate shapes:

| Shape | Reachability and authentication |
|---|---|
| Local/SSH tunnel | Loopback, no application authentication. |
| LAN compatibility | One explicit LAN address plus HTTP Basic Auth, checked in constant time. Never port-forward it. |
| Selected private owner path | Loopback plus Tailscale Serve HTTPS and an exact `Tailscale-User-Login` allowlist. Never Funnel. |

An omitted `auth_mode` preserves the legacy behavior: complete Basic credentials select
Basic Auth; otherwise only a loopback listener is allowed. An explicit Tailscale mode adds a
stronger refusal: it is never accepted on a network-visible listener.

### Selected v1 remote path — Tailscale Serve identity

The owner selected Tailscale Serve for private HTTPS access from their iPhone, macOS, Linux
and Windows devices. Serve is configured and the owner has verified the HTTPS path with the
temporary production Basic Auth gate. The application target is:

```text
owner browser -> private Tailscale HTTPS -> Tailscale Serve -> 127.0.0.1:8090
                                                       -> exact login allowlist
```

Keep `listen_addr` on loopback. Do not enable Tailscale Funnel, do not expose the raw port,
and do not place another proxy path around Serve. Serve removes caller-supplied identity
headers and inserts `Tailscale-User-Login`; Coldstore accepts only exact configured logins.
It rejects missing/unlisted identities, applies same-origin checks to browser mutations and
records each accepted mutation's login in the journal. This trust is safe only because the
backend is loopback-only. A local process on the server remains inside that trust boundary.

This is full owner access, not a remote viewer. After Tailscale identity succeeds, the
existing web UI/API retains archive, restore, rename, tagging, configuration and every other
current operation. Each co-owner has a separately revocable Tailscale login; no shared
Coldstore password is distributed. Basic Auth remains a supported alternative deployment
mode, not a fallback that bypasses the Tailscale allowlist.

The second co-owner is not yet onboarded and will use a separate Tailscale account. Prefer a
revocable share of this server machine over inviting that account into the whole tailnet;
restrict the tailnet policy to TCP 443, which serves only the reviewed Coldstore proxy. Do
not grant SSH, Syncthing, the raw backend port or any other server service. `onboarding.md`
covers creating, testing and revoking the share without recording account identifiers in
tracked documentation.

### Candidate future public guest ingress — deferred

For ordinary-browser guests, the preferred candidate is a DNS-only public hostname at
Caddy, forwarding only TCP 80/443 to Caddy while coldstore remains on
`127.0.0.1:8090`. Caddy obtains and renews public certificates automatically when DNS and
inbound reachability are correct; its reverse proxy streams responses without full buffering,
so the existing Go `ServeContent`/`ServeFile` Range behavior remains useful.

This is a decision gate, not a deployment instruction. The owner must first confirm a
reachable public address/CGNAT status, inbound-port policy, domain and DNS/DDNS ownership,
guest scope and whether exposing the home IP is acceptable. Publish an AAAA record only
with intentional IPv6 routing/firewalling. If a DNS provider also offers proxy/CDN service,
keep this hostname DNS-only so private full-resolution media is not carried by that
intermediary.

The public hostname must be **read-only**. Today's Basic Auth grants every authenticated
client archive, restore, budget and bulk-job authority, so it must never be the public guest
gate. Add app-enforced, individually revocable and expiring guest grants across every API,
thumbnail, preview and download route; keep owner/admin access private over SSH or Tailscale.
Caddy method denial is useful defence in depth but is not authorization.

Trust proxy headers only when the direct peer is loopback. Use explicit canonical-origin and
secure-cookie configuration, plus CSRF defense, rate limiting, logout/revocation and private
media cache controls. Verify multi-gigabyte `HEAD`, byte Range, interrupted resume and
concurrent downloads through the proxy before rollout.

Emergency disable has three independent controls: remove the router/firewall 80/443 forward,
stop the dedicated Caddy site, and disable/revoke public guest access in coldstore. DNS
deletion is slower because records are cached. Preserve the private owner path throughout.

Cloudflare Tunnel is an origin-private alternative, not an authorization layer. Cloudflare
Access can authenticate browser guests, but the application must still enforce read-only
scope. Cloudflare's current self-serve media policy warns that video or disproportionate
large-file delivery outside its media products may be restricted, so it is not the preferred
path for this archive. References: [Caddy automatic HTTPS](https://caddyserver.com/docs/automatic-https),
[Caddy reverse proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy),
[Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/),
and [Cloudflare media policy](https://developers.cloudflare.com/fundamentals/reference/policies-compliances/delivering-videos-with-cloudflare/).

### The startup refusal

**Coldstore refuses a non-loopback listener without complete Basic credentials, and refuses
Tailscale identity mode on every non-loopback listener.** These are hard errors before the
catalog opens or a socket binds. There is no override flag.

```
$ coldstore --config /etc/coldstore/coldstore.toml
coldstore: refusing to start: [server] listen_addr = "0.0.0.0:8090" is reachable
from the network but no credentials are set.
...
```

Loopback means `127.0.0.0/8`, `::1` or the literal `localhost`. Everything else is
treated as network-reachable, including `0.0.0.0`, a bare `:8090`, and any
hostname -- a hostname is not resolved, because startup must not depend on DNS and
a name that resolves to a LAN address is exactly the case this check exists for.

Previously this was a single `WARN` line in the journal. That is not a decision
anyone consciously makes; it is one they scroll past.

### Why the shipped example binds loopback

`coldstore.toml.example` ships `listen_addr = "127.0.0.1:8090"` rather than a
placeholder LAN address such as `192.168.1.10:8090`. The placeholder was
considered and rejected: it fails to bind on almost every real network with
`cannot assign requested address`, and the fastest fix a hurried owner reaches for
is `0.0.0.0` -- the exact outcome the example exists to prevent. Loopback works
verbatim on any machine, and it is genuinely usable over an SSH tunnel with zero
network exposure.

To use the Basic Auth LAN compatibility shape, do both of these:

1. Set `listen_addr` to **this machine's actual LAN address**, e.g.
   `192.168.1.10:8090`. Not `0.0.0.0:8090`: a wildcard bind also listens on any
   VPN, container bridge or tethered interface the box later grows.
2. Set credentials (below).

### Setting the password

Put it in the environment, not in `coldstore.toml`:

```bash
sudo install -d -m 750 -o root -g YOURUSER /etc/coldstore
sudo install -m 600 -o root -g root /dev/null /etc/coldstore/coldstore.env

# Generate the password once, into your password manager AND into the file, so
# it is never echoed to the terminal and never lands in shell history.
PW=$(head -c 24 /dev/urandom | base64) \
  && printf 'COLDSTORE_USERNAME=coldstore\nCOLDSTORE_PASSWORD=%s\n' "$PW" \
     | sudo tee /etc/coldstore/coldstore.env >/dev/null \
  && printf '%s' "$PW" | wl-copy    # or: | xclip -sel clip, or | pbcopy
unset PW
```

Paste it into your password manager now. **Do not `cat` the file** to read the
password back: that puts it in terminal scrollback, and on a box you reached
over SSH, in whatever is recording that session. If you lose it, generate a new
one and restart the daemon -- there is nothing else stored against it.

The env file is `root:root 0600` on purpose, and that is *not* the same rule as
the TOML above. systemd reads `EnvironmentFile=` itself, as PID 1, before it
drops to `User=`; the service account never opens this file and must not be able
to.

The shipped unit already has `EnvironmentFile=-/etc/coldstore/coldstore.env`. The
`[server] username` / `password` keys still work if you prefer them; the
environment wins when both are present.

### What is NOT protected

- **The password crosses the LAN in base64.** Anyone who can sniff your wireless
  segment can read it. Accepted; do not reuse a password you use elsewhere.
- **There is no rate limiting and no lockout.** The constant-time comparison stops
  a byte-by-byte timing oracle, not an offline-speed guessing attack. Use a long
  random password, not a memorable one.
- **Basic Auth mode has no CSRF protection.** A browser can attach cached Basic credentials
  to a cross-site form post. Tailscale identity mode separately rejects cross-site browser
  mutations using `Origin` and Fetch Metadata, while permitting headerless non-browser API
  clients through Serve.
- **`IPAddressAllow=` in the unit is defence in depth, not the control.** The
  control is the bind address plus the credential.

---

## First deployment on a fresh Debian box

Run through this in order. It ends with an authenticated, LAN-bound daemon and a
verification that it is not reachable from anywhere else.

### 1. Prerequisites on the box

```bash
sudo apt update
sudo apt install -y ffmpeg poppler-utils sqlite3 btrfs-progs   # or zfsutils-linux, if that is your pool

# Only if you are building the Go binary on this box (the usual case):
sudo apt install -y build-essential
```

`build-essential` is not optional: the catalog uses `go-sqlite3`, which is CGo.

**Do not install `golang-go`, `nodejs` or `npm` from Debian.** Both are too old
on every current stable release -- Go 1.19 (bookworm) or 1.24 (trixie) against a
`go.mod` that asks for 1.25.7, and Node 18 (bookworm) against a Vite that needs
20.19+. Install Go from the upstream tarball:

```bash
curl -fsSLO https://go.dev/dl/go1.25.7.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.7.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin      # and add it to ~/.profile
go version
```

and build the frontend somewhere else -- [Building on two
machines](#building-on-two-machines). Installing Node on the server is a valid
alternative, but it is the only reason that box would need Node at all.

Syncthing should already be installed, running, and syncing the folders you intend
to manage. Note **which user account it runs as** -- you need it in step 4.

```bash
systemctl status syncthing@YOURUSER.service   # or: systemctl status syncthing
ps -o user= -C syncthing | sort -u
```

**Every managed folder must be Send & Receive in Syncthing.** coldstore frees space
by deleting files and letting Syncthing propagate the deletion. On a Receive Only
folder nothing propagates, the budget does nothing, and clicking "Revert Local
Changes" in Syncthing starts a full-library archive/restore flap. Check each
folder's type in the Syncthing UI before continuing.

### 2. Cold storage volume

The archive is the tier that needs redundancy. Read [Cold storage
filesystem](#cold-storage-filesystem-btrfs-or-zfs) before choosing where it goes;
the short version is that once a file is offloaded, the copy under
`cold_storage_path` is the only copy that still exists anywhere.

**btrfs** -- a subvolume plus an `/etc/fstab` entry:

```bash
# One-time, if the filesystem does not exist yet. RAID1 across two whole devices:
sudo mkfs.btrfs -L vault -d raid1 -m raid1 /dev/sdX /dev/sdY

sudo mount /dev/sdX /mnt
sudo btrfs subvolume create /mnt/coldstore
sudo umount /mnt

sudo mkdir -p /srv/coldstore
sudo blkid -s UUID -o value /dev/sdX        # both devices report the SAME uuid
```

```fstab
UUID=<uuid>  /srv/coldstore  btrfs  defaults,nofail,noatime,compress=zstd:3,subvol=coldstore  0 0
```

```bash
sudo mount -a && findmnt -no SOURCE,FSTYPE,OPTIONS /srv/coldstore
```

`nofail` is close to mandatory on a headless box. Without it a mount failure
takes down `local-fs.target`, and with it `sshd`, leaving only the physical
console. The last column is `0` because btrfs has no boot-time fsck -- integrity
comes from `scrub`, not from `fsck`.

**ZFS** -- a dataset with a native mountpoint:

```bash
sudo zfs create -o mountpoint=/srv/coldstore -o compression=lz4 tank/coldstore
```

**Neither** -- a plain directory on an existing filesystem. Supported, and what
the tests and the demo use, but you are then trusting one non-redundant
filesystem with the only copy of every archived file:

```bash
sudo mkdir -p /srv/coldstore
```

#### coldstore will not create `cold_storage_path` -- and that is not a mount guard

coldstore refuses to start when `cold_storage_path` is missing, rather than
creating it. The stated reason is that creating it on a boot where the pool had
not mounted would silently write the catalog and the whole archive to the root
filesystem.

**That protection only works when the path can actually be absent, and with an
fstab mount it cannot be.** `mount` requires the mountpoint directory to exist,
so `/srv/coldstore` is a real, empty, writable directory on the root filesystem
whenever the volume is *not* mounted. Every check coldstore makes then passes:
it exists, it is a directory, it is writable. The daemon starts, creates a fresh
`catalog.db` on root, seeds folder policy from `coldstore.toml` -- discarding the
budgets and pins the real catalog held -- and begins archiving into
`/srv/coldstore/archive/` on the root filesystem. Syncthing propagates each
deletion to every device you own. When the volume is finally mounted, those
archive copies are shadowed and invisible, and the files are gone everywhere else.

ZFS with a native `mountpoint=` does not have this problem, because `zfs mount`
creates the directory itself, so an unmounted pool really does leave the path
absent. **Anything mounted from `/etc/fstab` -- btrfs, ext4, XFS, a legacy ZFS
dataset -- does.**

There is no `.stfolder`-style marker on cold storage. `internal/daemon/folder.go`
has exactly that guard for the other side of the move, and cold storage has no
equivalent, so on an fstab mount the real protections are:

| Guard | Covers |
|---|---|
| `[storage] require_mountpoint = true` | **Startup and every offload.** Exit 1 at startup, and a refusal on each archive if the volume goes away later. Off by default because a plain directory is a supported layout -- **turn it on**, see [`[storage]`](#storage). |
| `RequiresMountsFor=` in the unit (step 6) | **Boot ordering.** systemd will not start coldstore unless the mount unit succeeded, so the daemon never even runs in the bad state. |

Set both. They fail at different moments and neither subsumes the other:
`RequiresMountsFor=` does nothing for a daemon started by hand or for a volume
unmounted an hour after boot, and `require_mountpoint` cannot stop systemd from
launching a daemon that will then exit 1 once a minute forever.

Do not unmount `cold_storage_path` while the daemon is running. Stop the service
first:

```bash
sudo systemctl stop coldstore && sudo umount /srv/coldstore
```

A cheap belt-and-braces check, run after any storage work:

```bash
findmnt -no SOURCE,FSTYPE,TARGET /srv/coldstore || echo 'NOT MOUNTED -- do not start coldstore'
```

### 3. Ownership and permissions

```bash
sudo chown YOURUSER:YOURUSER /srv/coldstore
sudo chmod 700 /srv/coldstore
```

`YOURUSER` is the account from step 1 -- the one Syncthing runs as. Coldstore must
run as that same account:

- It deletes files out of the Syncthing folder and writes them back on restore, so
  it needs the same write access Syncthing has.
- A restored file is created by coldstore. Owned by a different user, Syncthing
  could no longer modify or delete it, and the folder would start throwing
  permission errors on the next remote change.

A dedicated `coldstore` service account is only worth the trouble if you also add
it to Syncthing's group and set `g+w` on every managed folder.

### 4. Install the binary and the config

```bash
cd /path/to/coldstore
(cd web && npm ci)                     # web/node_modules is gitignored; without this the build fails
make build                             # build as YOURSELF, not as root -- see the note below
sudo make install-binary               # installs ./coldstore to /usr/local/bin

sudo install -d  -m 750 -o root -g YOURUSER /etc/coldstore
sudo install -m 640 -o root -g YOURUSER coldstore.toml.example /etc/coldstore/coldstore.toml
sudoedit /etc/coldstore/coldstore.toml
```

**If this box cannot build the frontend** -- Node too old, or too little RAM for
Vite -- use the two-machine split instead: [Building on two
machines](#building-on-two-machines). The rest of this step is unchanged.

**Build as yourself, install as root.** `sudo make install` would work, but it
runs the whole build as root and leaves root-owned `web/dist/`,
`internal/api/dist/` and `./coldstore` in your checkout, so your next non-sudo
`make build` fails on permissions. `make build && sudo make install-binary` is
the same result without that.

**The mode pair is `750` on the directory and `640` on the file, both group
`YOURUSER`.** The daemon reads the config as the service user, so it needs
`x` on the directory to traverse it and `r` on the file. A root:root `750`
directory or a `600` file makes the daemon exit 1 at startup with
`reading config: ... permission denied` before it does anything else.

**The fix for that error is not `chmod 644`.** `[server] password` is a
supported place to put the web-UI password, and this file also names every
directory coldstore is permitted to delete from. Keep it unreadable to everyone
except root and the service user.

Edit at minimum:

- `cold_storage_path` -- the volume from step 2.
- `require_mountpoint = true`, unless cold storage really is a plain directory on
  an existing filesystem. See [`[storage]`](#storage).
- `listen_addr` -- this box's LAN address, from `ip -4 -o addr show scope global`.
- one `[[folders]]` block per Syncthing folder, with an absolute `path`, a unique
  `label`, and a seed `budget`.

Then set the credentials as described in [Setting the password](#setting-the-password).

### 5. Preflight

```bash
sudo systemd-run --pipe --wait --collect \
  --uid=YOURUSER \
  --property=EnvironmentFile=-/etc/coldstore/coldstore.env \
  /usr/local/bin/coldstore --config /etc/coldstore/coldstore.toml --check
```

**Not `sudo -u YOURUSER COLDSTORE_PASSWORD=... coldstore --check`.** That puts
the password in `argv`, where every user on the box can read it out of `ps`, and
in sudo's own log line -- on Debian, `authpriv`, so the journal and
`/var/log/auth.log`. It also lands in your shell history. The whole reason the
password lives in the environment is that it stays out of places that get copied
around; a preflight command that undoes that is worse than no preflight.

`systemd-run` reads the credentials from the same `EnvironmentFile=` the unit
will, so this checks the real configuration rather than a hand-typed
approximation of it, and the secret never appears on a command line.

If your systemd rejects `--property=EnvironmentFile=` -- verified working on
systemd 259, not verified on every older release -- skip the preflight rather
than passing the password on a command line: install the unit,
`systemctl start coldstore`, and
read the same validation output from `journalctl -u coldstore -b`. Everything
`--check` prints, the daemon prints too -- and it prints it from *inside* the
sandbox, which is strictly more informative.

`--check` loads the config, validates it, inspects the machine, prints a summary and
exits. It never opens the catalog and never binds a port. Sample output:

```
config           /etc/coldstore/coldstore.toml
cold storage     /srv/coldstore (exists, writable; mounted, btrfs)
listen           192.168.1.10:8090 -- reachable from LAN
auth             basic auth as coldstore
folders          2 declared
  CameraRoll           /home/tjmisko/CameraRoll (seed budget 20GB)
  Books                /home/tjmisko/Books (seed budget 20GB)

systemd          ReadWritePaths=/srv/coldstore /home/tjmisko/CameraRoll /home/tjmisko/Books
                 --check runs OUTSIDE the unit's sandbox and cannot verify
                 this line. Copy it into /etc/systemd/system/coldstore.service;
                 a path missing from it makes the daemon exit 1 at startup
                 with EROFS, not silently misbehave later.

preflight OK, no warnings
```

The `cold storage` line reports whether that path is its own mounted filesystem.
`; NOT a separate mount -- confirm this is deliberate` is correct and expected
for a plain directory, and is the one warning you get that an fstab volume failed
to mount -- see [step
2](#coldstore-will-not-create-cold_storage_path----and-that-is-not-a-mount-guard).
It is a description, not a verdict: `preflight OK` is still printed either way.

**The one thing `--check` cannot check is itself.** Run by hand it is not inside
the unit's `ProtectSystem=strict` sandbox, so every path looks writable and it
will print `preflight OK` for a unit whose `ReadWritePaths=` is missing a folder.
That is why it prints the line you need: paste it into the unit in step 6. The
daemon runs the identical writability probe *inside* the sandbox on every real
start, so the mistake is caught -- as an immediate exit 1, at startup, with a
message naming `ReadWritePaths=`.

The full checklist, including what each failure means, is in
[Preflight checklist](#preflight-checklist) below.

### 6. Install the unit

```bash
sudo cp coldstore.service /etc/systemd/system/
sudoedit /etc/systemd/system/coldstore.service
```

Three things must be edited before it will work, plus one more if you are on ZFS:

| Directive | Set it to |
|---|---|
| `User=` / `Group=` | The account Syncthing runs as (step 1). |
| `RequiresMountsFor=` | Your `cold_storage_path`, plus any folder path on a separate volume. |
| `ReadWritePaths=` | `cold_storage_path` **and every `[[folders]].path`** -- the exact line `--check` printed in step 5. |
| `After=`/`Wants=zfs-mount.service` | Uncomment **only** on a ZFS dataset with a native `mountpoint=`; see below. |

**`RequiresMountsFor=` is not a nicety on an fstab mount.** It is the only thing
standing between an unmounted volume and the daemon archiving into the root
filesystem -- see [coldstore will not create `cold_storage_path` -- and that is
not a mount
guard](#coldstore-will-not-create-cold_storage_path----and-that-is-not-a-mount-guard).
Get the path exactly right; a typo makes the directive silently resolve to the
nearest existing mount, usually `-.mount`.

Verify it resolved to what you meant, after `daemon-reload`:

```bash
systemctl show coldstore -p Requires -p After | tr ' ' '\n' | grep '\.mount'
# expect srv-coldstore.mount, NOT -.mount
```

`ReadWritePaths` is the one that bites. `ProtectSystem=strict` makes the whole
filesystem read-only apart from what is listed there, so a folder you forget is
read-only **to this process** -- and the daemon **exits 1 at startup** when its
writability probe hits `EROFS`, before the catalog is opened. It is a loud,
immediate failure, not a surprise at the first move hours later, and the message
names `ReadWritePaths=` rather than sending you to `chown`. Every entry, and
every option deliberately left out, is justified in comments inside the unit
file.

**On ZFS, `RequiresMountsFor=` is not enough by itself.** It works by finding the
`.mount` unit that provides the path, and a dataset with a native `mountpoint=`
has no such unit -- it is mounted by `zfs-mount.service`, which is a service.
Check which case you are in:

```bash
findmnt -no SOURCE,FSTYPE /srv/coldstore
# /dev/sdb1  ext4        -> not ZFS at all; RequiresMountsFor= alone is fine
# tank/coldstore  zfs    -> ZFS; run the next command to tell which kind

zfs get -H -o value mountpoint tank/coldstore
# /srv/coldstore  -> NATIVE mountpoint. RequiresMountsFor= is not enough:
#                    uncomment the pair below.
# legacy          -> mounted from /etc/fstab, so there IS a .mount unit and
#                    RequiresMountsFor= works.
```

For a native-mountpoint dataset, uncomment the pair already present in the unit:

```ini
After=zfs-mount.service
Wants=zfs-mount.service
```

Without it, systemd is free to start coldstore before the pool is imported. The
unit is configured to keep retrying once a minute rather than give up, so that is
recoverable -- but it means the daemon spends the first minutes of every boot
failing for no reason.

```bash
sudo systemd-analyze verify /etc/systemd/system/coldstore.service
sudo systemctl daemon-reload
sudo systemctl enable --now coldstore.service
```

### 7. First-run verification

```bash
systemctl status coldstore
journalctl -u coldstore -b --no-pager | head -40
```

Expect, in order: `seeded folder policy from coldstore.toml` once per folder,
`API server starting ... auth_mode=basic` (or `auth_mode=tailscale`),
`running initial catalog scan`, one
`folder scan complete` per folder, then `thumbnail pass complete`.

`auth_mode=none` means the daemon is on loopback with no application authentication. On any
other address it could not have started at all.

Then from another machine on the LAN:

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://192.168.1.10:8090/api/v1/status
# 401

curl -s -u coldstore:YOURPASSWORD http://192.168.1.10:8090/api/v1/status | head -c 200
# {"total_files":...}
```

Open `http://192.168.1.10:8090/` in a browser; the native basic-auth dialog should
appear before the UI does.

### 8. Confirm it is not reachable from outside the LAN

Four checks, cheapest first.

**a. It bound one address, not all of them.**

```bash
sudo ss -ltnp | grep 8090
# LISTEN 0 4096 192.168.1.10:8090 0.0.0.0:*  users:(("coldstore",...))
```

The left column must be your LAN address (or `127.0.0.1`). If it reads `0.0.0.0:*`
or `*:8090`, the daemon is on every interface -- fix `listen_addr` and restart.

**b. It is not reachable on loopback-only-or-LAN-only assumptions you did not make.**

```bash
# from the box itself, if you bound the LAN address:
curl -m 3 http://127.0.0.1:8090/api/v1/status; echo "curl exit $?"
# curl exit 7 (connection refused) -- correct, it is not on loopback
```

**c. Your router is not forwarding the port.** Check the port-forwarding /
"virtual server" / UPnP page of your router's admin UI and confirm there is no rule
for 8090. If UPnP is enabled on the router, disable it or confirm nothing has
punched a hole:

```bash
sudo apt install -y miniupnpc
upnpc -l | grep -i 8090      # should print nothing
```

**d. It is actually closed from the internet.** From a network that is not your
LAN -- a phone on mobile data is easiest -- against your public address:

```bash
curl -m 10 http://$(curl -s https://api.ipify.org):8090/api/v1/status; echo "curl exit $?"
```

`curl exit 28` (timeout) or `7` (refused) is what you want. A `401` means the port
is open to the internet: stop the daemon and fix the router before doing anything
else. Run this from outside; running it from inside the LAN can succeed via
hairpin NAT and tell you nothing.

Optionally, refuse non-LAN traffic at the host as well:

```bash
sudo apt install -y ufw
sudo ufw allow from 192.168.1.0/24 to any port 8090 proto tcp
sudo ufw deny 8090/tcp
```

The shipped unit also sets `IPAddressDeny=any` with the RFC1918 ranges allowed,
which enforces the same rule in the kernel's cgroup BPF filter.

---

## Offline archive health report

Use `--status` when the daemon or HTTP listener is unavailable, and before pasting a
support handoff:

```bash
coldstore --config /etc/coldstore/coldstore.toml --status
coldstore --config /etc/coldstore/coldstore.toml --status --json
```

`--status` is an inspection mode, not daemon startup. It does not load credential
environment variables, retain Tailscale login identities, enforce the configured server
authentication mode, perform
writability probes, reconcile TOML, resume jobs, scan folders, migrate SQLite, create a
catalog, change journal mode, or write the schema marker. `--json` is valid only with
`--status`; `--status`, `--check`, and `--scan` are mutually exclusive.

The report includes the binary build identifier; supported and on-disk schema versions;
cold-storage mount, filesystem and free space; best-effort btrfs membership; read-only
folder readiness and budget/pin/wedge state; file counts including transient states;
active and failed jobs; thumbnail failures and content-detection backlog; redacted recent
warning events; and the last recorded successful scan and offload. Human output is the
default. JSON uses the same fields and severities.

Exit status separates the result:

| Exit | Meaning |
|---|---|
| `0` | healthy; no findings |
| `1` | warnings, such as a missing `.stfolder`, stale scan, failed job or derived-data failure |
| `2` | load-bearing failure, such as missing storage/catalog, schema mismatch, btrfs missing devices, low cold-storage space, a wedged folder or a recent unconfirmed/vanished archive copy |

The command omits server credentials, Syncthing API credentials and Syncthing folder IDs.
It also omits event paths/details and job error strings; those often contain filenames or
tool diagnostics unsuitable for a pasted report.

SQLite WAL mode has one important safety boundary. A plain `mode=ro` SQLite connection
can still create `catalog.db-wal`/`catalog.db-shm`. Status instead uses immutable,
query-only access and refuses a WAL containing uncheckpointed frames, because ignoring it
would produce a stale report and checkpointing it would mutate production. Stop the daemon
cleanly and rerun status. The command never deletes or checkpoints the WAL for you.

Checks the binary cannot make honestly:

- storage/folder writability, because proving it requires the create/delete probes that
  inspection mode forbids; use `--check` and the daemon's in-sandbox startup preflight;
- daemon liveness/running version, because status deliberately does not require the HTTP
  listener; the report identifies its own build and says daemon status was not probed;
- Syncthing Send & Receive mode, peer health and propagation, because there is no
  Syncthing API client;
- authoritative btrfs device health when `btrfs filesystem show --raw` is absent or denied.
  An `unknown` result is a warning; run the btrfs command with the privileges appropriate
  to the host. Raw device output is never copied into the report.

`scan_completed`, `scan_incomplete`, and `scan_failed` events begin with the release that
adds this command. Until a successful post-upgrade scan runs, “no successful scan recorded”
is expected and is reported as a warning rather than guessed from per-file timestamps.

---

## Preflight checklist

`coldstore --config <path> --check` performs all of the automated checks. Run it as
the service user, with the same environment the unit will have.

What it **cannot** check, and you must: that `cold_storage_path` is a mounted
volume rather than an empty mountpoint directory, and that the unit's
`ReadWritePaths=` is complete. Both are in the manual list below.

| Check | Failure mode | Fatal? |
|---|---|---|
| Config parses as TOML | `parsing config: ...` | yes |
| `listen_addr` is `host:port` | `not a valid host:port` | yes |
| Non-loopback bind has credentials | `refusing to start: ...` | yes |
| Both credentials set, or neither | `Basic Auth is incomplete` | yes |
| Tailscale auth is loopback-only | `could forge Tailscale identity headers` | yes |
| Tailscale auth has an exact login allowlist | `requires at least one exact Tailscale login` | yes |
| At least one `[[folders]]` block | `no [[folders]] are declared` | yes |
| `cold_storage_path` is absolute | `must be absolute` | yes |
| `cold_storage_path` exists and is a directory | `does not exist. coldstore will not create it` | yes |
| `cold_storage_path` is writable by this user | `is not writable by this user (uid N)` | yes |
| Folder labels are unique | `declared more than once` | yes |
| Non-empty Syncthing folder IDs are unique | `one Syncthing folder cannot back two Coldstore identities` | yes |
| Folder paths are absolute | `must be absolute` | yes |
| No two folders share a path | `two policy rows over one directory would fight` | yes |
| A catalogued path or Syncthing ID keeps its original label during normal reconciliation | `already catalogued under label ...; folder labels are stable catalog identifiers` | yes, including `--check` |
| Cold storage and folders do not overlap | `coldstore would archive its own archive` | yes |
| Each `budget` parses and is positive | `no recognized unit` / `is not positive` | yes |
| Folder path exists | `does not exist yet` | **no -- warning** |
| Folder path has `.stfolder` | `has no .stfolder marker` | **no -- warning** |
| A mounted folder is writable | `mounted (.stfolder present) but not writable` | yes |

The last two writability checks distinguish their errno, because the two causes
have opposite fixes. `permission denied` means wrong `User=` or wrong ownership
-- `chown` it. `read-only file system` means `ProtectSystem=strict` and a path
missing from `ReadWritePaths=` -- edit the unit; `chown` cannot help.

**`--check` run by hand is outside the unit's sandbox**, so it can never produce
the second of those, and it will report `preflight OK` for a unit whose
`ReadWritePaths=` is incomplete. What it does instead is print the exact
`ReadWritePaths=` line your config needs. The daemon repeats the same probe
inside the sandbox on every real start, so the mistake is caught there -- exit 1
at startup, not a silent failure later.

### Why two folder checks are only warnings

A missing folder path, or one without Syncthing's `.stfolder` marker, is **not** a
configuration error. On a cold boot the daemon can easily win the race against the
mount unit or against Syncthing's first scan. Refusing to start would turn a
transient ordering problem into a service that stays down until someone logs in.
Those cases warn, the daemon starts, and the existing `.stfolder` guards keep it
from touching the folder until it really is there.

Once `.stfolder` *is* present the folder is demonstrably real and mounted, so a
permission problem there is a deployment mistake rather than a race -- and that one
is fatal, with a message naming the fix.

Manual checks `--check` cannot make:

- [ ] Every managed folder is **Send & Receive** in the Syncthing UI.
- [ ] Each folder's `.stignore` contains `*.coldstore-tmp` **and**
      `*.coldstore-superseded*`.
- [ ] `/etc/coldstore` is mode 750, group = the service user; and
      `/etc/coldstore/coldstore.toml` is mode 640, group = the service user.
      (`sudo -u YOURUSER cat /etc/coldstore/coldstore.toml >/dev/null` is the
      check that matters -- the daemon reads this file as that account.)
- [ ] `/etc/coldstore/coldstore.env`, if used, is mode 600 and owned by
      `root:root`. Unlike the TOML this one is read by systemd (PID 1, as root)
      and handed to the daemon as environment, so the service user needs no
      access to it at all.
- [ ] `ReadWritePaths=` in the unit matches the line `--check` printed, exactly.
      This is the one thing `--check` genuinely cannot verify, because it does
      not run inside the sandbox.
- [ ] `cold_storage_path` is **actually mounted right now**:
      `findmnt -no SOURCE,FSTYPE,TARGET /srv/coldstore`. `--check` cannot tell a
      mounted volume from an empty mountpoint directory, and on an fstab mount
      the two look identical to it. See [step
      2](#coldstore-will-not-create-cold_storage_path----and-that-is-not-a-mount-guard).
- [ ] `RequiresMountsFor=` in the unit names that path, and resolved to the right
      mount unit: `systemctl show coldstore -p Requires | tr ' ' '\n' | grep mount`.
      On an fstab mount this is the only guard against archiving to the root
      filesystem.
- [ ] On ZFS with a native `mountpoint=`, the unit's `After=zfs-mount.service` /
      `Wants=zfs-mount.service` pair is uncommented. `findmnt -no SOURCE,FSTYPE
      /srv/coldstore` tells you which case you are in.
- [ ] `User=` in the unit is the account Syncthing runs as.
- [ ] `catalog.db` is backed up (see [Rolling back](#rolling-back), below).
- [ ] The port is not forwarded on your router.

---

## Simple schema-preserving redeploys

Once the host is configured using this runbook, routine application and UI
changes can be deployed from the production checkout with one command:

```bash
make redeploy-simple
```

Run it as the checkout owner, not through `sudo`. The command pulls
`origin/main` with `--ff-only`, builds the embedded frontend and daemon while
the existing service stays online, and checks that the built binary came from
the exact commit being deployed. It then runs the production preflight inside
the service account, verifies the archive filesystem, and waits for jobs and
file moves to settle before stopping the service.

The short downtime begins only after every read-only gate passes. The script
uses SQLite's online backup API to save a verified catalog backup and copies the
currently installed binary to a unique timestamped directory under
`/var/backups/coldstore`. Backups are never pruned automatically. It installs
the new binary, repeats preflight, starts the service, watches it for ten
seconds, and prints the new journal entries. If installation or startup fails,
it prints the failing stage and service diagnostics, restores the previous
binary, and attempts to restart the old service.

The shortcut deliberately refuses to continue when the checkout is dirty or
not on `main`, the archive is not mounted on the expected filesystem, active
work does not settle, or the source and production catalog schema versions
differ. Use the complete deployment and rollback procedure for database
migrations, configuration or unit changes, storage moves, and other changes
that need operator judgement. The script never restores a database
automatically.

Defaults match the installation documented here. Override them with command
options shown by `./scripts/redeploy-simple --help`, or environment variables;
for example:

```bash
COLDSTORE_BACKUP_DIR=/srv/backup/coldstore make redeploy-simple
```

The backup directory must be outside `cold_storage_path`. If this installation
intentionally uses a filesystem other than Btrfs, set
`COLDSTORE_EXPECTED_FSTYPE` to that filesystem's `findmnt` name. Do not set it
to `any` unless another reliable mount guard is already in place.

---

## Rolling back

**Back up `/srv/coldstore/catalog.db` before every upgrade.**

The catalog carries a `PRAGMA user_version` and each release migrates it forward.
A migration-aware older binary refuses a catalog whose `user_version` is newer
than it supports, before the daemon starts concurrent work. The silent downgrade
hazard applies only to binaries from before migration/version checks existed:
those binaries can open a newer catalog while ignoring fields such as `pinned`,
then make unsafe quota decisions.

```bash
sudo systemctl stop coldstore
sudo -u YOURUSER sqlite3 /srv/coldstore/catalog.db \
  ".backup /srv/backup/catalog.db.$(date +%F)"
# ... upgrade ...
sudo systemctl start coldstore
```

Write the backup **outside `cold_storage_path`**. A copy that lives inside the
tree it protects is lost to the same lost volume, and it is also inside the
directory the daemon walks and prunes. Another subvolume or dataset on the same
array is enough for the upgrade case; a different machine is better.

Use `.backup`, not `cp`: WAL mode means a file copy can be torn. If you must go
back to an older binary, restore the matching catalog backup **alongside** it. Do
not point an old binary at a new catalog.

---

## Configuration

Coldstore reads a TOML file (default: `coldstore.toml` in the working directory, overridden with `--config`).

Copy the example to get started:

```bash
sudo install -d  -m 750 -o root -g YOURUSER /etc/coldstore
sudo install -m 640 -o root -g YOURUSER coldstore.toml.example /etc/coldstore/coldstore.toml
```

`YOURUSER` is the account in the unit's `User=`. Root owns both, the service
user gets group read (and directory traverse), nobody else gets anything.

Validate it without starting the daemon at any point with
`coldstore --config /etc/coldstore/coldstore.toml --check`.

### Full field reference

#### `[storage]`

| Field | Type | Default | Description |
|---|---|---|---|
| `cold_storage_path` | string | `"/srv/coldstore"` | Root directory for the archive, catalog database, and thumbnails. |
| `require_mountpoint` | bool | `false` | Refuse to start, and refuse every offload, unless `cold_storage_path` is its own mount point. **Set this to `true` on any deployment whose cold storage is a separate volume.** |

**`require_mountpoint = true` belongs in every real deployment.** It is the only
guard against the failure in [step
2](#coldstore-will-not-create-cold_storage_path----and-that-is-not-a-mount-guard)
that `RequiresMountsFor=` does not cover: a volume unmounted *after* the daemon
started, or a daemon started by hand outside systemd. Both leave an empty
writable stub on the root filesystem that passes every other check, and the
5%-free floor cannot help because `statfs` on the stub answers about root.

It defaults to `false` only because a plain directory on an existing filesystem
is a supported layout -- it is what the tests and `make demo` use -- so it cannot
be the default without breaking them.

Where it applies:

| Point | Behaviour when the path is not a mount |
|---|---|
| `--check` and startup | Exit 1, naming `findmnt` and the mount unit. |
| Every offload -- quota tick, manual archive, bulk job | Refused with `ErrColdStorageUnmounted`; the row stays `synced` and the source file is untouched. A `cold_storage_unmounted` event is written, deduped to one an hour. The API answers `503`. |
| Restore | Not checked, and does not need to be: a restore reads *from* cold storage, so an unmounted volume fails the open and nothing is lost. |

#### `[daemon]`

| Field | Type | Default | Description |
|---|---|---|---|
| `scan_interval_minutes` | int | `15` | How often quota enforcement runs. Each tick checks every folder against its budget and offloads the oldest files that exceed it. |
| `catalog_sync_interval_minutes` | int | `30` | How often a full filesystem scan runs to reconcile the catalog with what is actually on disk. Also triggers thumbnail generation. |
| `thumbnail_workers` | int | `2` | Number of concurrent goroutines for thumbnail generation. Increase on machines with fast storage and many CPU cores; keep low if ffmpeg contention is a concern. |
| `grace_period_minutes` | int | `30` | Files whose most recent activity (the later of mtime and atime) is within this window are never offloaded, even if the folder is over budget. Protects files that are actively being synced or viewed. |

#### `[server]`

| Field | Type | Default | Description |
|---|---|---|---|
| `listen_addr` | string | `"127.0.0.1:8090"` | Address and port for the web UI and REST API. **Loopback by default.** Any other value requires Basic credentials; Tailscale identity mode always requires loopback. |
| `auth_mode` | string | `""` | `"tailscale"` requires Serve identity and the exact allowlist; `"basic"` explicitly selects Basic Auth. Empty preserves legacy inference from credentials. |
| `tailscale_allowed_users` | string array | `[]` | Exact `Tailscale-User-Login` values admitted in Tailscale mode. At least one is required; private production identities do not belong in tracked files. |
| `username` | string | `""` | HTTP Basic Auth username. Overridden by `COLDSTORE_USERNAME`. |
| `password` | string | `""` | HTTP Basic Auth password. Overridden by `COLDSTORE_PASSWORD`. Prefer the environment variable so the password is not in this file. |

Both credential fields must be set or neither. Setting one and not the other is a startup
error, and the HTTP handler also fails closed if an invalid configuration somehow reaches
it. Tailscale mode rejects either Basic credential rather than retaining a bypass path.

| Environment variable | Effect |
|---|---|
| `COLDSTORE_USERNAME` | Overrides `[server] username` when non-empty. |
| `COLDSTORE_PASSWORD` | Overrides `[server] password` when non-empty. |

The environment is the preferred home for the password. `coldstore.toml` is the
file you are most likely to attach to a bug report, paste into a chat, or copy to
another machine; `/etc/coldstore/coldstore.env`, loaded by the unit's
`EnvironmentFile=`, is not. systemd does not echo an `EnvironmentFile`'s contents
to the journal or to `systemctl show`.

#### `[syncthing]` -- reserved, currently unused

> **Neither key does anything today.** `api_url` and `api_key` are parsed into
> `config.SyncthingConfig` and then never read: there is no Syncthing API client
> anywhere in the codebase (`rg -n 'Syncthing\.API' internal/` returns nothing outside
> `config.go`). Coldstore integrates with Syncthing purely through the filesystem -- it
> moves files in and out of the folder path and lets Syncthing's own watcher propagate
> the change. Setting these keys will not make coldstore talk to Syncthing, and leaving
> them unset breaks nothing.
>
> They are kept in the schema because a future version may query Syncthing for folder
> IDs, pause/resume state, or scan triggers. Treat them as reserved.

| Field | Type | Default | Status | Description |
|---|---|---|---|---|
| `api_url` | string | `""` | Reserved -- not read | Intended as the Syncthing REST API base URL (e.g., `http://127.0.0.1:8384`). |
| `api_key` | string | `""` | Reserved -- not read | Intended as the Syncthing API key (found in Syncthing's settings). |

#### `[[folders]]` (array of tables)

Each `[[folders]]` block defines a Syncthing folder to manage.

| Field | Type | Default | Description |
|---|---|---|---|
| `label` | string | required | Permanent folder identifier. Used by catalog rows and as the top-level directory name inside the archive and thumbnails trees. Must be unique; never change it after the folder has been catalogued. |
| `syncthing_id` | string | required | The Syncthing folder ID (e.g., `xxxxx-xxxxx`). |
| `path` | string | required | Absolute path to the Syncthing folder on disk. |
| `budget` | string | required | **Seed value only.** Maximum size of synced files before offloading begins, written to the catalog the first time this folder is seen and ignored on every later start. See "Who owns which setting" below. |
| `persistent` | string[] | `[]` | **Seed value only.** Glob patterns for files that must never be offloaded, copied into the catalog on first sighting. |

**Every managed folder must be "Send & Receive" in Syncthing.** coldstore frees
space on your devices by deleting files from this folder and letting Syncthing
propagate the deletion. On a Receive Only folder nothing propagates, coldstore's
budget does nothing, and clicking "Revert Local Changes" in Syncthing will start a
full-library archive/restore flap. coldstore cannot check this for you -- there is
no Syncthing API client -- so verify it in the Syncthing UI.

**Each folder's `.stignore` should contain two lines:**

```
*.coldstore-tmp
*.coldstore-superseded*
```

Every move writes a temp sibling at the destination and commits it with a rename;
without the first line Syncthing will briefly try to propagate those partial
files. The first line also covers the writability probe: `CheckEnvironment`
creates and immediately deletes a `.coldstore-writecheck-*.coldstore-tmp` file in
each folder root at every startup and on every `--check`, and Syncthing's watcher
sees the create before the unlink.

The second line matters on the restore path: when a restore's source vanishes
mid-copy, coldstore keeps the bytes rather than destroying them, under a
`.coldstore-superseded` name **inside the Syncthing folder** -- and without the
ignore line Syncthing propagates that name to every device. The trailing `*` is
required: repeated collisions produce `.coldstore-superseded.2`, `.3`, and so on.
The scanner applies the same rule, so those files are never catalogued either.

A duplicate `label`, a duplicate `path`, a blank `label`, a non-positive `budget`,
or a `path` that is not absolute all fail config loading outright: the label is the
key the whole policy table hangs off, two rows over one directory would fight each
other, a zero budget would offload the whole folder on the first tick, and a
relative path would resolve against whatever directory systemd happened to start
the daemon in. So does a `path` that overlaps `cold_storage_path` in either
direction, which would have coldstore archiving its own archive.

A folder `path` that **does not exist**, or exists without `.stfolder`, is a
warning rather than an error -- see [why](#why-two-folder-checks-are-only-warnings).

**`path` must contain Syncthing's `.stfolder` marker.** Before moving anything into or
out of a folder, coldstore checks for it, and refuses the move if it is missing. This is
the guard against an unmounted mountpoint: an unmounted drive leaves an empty directory
that `stat` happily accepts, and without the check a restore would write hundreds of
files onto the root filesystem and delete the cold-storage copies as it went. Syncthing
creates `.stfolder` itself; if it is absent, the drive is not mounted or the path is not
a Syncthing folder. Enforcement logs a `folder_unavailable` event (deduped to one an
hour) and skips the folder; the restore and archive endpoints answer `409`.

### Who owns which setting

`coldstore.toml` **declares** folders; the catalog database **owns** their policy.

| Setting | Owner | Edited where | Takes effect |
|---|---|---|---|
| `label` | `coldstore.toml` plus the offline migration command | initial setup; explicit maintenance only afterward | on successful offline migration |
| `path`, `syncthing_id` | `coldstore.toml` | over SSH | on restart |
| budget | database | web UI | next enforcement tick, no restart |
| grace period | database | web UI | next enforcement tick, no restart |
| enabled/disabled | database | web UI | next enforcement tick, no restart |
| persistent globs | database | web UI | next enforcement tick, no restart |
| pins | database | web UI | immediately |

The label is an identity, not a display string: file rows, folder policy and archive
paths all use it as their key. Startup and `--check` reject a TOML declaration whose
cleaned path (including equivalent trailing-slash forms) or non-empty Syncthing ID is
already catalogued under a different label, even when the old folder was removed from
TOML. Keep the label when a folder merely moves to a new path, and use `display_name`
for a cosmetic change.

An intentional identity change is a separate, offline maintenance operation. Stop the
service, back up the catalog/config/binary, prepare a replacement config that contains
the new label but not the old one, and run:

```bash
coldstore --config /path/to/replacement.toml \
  --rename-folder-label OLD=NEW --rename-folder-budget 2TB
```

The command refuses a listening daemon, a changed or empty Syncthing ID, an existing
destination identity, unfinished jobs, in-flight file moves, ambiguous archive/thumbnail
directories, and thumbnail paths outside the old label root. On success it preserves file
IDs, tags, pins, metadata, policy and history; rewrites all label-bearing catalog rows;
moves `archive/OLD` and `thumbnails/OLD`; clears the stale display name and enforcement
latches; and applies the explicitly supplied temporary budget. If the catalog transaction
fails, any data-root moves are rolled back. Run the replacement config's `--check` before
starting the daemon.

**Pins (D2/D3).** A pinned file is never an offload candidate, whatever its age or the
budget pressure, until the owner unpins it. Restoring a file pins it automatically, with
`pin_source = "restore"`, which is what stops the next quota tick taking it straight back
-- an explicit user pin is never downgraded to that automatic one. Pinning moves nothing
and is legal in every status, including archived, where it takes effect on restore.

**Pins count against the budget.** The budget is a hard cap on what sits on the device,
because its whole purpose is controlling space on the owner's phone, so pinning does not
buy extra room. `GET /api/v1/status` reports `pinned_bytes` alongside `synced_size_bytes`
(a subset of it, never an addend), plus `over_budget` and `wedged`. When pinned bytes
*alone* exceed the budget the folder is **wedged**: the daemon sheds every unpinned
eligible file, cannot converge, and logs one `folder_wedged` event -- latched in the
database so a restart does not re-flood the log, refreshed once a day while it lasts, and
cleared with a `folder_unwedged` event when it resolves. Pinning past the budget is
allowed and warns.

At startup the daemon reconciles the folders in `coldstore.toml` into the catalog:

- **A folder it has never seen** gets a new row, seeded with the TOML `budget`, the
  `[daemon] grace_period_minutes`, and the TOML `persistent` globs.
- **A folder it already knows** has only its `syncthing_id` and `path` refreshed from
  TOML. Its budget, grace period, enabled flag and persistent globs are left exactly as
  the UI set them. **Editing `budget` in `coldstore.toml` after the first run does
  nothing.**
- **A folder that has disappeared from TOML** is not deleted. It is flagged
  `present_in_toml: false`, drops out of scanning and enforcement, and keeps its budget,
  its pins and its catalogued files, which stay browsable and downloadable. Putting the
  block back restores it with the owner's tuning intact.

Reconciliation failing is fatal: a daemon that cannot establish its policy table does
not start.

### Zero means default, not zero

`config.setDefaults()` fills in a value for every `int` in `[daemon]` that comes out of
TOML parsing as `0`. Go cannot distinguish "the user wrote `0`" from "the key was
absent", so both take the default:

| Setting | Written as `0` | Actually used |
|---|---|---|
| `scan_interval_minutes` | `0` | `15` |
| `catalog_sync_interval_minutes` | `0` | `30` |
| `thumbnail_workers` | `0` | `2` |
| `grace_period_minutes` | `0` | `30` |

The one most likely to bite: **`grace_period_minutes = 0` does not disable the grace
period.** It silently becomes 30 minutes, so files touched in the last half hour stay
put and a folder that looks over budget will appear not to offload. If you want the
shortest possible grace period, write `grace_period_minutes = 1`. There is currently no
way to express "no grace period at all" through the config file.

The same rule applies to the string fields, though the consequences are milder:
`cold_storage_path = ""` becomes `/srv/coldstore` and `listen_addr = ""` becomes
`127.0.0.1:8090`. That last default is loopback on purpose -- silence in the config
file must never be the thing that puts the control plane on the network.

`[[folders]].budget` is the exception -- it is **not** defaulted. A missing or empty
`budget` fails config loading outright (`folder "X": empty budget string`), as does an
unparseable one (`no recognized unit (B, KB, MB, GB, TB)`); either way the daemon exits
1 before binding a port. That is deliberate: a silently-defaulted budget would decide
how much of the user's data gets moved.

### Budget string format

Budgets are human-readable size strings parsed case-insensitively. Supported units:

| Unit | Multiplier |
|---|---|
| `B` | 1 |
| `KB` | 1,000 |
| `MB` | 1,000,000 |
| `GB` | 1,000,000,000 |
| `TB` | 1,000,000,000,000 |

The numeric part can be a float: `1.5TB`, `500MB`, `20GB` are all valid. Units use SI (base-10) multipliers, not binary.

### Persistent glob semantics

A persistent pattern marks files coldstore must never archive, whatever the budget says.
Three rules are tried in order for each pattern (`daemon.IsPersistent`):

1. **A pattern ending in `/` or `/*` protects the entire subtree beneath it.**
   `Albums/Starred/` and `Albums/Starred/*` both protect
   `Albums/Starred/2019/IMG_1.jpg`.
2. Otherwise the pattern is matched against the **full relative path** with Go's
   `filepath.Match`, whose `*` does not cross a `/`. `Albums/*.raw` matches
   `Albums/a.raw` and not `Albums/2019/a.raw`.
3. Otherwise, if the pattern contains no `/`, it is matched against the **base name**, so
   `*.raw` protects `Albums/2019/a.raw` wherever it sits.

`?` matches a single non-separator character and `[abc]` matches a character in the set,
as in `filepath.Match`. A malformed pattern matches nothing; it never matches everything.

Examples:

```toml
persistent = [
  "Albums/Starred/",       # the whole subtree, at any depth
  "important.txt",         # that filename, anywhere in the folder
  "Docs/*.pdf",            # PDFs directly inside Docs/, not in its subdirectories
]
```

Rule 1 exists because the version before it did not. Under plain `filepath.Match`,
`Albums/Starred/*` protected exactly the loose files directly inside that directory and
nothing nested -- so an owner whose starred albums were filed by year watched the folder
they had explicitly marked never-archive empty off their phone one subfolder at a time,
while two loose files at its root stayed put.

Patterns can also be added and removed at runtime in **Settings** in the web UI; they live
in the `persistent_paths` table in the catalog. The editor shows how many of the folder's
on-device files each pattern currently protects, because a pattern that protects nothing
looks exactly like one that protects everything -- a freshly typed pattern reading
"protects 0 files" is the only warning you get that your favourites are unprotected.

## systemd setup

A hardened unit is provided at `coldstore.service`. Installation is step 6 of the
[first deployment runbook](#6-install-the-unit); this section is the reference for
what each part of it does and why.

The unit expects the binary at `/usr/local/bin/coldstore` (produced by
`make install`), the config at `/etc/coldstore/coldstore.toml`, and
`WorkingDirectory=/srv/coldstore`.

### Ordering and dependencies

| Directive | Why |
|---|---|
| `After=network-online.target syncthing.service` | Ordering only. coldstore never talks to Syncthing over a socket, so it runs fine with Syncthing down; ordering just avoids a pointless "folder not ready" warning on a cold boot. |
| `RequiresMountsFor=/srv/coldstore` | The real dependency is the storage. This pulls in the `.mount` unit that provides the path -- an fstab entry, a `mountpoint=legacy` ZFS dataset, an automount. It does **not** cover a native-mountpoint ZFS dataset, which has no `.mount` unit; see the commented `After=zfs-mount.service` pair. Add a line per folder path on a separate volume. |
| `StartLimitIntervalSec=0` | **No start limit, on purpose.** See below. |
| `Restart=on-failure`, `RestartSec=60s` | Retry forever, once a minute, so a slow pool import is recovered from rather than given up on -- but see the caveat below. |

#### What `Restart=` does and does not retry

`Restart=on-failure` applies to the daemon **exiting non-zero after it started**.
It does not apply to a start job that failed on a dependency.

`RequiresMountsFor=` adds `Requires=` on the mount unit, so if that mount fails
-- a dead disk, a degraded btrfs RAID1 that will not mount without `-o
degraded`, a bad `fstab` line -- coldstore's start job is cancelled with
`dependency failed` and **`Restart=` never fires**. The unit sits inactive until
someone intervenes.

That is the right outcome: a failed mount is not a transient ordering problem,
and the alternative is the root-filesystem archive described in step 2. But it
does mean "retries forever" is true only for the ZFS-native case (no `.mount`
unit, so the daemon starts, finds nothing at `cold_storage_path`, and exits 1).
On an fstab mount, a boot with a broken volume leaves you with:

```bash
systemctl status coldstore                     # inactive, "Dependency failed"
systemctl status srv-coldstore.mount           # this is the one that actually failed
journalctl -b -u srv-coldstore.mount
```

#### Why there is no start limit

The obvious setting -- `StartLimitIntervalSec=300` with `StartLimitBurst=5` and
`RestartSec=10s` -- gives up **permanently 51 seconds** after the first failure
and leaves the unit `failed`. Clearing that needs `systemctl reset-failed
coldstore`; `systemctl start` alone does not do it.

Fifty-one seconds is not enough headroom for the most likely first-boot failure,
a pool that has not imported yet. A missing `cold_storage_path` is fatal on
purpose (creating it would write the whole archive to the root filesystem), so
that boot ends with a dead service and no coldstore until someone logs in --
exactly the outcome the "a missing folder path is a warning, not an error" rule
exists to prevent everywhere else.

Retrying forever costs one journal line a minute. A real config error still shows
up immediately at the top of `systemctl status coldstore`, and a daemon that
cannot start binds no port and writes no file.

If you prefer the bounded version, put the two `StartLimit*` directives back --
and remember that recovering afterwards is `systemctl reset-failed coldstore &&
systemctl start coldstore`.

There is deliberately **no `Requires=syncthing.service`**: coldstore has no runtime
dependency on it, and a hard requirement would take coldstore down whenever
Syncthing was restarted.

### Shutdown

`TimeoutStopSec=120` is a data-safety setting. On `SIGTERM` the daemon cancels its
context and finishes the move it is in the middle of; a move that gets `SIGKILL`
instead leaves a row in `archiving` for startup recovery to resolve. The systemd
default of 90 s is not enough for a large file on a slow pool.

The wait covers the quota tick, the job worker and the thumbnail pass -- all
three are joined before `Run` returns. It does **not** cover a single-file
archive or restore issued straight over the API (`POST
/api/v1/files/{id}/archive`), which runs synchronously in its HTTP handler
goroutine; the process exits as soon as the daemon loop returns and abandons it
mid-move. That is survivable by construction -- a move only ever leaves a
complete destination or a `.coldstore-tmp` sibling, and `RecoverStartupState`
resolves the row on the next start -- but a bulk job is the cleaner way to move
a lot of files across a planned restart, because a job *is* waited for and is
re-queued if it is not finished.

### Filesystem sandbox

`ProtectSystem=strict` makes the entire filesystem read-only except `/dev`,
`/proc`, `/sys` and what `ReadWritePaths=` lists. Every entry there is
load-bearing:

| Path | Why it must be writable |
|---|---|
| `cold_storage_path` (`/srv/coldstore`) | `catalog.db` and its `-wal`/`-shm`, `archive/`, `thumbnails/`, and the `.coldstore-tmp` sibling every move writes before renaming. |
| Each `[[folders]].path` | coldstore **deletes** from here to free space and **writes** here on restore. Read-only breaks the entire product. |

Nothing else is writable. `/etc/coldstore` is read-only (config is only read),
`PrivateTmp=yes` supplies a private `/tmp`, and coldstore creates no temporary
files outside the two trees above.

`ProtectHome=read-only` is set and is compatible with `ReadWritePaths` entries
under `/home`: `ReadWritePaths` is applied after `ProtectHome` and punches through
it. What it buys over `ProtectSystem=strict` alone is that other users' homes stay
read-only even if a later `ReadWritePaths` edit is sloppy. `ProtectHome=tmpfs`
would hide the managed folders themselves and is not usable here.

Also set: `NoNewPrivileges`, an empty `CapabilityBoundingSet`, `PrivateDevices`,
`ProtectProc=invisible`, `ProcSubset=pid`, the four `ProtectKernel*`/`ProtectClock`
options, `RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`,
`LockPersonality`, `SystemCallArchitectures=native`,
`SystemCallFilter=@system-service`, and
`RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`. ffmpeg has been verified to run
under that syscall filter and those restrictions.

`UMask=0077` covers `catalog.db` and the thumbnails. It does **not** govern
archived or restored files: every move chmods its copy back to the source file's
own mode, so an archived `0644` photo is `0644` in cold storage and a restored one
returns to the Syncthing folder with the mode it had. That is deliberate -- a
restore that tightened permissions would propagate the new mode to every synced
device -- so do not "fix" that `chmod` to agree with the umask.

### Network filter

```
IPAddressDeny=any
IPAddressAllow=localhost 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16 fe80::/10 fd00::/8
```

Defence in depth for the LAN-only rule, enforced by cgroup BPF rather than by
coldstore. The three RFC1918 ranges plus link-local cover every home network, and
coldstore needs no outbound access at all. If your LAN uses something else, or the
UI becomes unreachable after enabling this, add your range or comment the pair out
-- the actual control is still the bind address plus the credential.

### Deliberately not enabled

| Option | Why not |
|---|---|
| `MemoryDenyWriteExecute=yes` | The Go runtime is fine with it, but ffmpeg pulls in codec and hardware-acceleration libraries that have historically mapped W+X pages. Breaking thumbnails is a worse trade than the hardening is worth. Enable it and confirm thumbnails still generate if you want it. |
| `PrivateUsers=yes` | Untested against the cross-device move path, which cares about file ownership on both sides. |
| `DynamicUser=yes` | Incompatible with running as the Syncthing user, which is a hard requirement (see step 3). |

### Verifying the unit

```bash
sudo systemd-analyze verify /etc/systemd/system/coldstore.service
sudo systemd-analyze security coldstore.service
```

`verify` catches typos and missing paths. `security` scores the sandbox; the
options above are what move it out of the "UNSAFE" band.

### Checking logs

```bash
# Follow live logs
journalctl -u coldstore -f

# Last 100 lines
journalctl -u coldstore -n 100

# Since last boot
journalctl -u coldstore -b
```

## Cold storage layout

The `cold_storage_path` directory has this structure:

```
/srv/coldstore/
  catalog.db              # SQLite database (WAL mode)
  catalog.db-wal          # WAL file (may exist during operation)
  catalog.db-shm          # shared memory file (may exist during operation)
  archive/
    CameraRoll/           # mirrors the folder label
      DCIM/
        IMG_0001.jpg      # offloaded files, preserving relative paths
        IMG_0002.jpg
    Books/
      SomeBook.pdf
  thumbnails/
    CameraRoll/
      DCIM/
        IMG_0001.jpg.thumb.jpg   # 400px max-edge JPEG thumbnails
    Books/
      SomeBook.pdf.thumb.jpg
```

- **`catalog.db`** is opened with `?_journal_mode=WAL&_busy_timeout=5000`.
- **`archive/<label>/`** mirrors the relative path structure of the Syncthing folder.
- **`thumbnails/<label>/`** also mirrors the relative path, with `.thumb.jpg` appended.
- Empty parent directories inside `archive/` are cleaned up automatically after restores.

## Cold storage filesystem: btrfs or ZFS

Either is a good choice. Both checksum every block, which complements the
SHA-256 verification coldstore performs during a cross-device move: together they
give end-to-end integrity from the Syncthing folder to the archive and back.
Pool and array creation are out of scope; what follows is what matters to
coldstore.

### Why the archive is the tier that needs redundancy

This is the opposite of the usual hot/cold intuition, and it decides your layout.

coldstore frees space by deleting the file from the Syncthing folder and letting
Syncthing propagate that deletion to every peer. **Once a file is offloaded, the
copy under `cold_storage_path` is the only copy that still exists anywhere.**
The Syncthing folders are, by construction, replicated to every device you own;
the archive is replicated to nothing.

So: the archive goes on the redundant, self-healing tier. The Syncthing folders
do not need one. If you have exactly one redundant array and one unprotected
disk, `cold_storage_path` goes on the array, and the unprotected disk gets media
and anything else you could re-acquire -- never a `[[folders]].path`, never
`cold_storage_path`, and never an entry in the unit's `ReadWritePaths=`.

A single-device btrfs still checksums (metadata `DUP`, data `single`), so
corruption there is *detected but not repairable*. That is right for
re-downloadable data and wrong for an archive.

### Compression

Both filesystems compress cheaply, and the archive is mostly already-compressed
media that will pass through at near-zero cost. What does compress is the SQLite
WAL, the thumbnails, and metadata blocks.

```bash
# btrfs -- a mount option, so it goes in fstab (see step 2)
compress=zstd:3

# zfs
zfs set compression=lz4 tank/coldstore
```

### Scrub (btrfs) / scrub (ZFS)

A checksummed filesystem only heals what it reads. Schedule a scrub, or
corruption sits undetected until the day you need the file.

```bash
# btrfs on Debian: btrfs-progs ships NO timer (the btrfs-scrub@.timer you may
# have seen is Fedora/openSUSE). Use the btrfsmaintenance package, which drives
# its schedule from /etc/default/btrfsmaintenance:
sudo apt install -y btrfsmaintenance
sudoedit /etc/default/btrfsmaintenance      # BTRFS_SCRUB_MOUNTPOINTS="/srv/coldstore"
sudo systemctl enable --now btrfs-scrub.timer
sudo btrfs scrub status /srv/coldstore

# zfs -- Debian's zfsutils-linux does ship a monthly scrub timer
sudo systemctl enable --now zfs-scrub-monthly@tank.timer
```

If you would rather not add a package, a one-line timer of your own is fine --
what matters is that a scrub runs on a schedule, not which unit starts it. A
checksummed filesystem only heals what it reads.

On a RAID1 btrfs a scrub repairs from the good copy automatically. On a
single-device one it can only report; `btrfs device stats /srv/coldstore` is
where the running error counts live.

### Snapshots

Snapshots here are primarily about recovering **the catalog** quickly. The
archive can be rebuilt by rescanning, but a catalog rebuilt from a rescan does
not know about archived files -- see [Backup](#backup).

```bash
# btrfs, via btrbk (apt install btrbk), snapshotting into /srv/vault/.snapshots
sudo btrbk run

# zfs
zfs snapshot tank/coldstore@$(date +%Y-%m-%d)
zfs list -t snapshot -o name -H tank/coldstore | head -n -30 | xargs -r -n1 zfs destroy
```

### The catalog on a copy-on-write filesystem

`catalog.db` lives inside `cold_storage_path`, so it cannot be moved into its own
no-CoW directory without moving it out of the archive tree entirely.

**Do not `chattr +C` the whole tree.** That disables checksums on the archive --
the one thing that most needs them -- to fix fragmentation in a database that is
a few megabytes. If SQLite fragmentation ever does become measurable, set the
flag on the empty file before anything writes to it; `+C` only takes effect on a
zero-length file:

```bash
sudo -u YOURUSER touch  /srv/coldstore/catalog.db
sudo -u YOURUSER chattr +C /srv/coldstore/catalog.db
```

### Free-space reporting is an estimate on btrfs

The 5%-free floor (see [Monitoring](#disk-space)) comes from `statfs`, and
btrfs's answer is a projection, not a fact: unallocated chunks, the RAID profile
and metadata over-allocation all move it, and `ENOSPC` can arrive while
`f_bavail` still looks healthy. `btrfs filesystem usage /srv/coldstore` is the
number to trust when you are close to the edge. On a volume sized for the job
this never matters; on one that is nearly full, do not treat "5% free" as a
precise threshold.

## Monitoring

### Key log messages to watch

| Level | Message | Meaning |
|---|---|---|
| `INFO` | `coldstore daemon starting` | Daemon started successfully. |
| `INFO` | `API server starting` | Carries `addr` and `auth_mode` (`none`, `basic`, or `tailscale`). `none` and `tailscale` are loopback-only. |
| `INFO` | `API mutation completed` | Audit record containing authenticated `actor`, `auth_provider`, method, path, status and duration. Tailscale mode attributes each mutation to its exact login. |
| `WARN` | `cross-origin API mutation rejected` | Tailscale-authenticated browser attempted a mutation from a foreign origin. The handler was not run. |
| `WARN` | `API server starting without authentication; reachable only from this host` | Loopback bind with no credentials. Fine for an SSH-tunnel setup; wrong if you meant to expose it on the LAN. |
| `WARN` | `preflight` | A folder path is missing or has no `.stfolder`. Not fatal -- the folder is skipped until it appears. Persisting past the boot means a real mount or path problem. |
| `INFO` | `running initial catalog scan` | Startup scan in progress. |
| `WARN` | `initial scan failed after all retries, starting without offload capability` | The daemon is running but will not offload files until a catalog sync tick succeeds. Investigate the scan error. |
| `WARN` | `skipping quota enforcement: initial scan incomplete` | Quota enforcement is gated on a successful initial scan. Not an error, but persistent appearances mean scans keep failing. |
| `ERROR` | `cold storage is not a mounted volume, refusing to archive` | `require_mountpoint` is set and `cold_storage_path` stopped being a mount point while the daemon was running. Every offload is refused until it is remounted; nothing has been lost. `findmnt` it. |
| `WARN` | `cold storage below 5% free, refusing to archive` | The cold storage filesystem is nearly full. Free space or expand the volume. A `cold_storage_low` event is also written to the `events` table, deduped to one an hour. |
| `WARN` | `folder wedged: pinned bytes exceed budget` | Pinned bytes alone are over the folder's budget, so enforcement cannot converge. Latched in the database and refreshed once a day. |
| `WARN` | `bulk eviction` | A single tick is about to shed an unusually large share of a folder. Worth reading before it finishes. |
| `WARN` | `archived file's path is occupied again` | A file has reappeared at the path of an archived record. Logged as a `conflict` event; resolve it in the UI. |
| `WARN` | `source changed during move, keeping both copies` | Syncthing rewrote the file mid-move. Both copies are kept, the new one as `.coldstore-superseded`. |
| `ERROR` | `panic in quota enforcement` | A bug triggered a panic during offloading. The daemon recovered and will retry on the next tick. File an issue with the panic value. |
| `ERROR` | `scan failed for folder` | A folder scan failed (permissions, missing directory, etc.). Other folders still scan normally. |
| `ERROR` | `offload failed` | A single file could not be moved to cold storage. The file's status is reverted to `synced` and it will be retried on the next tick. |

### Disk space

The daemon checks available space before every offload. If the cold storage
filesystem drops below 5% free, offloading pauses and a warning is logged.
Monitor the `events` table for `cold_storage_low` events to catch this
programmatically.

**There is no matching check on the restore path.** `Mover.RestoreIn` does not
probe free space on the Syncthing folder's filesystem before copying a file back
into it, so a bulk restore can fill that volume. It fails safely -- the
cross-device move writes to a `.coldstore-tmp` sibling, so an `ENOSPC` mid-copy
discards the temp, leaves the cold-storage source untouched and returns an error
-- but you get a run of failed restores rather than a refusal up front. If your
archive is larger than the free space in the folders it restores into, restore in
batches and watch `df`.

### What actually gets a thumbnail

`thumbnail.CanThumbnail` is an allowlist, and it is worth knowing exactly what
is on it:

| Pipeline | Extensions | Needs |
|---|---|---|
| Go imaging | `.jpg` `.jpeg` `.png` `.webp` `.gif` | nothing |
| ffmpeg (decode) | `.heif` `.heic` | ffmpeg |
| ffmpeg (frame extract) | `.mp4` `.mov` `.avi` `.mkv` `.webm` | ffmpeg |
| poppler (`pdftocairo`) | `.pdf` | poppler-utils |
| Go `archive/zip` | `.epub` | nothing |

The PDF path renders the first page that actually has something on it, trying up
to three pages before keeping page 1 regardless -- a cover sheet is a common
thing for a document to open with. The EPUB path extracts the book's declared
cover; a book that declares none, and has no image in its first ten sections,
gets no thumbnail. A cover stored as SVG is recorded as a clean failure rather
than pulling a vector rasteriser onto the box.

Everything else -- **`.mobi`, `.djvu`, office documents, plain text** -- still
gets no thumbnail and no error.

**A file with no extension is reached by its content, not its name.** The
scanner reads a bounded prefix of every new or changed file and records what the
bytes say it is, so a browser bookmark export called `reading` or a screenshot
saved without a suffix is thumbnailed and previewed like any other file. That
read happens once per file, on insert or when the mtime or size changes -- not
on every scan.

Previewing is wider than thumbnailing. PDFs, EPUBs and plaintext all open in the
browser through the reader the daemon ships, and **previewing an archived file
streams it out of cold storage without restoring it**: the row stays archived.
None of that needs poppler; poppler is only for the thumbnail.

Two knobs matter on a small box. `thumbnail_workers` is that many concurrent
decoders, and the Go path decodes a whole image into memory: a 48-megapixel JPEG
is roughly 190 MB of RGBA while it is being resized. On a machine with a few GB
of RAM and **no swap**, two of those plus Syncthing is the shape of an OOM kill.
Start at `thumbnail_workers = 1` on such a box, and configure swap before the
first thumbnail pass:

```bash
sudo fallocate -l 4G /swapfile && sudo chmod 600 /swapfile
sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
```

(`fallocate` is fine on ext4 and xfs. On btrfs a swapfile must be created with
`btrfs filesystem mkswapfile`, and it cannot live on a RAID1 profile at all.)

## Backup

**The SQLite catalog is the only state that matters.** Back it up regularly:

```bash
sqlite3 /srv/coldstore/catalog.db ".backup /path/to/backup/catalog.db"
```

Use the `.backup` command (not a file copy) to get a consistent snapshot even while the daemon is running with WAL mode.

**The archive directory** contains the offloaded files themselves. If the archive is lost, files are gone unless you have ZFS snapshots or another backup of the cold storage volume.

**Thumbnails** are fully regenerable. If lost, the daemon will regenerate them on its next catalog sync tick for any files with an empty `thumbnail_path`.

**The catalog is not reconstructible from the hot folders.** A scan can discover
only files still present in configured Syncthing folders. It cannot recover the
rows that map offloaded files to `archive/`, or restore pins, conflicts, runtime
folder policy, jobs and audit history. The SQLite `.backup` is therefore
irreplaceable state, not merely a faster way to rebuild a cache.

## Troubleshooting

### Files stuck in `archiving` or `restoring` state

`archiving` and `restoring` are transient states set just before a file is moved. If the daemon is killed mid-move, rows can be left in one of them.

**Automatic recovery:** the daemon resolves them once at startup, in `RecoverStartupState`, before the API server or any mover exists. It deliberately does **not** run on the periodic scan: recovery writes the filesystem without the mover lock, so running it alongside an in-flight move could delete the only copy of a file.

A move writes its destination to a `.coldstore-tmp` sibling and commits it with a rename, so at every instant the destination path is either absent or a complete, verified copy. That makes recovery decidable:

- **`archiving`** -- a complete archive copy is promoted to `archived` (and a leftover source with matching size and mtime is removed); no archive copy means the row reverts to `synced`; a copy that is a truncated prefix of the source is deleted; anything else that does not match is renamed aside as `.coldstore-superseded` and reported as a `conflict` event.
- **`restoring`** -- the cold-storage copy is authoritative. A truncated file at the Syncthing path is deleted and the row reverts to `archived`, because committing it would let Syncthing propagate the truncation to every device. A complete file at the Syncthing path commits the restore (and pins it).

Nothing is ever deleted to tidy the bookkeeping: any copy that could hold unique bytes is kept, renamed to `.coldstore-superseded`.

**Manual recovery:** If automatic recovery is not triggering (e.g., the daemon won't start), query the catalog directly:

```sql
-- Find stuck files
SELECT id, folder_label, relative_path, status FROM files WHERE status IN ('archiving', 'restoring');

-- If the file is in the archive, mark it archived
UPDATE files SET status = 'archived', archived_at = datetime('now') WHERE id = ?;

-- If the file is still in the syncthing folder, mark it synced
UPDATE files SET status = 'synced', archived_at = NULL WHERE id = ?;
```

### Catalog corruption recovery

If the catalog database is corrupt, stop the daemon and preserve the database plus
its WAL/SHM sidecars for diagnosis. Restore the newest consistent SQLite
`.backup`, then run `coldstore --status` before allowing the daemon to start. Do
not create an empty catalog and run `--scan` as a recovery procedure: that would
make hot files visible again while silently orphaning everything already in the
archive. If no backup exists, keep the daemon stopped and perform an explicit
forensic reconciliation of both trees; ad-hoc `INSERT` statements are not a safe
substitute for all catalog invariants.

### Daemon won't start

Run `coldstore --config /etc/coldstore/coldstore.toml --check` as the service user
first -- it reproduces every startup validation without opening the catalog or
binding a port, and prints the same message the daemon would.

| Message starts with | Meaning | Fix |
|---|---|---|
| `refusing to start: [server] listen_addr` | Non-loopback bind with no credentials. | Set `COLDSTORE_USERNAME`/`COLDSTORE_PASSWORD`, or bind loopback. See [the startup refusal](#the-startup-refusal). There is no override flag. |
| `[server] username is set but password is empty` | Half a credential; Basic Auth is incomplete. | Set both, or neither. |
| `[server] auth_mode = "tailscale" requires a loopback` | A direct network listener could accept forged identity headers. | Restore `127.0.0.1:8090` and use only Tailscale Serve HTTPS. |
| `[server] auth_mode = "tailscale" cannot be combined` | Basic credentials remain in TOML or the service environment. | Remove both Basic credentials as one reviewed change; do not print the environment file. |
| `[server] auth_mode = "tailscale" requires at least one` | The exact-login allowlist is empty. | Add the intended owner login(s) to `tailscale_allowed_users`. |
| `[server] listen_addr ... is not a valid host:port` | Missing port, or a stray space. | `"192.168.1.10:8090"`. |
| `no [[folders]] are declared` | Empty or truncated config, or the wrong `--config` path. | Check the path in `ExecStart=`. |
| `cold_storage_path ... does not exist` | The volume is not mounted, or a typo. | `findmnt /srv/coldstore`; then `btrfs device stats` or `zpool status`. coldstore will not create it. |
| `cold_storage_path ... is not a mount point` | `require_mountpoint` is set and the volume is not mounted -- the path is the empty stub on the root filesystem. | `findmnt` it, fix the mount, restart. **Do not** remove `require_mountpoint` to make the message go away: it is the only thing that stopped the archive landing on root. |
| `... is not writable ... read-only file system` | `ProtectSystem=strict` plus a path missing from `ReadWritePaths=`. The path is read-only **to this process only**, which is why `ls -ld` and a hand-run `--check` both look fine. | Add the path to `ReadWritePaths=`, then `systemctl daemon-reload && systemctl restart coldstore`. **Not `chown`** -- it cannot make a read-only mount writable. |
| `... is not writable ... permission denied` | Wrong `User=`, or wrong ownership on the directory. | `chown` it to the account Syncthing runs as. |
| `... is mounted (.stfolder present) but not writable` | Either of the two above, on a `[[folders]].path`. The message names which. | Read the errno in the message: `read-only file system` → the unit; `permission denied` → `User=`/ownership. |
| `coldstore would archive its own archive` | `cold_storage_path` is inside a managed folder, or vice versa. | Move one of them. |
| `folder "X" is declared more than once` | Duplicate `label`. | Labels are the primary key of the policy table. |

Other causes:

- **Database locked:** ensure no other process has the catalog open; check for stale WAL/SHM files.
- **`EROFS` on a `[[folders]].path`:** it is missing from `ReadWritePaths=` in the unit. This is a **startup** failure -- the daemon probes every mounted folder for writability before it opens the catalog -- so it will not run happily for an hour first. It also cannot be reproduced outside the unit, because the read-only-ness comes from the sandbox and nothing else.
- **The unit failed at boot and `systemctl start` will not bring it back:** you kept the bounded `StartLimit*` pair. Run `systemctl reset-failed coldstore` first. The shipped unit has no start limit precisely so this cannot happen.
- **`systemctl status coldstore` says "Dependency failed" and there is nothing in the daemon's own journal:** the `RequiresMountsFor=` mount unit failed, so coldstore never ran and `Restart=` does not apply. Look at `systemctl status srv-coldstore.mount` instead -- see [What `Restart=` does and does not retry](#what-restart-does-and-does-not-retry).
- **The daemon started, but the archive is on the root filesystem:** `cold_storage_path` was an empty mountpoint directory when it started. `findmnt /srv/coldstore` and read [coldstore will not create `cold_storage_path`](#coldstore-will-not-create-cold_storage_path----and-that-is-not-a-mount-guard). Stop the daemon before touching anything.
- **A folder warning at startup is not a failure.** `does not exist yet` and `has no .stfolder marker` are warnings by design, so a boot that beats the mount does not leave the service down.

### Files not being offloaded

1. Verify the folder is over budget: the enforcement log line reports the folder's on-device size (`FolderSizes`) against its budget.
2. Check if files match a persistent pattern (config `persistent` array or `persistent_paths` table).
3. Check the grace period: files with recent mtime or atime (within `grace_period_minutes`) are skipped.
4. Check cold storage free space: offloading pauses below 5% free.
5. Confirm the initial scan completed successfully (look for `initial scan completed` or the absence of `skipping quota enforcement` warnings).
