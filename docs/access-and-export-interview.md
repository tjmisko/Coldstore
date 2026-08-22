# Owner interview — metadata export and browser access

Status: prepared for owner review on 2026-08-21. This document explains the choices and
records decisions; it does not authorize deployment, public exposure, or production changes.

## Current summary

The owner has stated these requirements:

- coldstore must work from an iPhone, Linux, macOS and probably Windows;
- access away from home would be useful, but is not required for v1; and
- Tailscale is already installed for another project on several owner devices; and
- the earlier interface interview asked for the possibility of guest access from another
  person's device. Zero-install public guest access is deferred beyond v1 by the private
  Tailscale decision.

Two unrelated decisions appear beside each other in the tasklist:

1. **XMP export** is an optional way to carry coldstore's tags and descriptions into other
   photo-management software. It has nothing to do with browser or remote access.
2. **Session authentication** is how a browser remains signed in and how the server decides
   whether that person is an owner or a read-only guest. It becomes load-bearing only if
   coldstore is reachable beyond a private network.

The owner selected this v1 direction on 2026-08-21:

- defer XMP export implementation and retain it as a portability follow-up;
- use Tailscale Serve as the private HTTPS path for the owner's devices;
- keep the Go server on loopback and require the existing Basic Auth through Serve;
- do not trust Tailscale identity headers for v1 authorization; and
- defer public-domain guest sessions until zero-install guest access is required.

This path supports all named operating systems, can provide away-from-home access, and does
not make a destructive archive control plane public merely to satisfy v1.

## Part 1 — XMP export in plain language

### What XMP is

XMP is a standard XML format for descriptive metadata. A photo-management application can
store information such as keywords, a caption, capture information and coordinates in an
XMP document. When that document is kept beside, rather than embedded in, the photo, it is
called a **sidecar**.

For example, coldstore might know this about a photo:

```text
File:       2025/Iceland/IMG_1234.HEIC
Tags:       trips/iceland, family
Description: Reykjavik harbor at dusk
Location:   64.1466, -21.9426
```

An export would create a small XML file representing that information. The current design
places it under a separate cold-storage tree:

```text
archive/2025/Iceland/IMG_1234.HEIC
sidecars/2025/Iceland/IMG_1234.HEIC.xmp
```

It would **not** edit the HEIC, and it would not add anything to an iPhone/Syncthing folder.
That separation is important: writing sidecars into a synced folder would make Syncthing
propagate and coldstore catalog those files, consume quota, and disturb archive ordering.

### What the export is for

Coldstore's tags currently live in its SQLite catalog. That is ideal for fast browsing and
search inside coldstore, but another photo application does not understand coldstore's
database. XMP export provides a conventional interchange copy that a compatible desktop
photo manager can import.

The export is useful if the owner wants to:

- migrate away from coldstore without manually recreating tags and captions;
- use the same organization work in a compatible desktop photo manager;
- keep a human/tool-readable metadata copy alongside cold storage; or
- preserve metadata independently from coldstore's database format.

Compatibility is application-specific. Exporting an XMP file does not make Apple Photos or
every other photo application automatically discover it; the target application and naming
convention must be tested before claiming interoperability.

### What the export is not

- It is not a photo backup; it contains metadata, not image bytes.
- It is not required for tags, descriptions, search or browsing inside coldstore.
- It does not enable iPhone, browser, LAN or remote access.
- It is not the primary recovery plan. The catalog and archive still need real backups.
- It should never rewrite an original photo or write into a Syncthing-managed folder.

### Do we need it?

Probably not for v1. The owner has not yet named a second photo manager or a migration need.
The safest scope is to file the designed follow-up and defer implementation. A future first
version should be an explicit, repeatable CLI export into cold storage, not an automatic
background writer. That keeps the feature reversible and makes compatibility testable.

### XMP interview

Record answers directly below each question.

1. Do you currently use, or expect to use, another photo-management application that should
   receive coldstore tags or captions? If yes, which application?
   - **Answer:** pending.
2. Is portability a near-term need, or merely insurance against a future migration?
   - **Answer:** pending.
3. Would a manual export command be sufficient, or would you expect automatic refreshes?
   - **Provisional answer:** manual, on demand.
4. Should GPS data be included by default? An exported sidecar may make sensitive location
   information easier for other software or people to find.
   - **Answer:** pending.
5. Is it acceptable to defer all XMP implementation until after v1?
   - **Provisional answer:** yes.

## Part 2 — access, authentication, sessions and authorization

These words describe separate layers:

- **Reachability:** can the device make a network connection to coldstore?
- **TLS/HTTPS:** is that connection encrypted and connected to the intended server?
- **Authentication:** who is making the request?
- **Session:** how does the browser stay signed in across later requests?
- **Authorization:** may that identity browse, download, tag, rename, restore or archive?

A secure login page does not make a public control plane safe by itself. Every API and file
route must enforce authorization on every request.

### Cross-platform consequence

