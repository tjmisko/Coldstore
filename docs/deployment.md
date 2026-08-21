# Coldstore Deployment Runbook

**Deploying for the first time?** Read
[Security posture](#security-posture-lan-only-basic-auth), then work straight
through [First deployment on a fresh Debian box](#first-deployment-on-a-fresh-debian-box).
Everything after that is reference material.

## Prerequisites

- **Go 1.25.7+**, or any Go 1.21+ with the default `GOTOOLCHAIN=auto` and
  outbound access to `proxy.golang.org`. `go.mod` requires 1.25.7 and Debian
  trixie packages 1.24, so on a stock Debian box the first build silently
  downloads a second toolchain (`go: downloading go1.25.7`). With
  `GOTOOLCHAIN=local`, or on a box with no outbound internet, that build fails
  -- install Go 1.25.7 by hand there.
- **Node 20+** and npm (for building the React frontend). Run `npm ci` in `web/`
  once per clone; `web/node_modules` is gitignored and `make build` cannot
  compile the frontend without it.
- **ffmpeg** -- required for HEIF/HEIC decoding and video thumbnail extraction
- **Syncthing** -- running and configured with the folders you want to manage
- **SQLite 3** -- linked via `go-sqlite3` (CGo; a C compiler is required at build time)
- **ZFS** (optional) -- recommended for the cold storage volume; see the ZFS section below

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

### Development

```bash
make dev            # go run with local coldstore.toml
make dev-web        # React dev server with hot reload (port 5173)
make test           # go test ./...
```

---

## Security posture: LAN only, basic auth

Read this before you change `listen_addr`.

**The web UI is a control plane, not a viewer.** Whoever can reach it can pin,
archive, restore, run bulk jobs across thousands of files, and rewrite each
folder's budget and enabled flag. Archiving is not a local operation: coldstore
frees space by deleting the file from the Syncthing folder and letting Syncthing
propagate that deletion to every device you own. An unauthenticated
`POST /api/v1/jobs {"action":"archive"}` is a remote delete button for your phone's
camera roll.

The settled posture is deliberately small:

| | |
|---|---|
| Reachability | LAN only. Never port-forwarded, never on a public address. |
| Authentication | HTTP Basic Auth, checked in constant time on every route. |
| Transport | Plain HTTP. **Cleartext basic auth on the LAN is an accepted tradeoff.** |
| Not present, on purpose | TLS, reverse proxy, sessions, OAuth, user accounts, roles. |

A home LAN daemon does not need an identity system. It does need to be impossible
to leave open by accident, which is what the rest of this section is about.

### The startup refusal

**coldstore refuses to start when `listen_addr` is not loopback and either
credential is empty.** This is a hard error, exit 1, before the catalog is opened
and before any socket is bound. There is no flag to skip it.

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

To reach the UI from your phone or laptop, do both of these:

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
- **There is no CSRF protection.** With basic auth the browser will attach
  credentials to a cross-site form post once you have authenticated. Do not leave
  the tab authenticated on a machine you are also browsing untrusted sites from.
- **`IPAddressAllow=` in the unit is defence in depth, not the control.** The
  control is the bind address plus the credential.

---

## First deployment on a fresh Debian box

Run through this in order. It ends with an authenticated, LAN-bound daemon and a
verification that it is not reachable from anywhere else.

### 1. Prerequisites on the box

```bash
sudo apt update
sudo apt install -y ffmpeg sqlite3 zfsutils-linux   # ffmpeg is required; zfs only if you use it

# Only if you are building on this box (the usual case):
sudo apt install -y golang-go nodejs npm build-essential
```

`build-essential` is not optional: the catalog uses `go-sqlite3`, which is CGo.
Debian's `golang-go` is 1.24 and `go.mod` asks for 1.25.7, so the first build
downloads the newer toolchain -- see [Prerequisites](#prerequisites) if this box
has no outbound internet.

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

### 2. Cold storage dataset

```bash
sudo zfs create -o mountpoint=/srv/coldstore -o compression=lz4 tank/coldstore
# without ZFS:
# sudo mkdir -p /srv/coldstore
```

coldstore **will not create `cold_storage_path` for you**, and refuses to start if
it is missing. That is on purpose: creating it would silently write the catalog and
the entire archive to the root filesystem on any boot where the pool had not
mounted yet.

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

- `cold_storage_path` -- the dataset from step 2.
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

`--check` loads the config, validates it, inspects the machine, prints a summary and
exits. It never opens the catalog and never binds a port. Sample output:

```
config           /etc/coldstore/coldstore.toml
cold storage     /srv/coldstore (exists, writable)
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
`API server starting ... authenticated=true`, `running initial catalog scan`, one
`folder scan complete` per folder, then `thumbnail pass complete`.

`authenticated=false` in that line means the daemon is on loopback with no
credentials. On any other address it could not have started at all.

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

## Preflight checklist

`coldstore --config <path> --check` performs all of the automated checks. Run it as
the service user, with the same environment the unit will have.

| Check | Failure mode | Fatal? |
|---|---|---|
| Config parses as TOML | `parsing config: ...` | yes |
| `listen_addr` is `host:port` | `not a valid host:port` | yes |
| Non-loopback bind has credentials | `refusing to start: ...` | yes |
| Both credentials set, or neither | `basic auth would be silently disabled` | yes |
| At least one `[[folders]]` block | `no [[folders]] are declared` | yes |
| `cold_storage_path` is absolute | `must be absolute` | yes |
| `cold_storage_path` exists and is a directory | `does not exist. coldstore will not create it` | yes |
| `cold_storage_path` is writable by this user | `is not writable by this user (uid N)` | yes |
| Folder labels are unique | `declared more than once` | yes |
| Folder paths are absolute | `must be absolute` | yes |
| No two folders share a path | `two policy rows over one directory would fight` | yes |
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
- [ ] On ZFS with a native `mountpoint=`, the unit's `After=zfs-mount.service` /
      `Wants=zfs-mount.service` pair is uncommented. `findmnt -no SOURCE,FSTYPE
      /srv/coldstore` tells you which case you are in.
- [ ] `User=` in the unit is the account Syncthing runs as.
- [ ] `catalog.db` is backed up (see [Rolling back](#rolling-back), below).
- [ ] The port is not forwarded on your router.

---

## Rolling back

**Back up `/srv/coldstore/catalog.db` before every upgrade.**

The catalog carries a `PRAGMA user_version` and each release migrates it forward.
An **older binary opens a migrated catalog without complaining**. It does not know
about the `pinned` column and it does not read the budget from the database, so on
its first quota tick it will archive every pinned file -- and Syncthing will
delete each one from every device you own.

```bash
sudo systemctl stop coldstore
sudo -u YOURUSER sqlite3 /srv/coldstore/catalog.db \
  ".backup /srv/coldstore/catalog.db.$(date +%F)"
# ... upgrade ...
sudo systemctl start coldstore
```

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
| `listen_addr` | string | `"127.0.0.1:8090"` | Address and port for the web UI and REST API. **Loopback by default.** Any other value requires credentials -- see [Security posture](#security-posture-lan-only-basic-auth). |
| `username` | string | `""` | HTTP Basic Auth username. Overridden by `COLDSTORE_USERNAME`. |
| `password` | string | `""` | HTTP Basic Auth password. Overridden by `COLDSTORE_PASSWORD`. Prefer the environment variable so the password is not in this file. |

Both credential fields must be set or neither. Setting one and not the other is a
startup error, because `Server.Handler()` falls back to the bare mux when either
is empty -- you would believe auth was on when it was off.

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
| `label` | string | required | Human-readable folder name. Used as the top-level directory name inside the archive and thumbnails trees. Must be unique across all folders. |
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
files. The second line matters on the restore path: when a restore's source
vanishes mid-copy, coldstore keeps the bytes rather than destroying them, under a
`.coldstore-superseded` name **inside the Syncthing folder** -- and without the
ignore line Syncthing propagates that name to every device. The trailing `*` is
required: repeated collisions produce `.coldstore-superseded.2`, `.3`, and so on.

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
| `label`, `path`, `syncthing_id` | `coldstore.toml` | over SSH | on restart |
| budget | database | web UI | next enforcement tick, no restart |
| grace period | database | web UI | next enforcement tick, no restart |
| enabled/disabled | database | web UI | next enforcement tick, no restart |
| persistent globs | database | web UI | next enforcement tick, no restart |
| pins | database | web UI | immediately |

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

Persistent patterns use Go's `filepath.Match` semantics:

- `*` matches any sequence of non-separator characters. It does **not** cross directory boundaries.
- `?` matches any single non-separator character.
- `[abc]` matches any character in the set.
- Patterns are matched against the file's **relative path** within the folder.

Examples:

```toml
persistent = [
  "Albums/Starred/*",      # all files directly inside Albums/Starred/
  "Albums/Starred/*/*",    # one level deeper
  "important.txt",         # a specific file at the folder root
  "Docs/*.pdf",            # PDFs in the Docs/ directory only
]
```

To protect an entire subtree, you need a pattern for each depth level -- `*` alone will not recurse. Persistent patterns can also be added at runtime via the API; those are stored in the `persistent_paths` table in the catalog database.

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
| `Restart=on-failure`, `RestartSec=60s` | Retry forever, once a minute, so a slow pool import is recovered from rather than given up on. |

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

## ZFS considerations

ZFS is recommended for the cold storage volume. Pool creation and management are out of scope, but some relevant settings:

**Checksumming.** ZFS checksums every block by default (`checksum=on`). This complements the SHA-256 verification that coldstore performs during cross-device moves. Together they provide end-to-end integrity from source to archive.

**Compression.** Enable LZ4 compression on the dataset:

```bash
zfs set compression=lz4 tank/coldstore
```

LZ4 has negligible CPU overhead and compresses well for many media metadata blocks, SQLite WAL pages, and thumbnails. Raw photos and videos are already compressed and will pass through at near-zero cost.

**Snapshots.** A simple cron-based snapshot strategy works well:

```bash
# Daily snapshot, keep 30
zfs snapshot tank/coldstore@$(date +%Y-%m-%d)

# Prune snapshots older than 30 days
zfs list -t snapshot -o name -H tank/coldstore | head -n -30 | xargs -r -n1 zfs destroy
```

Snapshots protect against accidental deletion and catalog corruption. Since the archive can always be reconstructed from a rescan (see Backup), snapshots are primarily valuable for recovering the catalog database quickly without a full rescan.

## Monitoring

### Key log messages to watch

| Level | Message | Meaning |
|---|---|---|
| `INFO` | `coldstore daemon starting` | Daemon started successfully. |
| `INFO` | `API server starting` | Carries `addr` and `authenticated`. `authenticated=false` is only reachable on a loopback bind -- on any other address the daemon refuses to start. |
| `WARN` | `API server starting without authentication; reachable only from this host` | Loopback bind with no credentials. Fine for an SSH-tunnel setup; wrong if you meant to expose it on the LAN. |
| `WARN` | `preflight` | A folder path is missing or has no `.stfolder`. Not fatal -- the folder is skipped until it appears. Persisting past the boot means a real mount or path problem. |
| `INFO` | `running initial catalog scan` | Startup scan in progress. |
| `WARN` | `initial scan failed after all retries, starting without offload capability` | The daemon is running but will not offload files until a catalog sync tick succeeds. Investigate the scan error. |
| `WARN` | `skipping quota enforcement: initial scan incomplete` | Quota enforcement is gated on a successful initial scan. Not an error, but persistent appearances mean scans keep failing. |
| `WARN` | `cold storage below 5% free, pausing offload` | The cold storage filesystem is nearly full. Free space or expand the volume. An event is also logged to the `events` table. |
| `ERROR` | `panic in quota enforcement` | A bug triggered a panic during offloading. The daemon recovered and will retry on the next tick. File an issue with the panic value. |
| `ERROR` | `scan failed for folder` | A folder scan failed (permissions, missing directory, etc.). Other folders still scan normally. |
| `ERROR` | `offload failed` | A single file could not be moved to cold storage. The file's status is reverted to `synced` and it will be retried on the next tick. |

### Disk space

The daemon checks available space before every offload. If the cold storage filesystem drops below 5% free, offloading pauses and a warning is logged. Monitor the `events` table for `warning` type events to catch this programmatically.

## Backup

**The SQLite catalog is the only state that matters.** Back it up regularly:

```bash
sqlite3 /srv/coldstore/catalog.db ".backup /path/to/backup/catalog.db"
```

Use the `.backup` command (not a file copy) to get a consistent snapshot even while the daemon is running with WAL mode.

**The archive directory** contains the offloaded files themselves. If the archive is lost, files are gone unless you have ZFS snapshots or another backup of the cold storage volume.

**Thumbnails** are fully regenerable. If lost, the daemon will regenerate them on its next catalog sync tick for any files with an empty `thumbnail_path`.

**The catalog is reconstructible.** If the catalog database is lost or corrupt, you can rebuild it from the files on disk:

```bash
coldstore --config /etc/coldstore/coldstore.toml --scan
```

This walks all configured Syncthing folders and populates the catalog. However, it will not know about archived files -- those will need to be manually reconciled or restored.

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

If the catalog database is corrupt:

1. Stop the daemon: `sudo systemctl stop coldstore`
2. Move the corrupt database aside: `mv /srv/coldstore/catalog.db /srv/coldstore/catalog.db.corrupt`
3. Also remove WAL/SHM files if present: `rm -f /srv/coldstore/catalog.db-wal /srv/coldstore/catalog.db-shm`
4. Run a one-shot scan to rebuild: `coldstore --config /etc/coldstore/coldstore.toml --scan`
5. Restart the daemon: `sudo systemctl start coldstore`

The rebuilt catalog will contain all files currently present in the Syncthing folders (marked `synced`). Files in the `archive/` directory will **not** be automatically re-cataloged by `--scan` -- they must be restored manually or you can insert records directly:

```sql
-- Example: re-catalog an archived file
INSERT INTO files (folder_label, relative_path, filename, extension, size_bytes, file_mtime, status)
VALUES ('CameraRoll', 'DCIM/IMG_0001.jpg', 'IMG_0001.jpg', '.jpg', 4194304, '2024-06-15 10:30:00', 'archived');
```

### Daemon won't start

Run `coldstore --config /etc/coldstore/coldstore.toml --check` as the service user
first -- it reproduces every startup validation without opening the catalog or
binding a port, and prints the same message the daemon would.

| Message starts with | Meaning | Fix |
|---|---|---|
| `refusing to start: [server] listen_addr` | Non-loopback bind with no credentials. | Set `COLDSTORE_USERNAME`/`COLDSTORE_PASSWORD`, or bind loopback. See [the startup refusal](#the-startup-refusal). There is no override flag. |
| `[server] username is set but password is empty` | Half a credential; basic auth would be silently off. | Set both, or neither. |
| `[server] listen_addr ... is not a valid host:port` | Missing port, or a stray space. | `"192.168.1.10:8090"`. |
| `no [[folders]] are declared` | Empty or truncated config, or the wrong `--config` path. | Check the path in `ExecStart=`. |
| `cold_storage_path ... does not exist` | The pool is not mounted, or a typo. | `zpool status`, `findmnt /srv/coldstore`. coldstore will not create it. |
| `... is not writable ... read-only file system` | `ProtectSystem=strict` plus a path missing from `ReadWritePaths=`. The path is read-only **to this process only**, which is why `ls -ld` and a hand-run `--check` both look fine. | Add the path to `ReadWritePaths=`, then `systemctl daemon-reload && systemctl restart coldstore`. **Not `chown`** -- it cannot make a read-only mount writable. |
| `... is not writable ... permission denied` | Wrong `User=`, or wrong ownership on the directory. | `chown` it to the account Syncthing runs as. |
| `... is mounted (.stfolder present) but not writable` | Either of the two above, on a `[[folders]].path`. The message names which. | Read the errno in the message: `read-only file system` → the unit; `permission denied` → `User=`/ownership. |
| `coldstore would archive its own archive` | `cold_storage_path` is inside a managed folder, or vice versa. | Move one of them. |
| `folder "X" is declared more than once` | Duplicate `label`. | Labels are the primary key of the policy table. |

Other causes:

- **Database locked:** ensure no other process has the catalog open; check for stale WAL/SHM files.
- **`EROFS` on a `[[folders]].path`:** it is missing from `ReadWritePaths=` in the unit. This is a **startup** failure -- the daemon probes every mounted folder for writability before it opens the catalog -- so it will not run happily for an hour first. It also cannot be reproduced outside the unit, because the read-only-ness comes from the sandbox and nothing else.
- **The unit failed at boot and `systemctl start` will not bring it back:** you kept the bounded `StartLimit*` pair. Run `systemctl reset-failed coldstore` first. The shipped unit has no start limit precisely so this cannot happen.
- **A folder warning at startup is not a failure.** `does not exist yet` and `has no .stfolder marker` are warnings by design, so a boot that beats the mount does not leave the service down.

### Files not being offloaded

1. Verify the folder is over budget: the enforcement log line reports the folder's on-device size (`FolderSizes`) against its budget.
2. Check if files match a persistent pattern (config `persistent` array or `persistent_paths` table).
3. Check the grace period: files with recent mtime or atime (within `grace_period_minutes`) are skipped.
4. Check cold storage free space: offloading pauses below 5% free.
5. Confirm the initial scan completed successfully (look for `initial scan completed` or the absence of `skipping quota enforcement` warnings).