iPhone Safari and mainstream browsers on macOS, Linux and Windows all support HTTPS cookies.
Therefore the operating-system list does not require a native coldstore app, JWTs or an
external identity provider. The meaningful platform question is whether installing a
private-network client such as Tailscale on each owner's device is acceptable.

## Part 3 — realistic network-access options

### Option A — home network only

The browser connects only while on the home LAN. This has the smallest Internet attack
surface and needs no client app, but the current loopback-only listener still needs a safe
LAN-facing proxy and an HTTPS/trust plan. Local Caddy certificates are not automatically
trusted by every device, so “just use local HTTPS” includes distributing a local CA or using
a publicly trusted hostname.

Best when: away access and guests are both deferred.

Trade-off: simple conceptually, but certificate and LAN firewall details still exist.

### Option B — Tailscale Serve, private owner access

Install Tailscale on the server and the owner's iPhone, Mac, Linux and Windows devices.
Tailscale Serve gives the loopback web server a private HTTPS name reachable inside the
tailnet, including away from home. It can pass authenticated Tailscale identity headers to
a loopback backend, so coldstore can eventually recognize the owner without inventing a
public login system.

Best when: the owner accepts installing one client on each personal device.

Trade-off: every user/device must join or be shared into the tailnet. It is excellent for
the owner's devices, but awkward for a casual guest who should open a normal link with no
installation. The backend must remain loopback-only if identity headers are trusted.

**Selected for v1.** The owner already uses Tailscale on several machines. This path supports
every named platform, supplies optional away-from-home access, avoids router port forwarding,
and does not make coldstore public. Basic Auth remains a separate application gate.

### Option C — public domain plus Caddy

A public DNS name points to the home connection. The router forwards ports 80/443 to Caddy;
Caddy obtains and renews a public TLS certificate and proxies to coldstore on loopback.
Any modern browser can connect without an installed client.

Best when: zero-install access from arbitrary guest devices is a real requirement.

Trade-off: this requires a suitable public IPv4/IPv6 path, domain and DNS/DDNS ownership,
deliberate firewalling, and production-grade application sessions and authorization. Public
guest access must be read-only while owner mutation remains private or separately gated.

### Option D — Cloudflare Tunnel plus Access

The server creates an outbound tunnel, so it can publish a hostname without inbound router
port forwarding. Cloudflare Access can authenticate ordinary-browser users through an
identity provider or email flow.

Best when: public reachability is required but direct inbound connectivity is unavailable.

Trade-off: media traffic traverses Cloudflare, coldstore still needs application-level role
authorization, and Cloudflare's published media-delivery guidance makes this a questionable
default for a full-resolution photo/video library. Treat it as a topology to evaluate, not
as a shortcut around authorization.

## Part 4 — session-authentication options

### Existing HTTP Basic Auth

The browser sends the same username/password on every request. It is small and useful for
CLI/runbook compatibility, but browser logout is poor, every user effectively shares one
credential, and individual sessions or guests cannot be revoked. Keep it as a compatibility
path; do not use it as the public guest model. The owner selected it behind Tailscale for v1.

### Tailscale identity

Tailscale authenticates the device/user before traffic reaches Serve, which can add verified
identity headers for a loopback backend. For private owner-only v1 this may remove the need
for a new coldstore login/session entirely. Coldstore would still need explicit mapping from
the Tailscale identity to owner permissions, and Basic Auth can remain for existing tools.

### Opaque server-side session cookie

After a password login or invite redemption, the server generates a random, meaningless
session ID. The browser holds it in a `Secure`, `HttpOnly`, `SameSite` cookie; SQLite holds
the identity, role, allowed resources, expiry and revocation state. Logout or owner
revocation deletes/disables that row.

This is the recommended application-managed model if public browser access is selected. It
is straightforward to revoke, audit and scope. A state-changing request also needs CSRF
protection; `SameSite` is useful defense in depth, not a replacement for it.

### Signed stateless cookie or JWT

The cookie contains signed claims that the server verifies without a database lookup. This
can help a large distributed service, but coldstore is one daemon with one SQLite database.
Immediate per-session revocation still needs server-side state, removing the main benefit.
Do not choose this merely because the current tasklist says “HMAC session”; an opaque random
session is a simpler fit for the required revocation model.

### External authentication proxy

Cloudflare Access, Authelia, Authentik or another identity-aware proxy performs login before
proxying to coldstore. This can avoid storing owner passwords in coldstore and can add MFA,
but it adds another service or provider. Coldstore must still enforce owner versus read-only
guest permissions rather than trusting hidden buttons or proxy method filters.

### What “token-based later” should mean

Yes, Basic Auth can be upgraded without changing the Tailscale network path. There are two
different token uses:

- **Browser sessions:** after login, use a random opaque token in a secure cookie. Keep its
  role, expiry and revocation state in SQLite. Do not put a bearer token or JWT in browser
  local storage; a cookie can be `HttpOnly`, while JavaScript-readable storage cannot.
- **CLI/API clients:** if Basic Auth becomes limiting, issue separately named bearer tokens
  with a hash stored server-side, an expiry, explicit scopes and individual revocation. Send
  them only over HTTPS in the `Authorization` header.

Tailscale device/user identity is a third kind of authentication and can also be evaluated
later. For v1, Coldstore does not consume Serve's identity headers: Tailscale controls network
membership and Basic Auth controls the application. This keeps the first rollout small and
allows session or API tokens to be added without making the backend public.

## Part 5 — proposed application session if public access is selected

The conservative design is:

- use an opaque, cryptographically random session ID stored server-side;
- set a host-only `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/` cookie over HTTPS;
- store no identity, role or sensitive data in the browser token;
- model one owner role and individually revocable, expiring read-only guest grants;
- authorize HTML, APIs, thumbnails, previews, original downloads and Range requests;
- keep owner mutations unavailable to guests regardless of frontend controls;
- use CSRF tokens for non-GET operations and rate-limit login attempts;
- provide real logout, guest revocation, idle/absolute expiry and audit events;
- retain Basic Auth for explicitly supported CLI/admin compatibility; and
- trust proxy/identity headers only from the selected loopback proxy path.

This works in Safari on iPhone/macOS and normal browsers on Linux/Windows without a native
coldstore app. It is intentionally more work than Tailscale owner access because a stolen
public session can reach the archive from anywhere.

## Part 6 — owner interview

The first four questions choose v1. The rest specify public sessions only if needed.

### V1 boundary

1. Are you willing to install Tailscale on your own iPhone, Mac, Linux and Windows devices?
   - **Answer:** yes. It is already installed for another project on several machines.
2. Must a guest be able to open coldstore in v1 without installing anything or joining an
   account/network?
   - **Answer:** no for v1. Public zero-install guest access is deferred.
3. For v1, is “works at home from all of my devices” sufficient, with away access treated as
   a bonus rather than a release requirement?
   - **Answer:** yes. Tailscale may provide the bonus remote path.
4. Should owner actions such as archive, restore, rename and configuration be available away
   from home, or should remote access be browse/download only?
   - **Answer:** pending.

### Network ownership

5. Do you already own a domain you would use, or are you willing to purchase one?
   - **Answer:** pending; irrelevant if v1 uses Tailscale only.
6. Are you willing and able to forward ports 80/443 and expose the home public IP, or would
   an outbound tunnel be required?
   - **Answer:** pending; do not probe without authorization.
7. Is it acceptable for photo/video traffic and request metadata to traverse a provider such
   as Cloudflare?
   - **Answer:** pending.

### Identity and permissions

8. Is there exactly one owner/admin identity?
   - **Provisional answer:** yes.
9. Should guests see the entire library, selected folders/albums, or explicit shared items?
   - **Answer:** pending.
10. May guests download originals and full-resolution video, or only browse derivatives?
    - **Answer:** pending.
11. Should a guest authenticate with an expiring invite link, an approved email login, or a
    password created for that guest?
    - **Answer:** pending.
12. What default lifetime is appropriate for owner devices and guest grants?
    - **Starting proposal:** owner 30 days; guest 7 days; both individually revocable.

### Failure and recovery

13. Should public access have a single application kill switch in addition to stopping the
    proxy/tunnel?
    - **Provisional answer:** yes.
14. What event should force every session to be revoked: password change, suspected link
    leak, restore from backup, or all three?
    - **Starting proposal:** all three, with an explicit revoke-all command.

## Decision record

Update this table as the interview proceeds. A row is not approved merely because a
provisional recommendation appears above.

| Decision | State | Owner answer |
|---|---|---|
| XMP export in v1 | Pending | Provisionally defer |
| V1 network path | Decided | Tailscale Serve to the loopback backend |
| V1 application auth | Decided | Retain Basic Auth through Serve |
| Later token upgrade | Direction set | Opaque browser sessions; scoped API tokens if needed |
| Zero-install guests in v1 | Decided | Deferred beyond v1 |
| Remote owner mutation | Pending | — |
| Public domain/proxy | Deferred | Revisit when zero-install guests are required |
| Session implementation | Pending | Opaque server-side cookie if public access wins |
| Guest resource scope | Pending | — |
| Guest download/video permission | Pending | — |
| Owner/guest session lifetime | Pending | Proposal: 30 days / 7 days |

## Primary references

- [Adobe XMP overview](https://www.adobe.com/products/xmp.html)
- [Tailscale supported installations](https://tailscale.com/docs/install)
- [Tailscale Serve and identity headers](https://tailscale.com/docs/features/tailscale-serve)
- [Caddy automatic HTTPS](https://caddyserver.com/docs/automatic-https)
- [Caddy reverse proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)
- [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/)
- [Cloudflare self-hosted public applications](https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/self-hosted-public-app/)
- [Cloudflare media-delivery guidance](https://developers.cloudflare.com/fundamentals/reference/policies-compliances/delivering-videos-with-cloudflare/)
- [OWASP Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)
- [OWASP CSRF Prevention Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html)
