# Coldstore co-owner onboarding

This runbook gives one additional co-owner full Coldstore access through Tailscale while
exposing no other server service. The selected design uses the co-owners' separate Tailscale
identities as Coldstore authentication; it does not share an application password.

Production binary/configuration changes and service restarts remain owner-mediated. The
feature must land, be built and pass the deployment gates before replacing today's Basic
Auth configuration.

## Access contract

```text
co-owner browser
    -> TCP 443 inside Tailscale only
    -> Tailscale Serve HTTPS
       (removes caller-supplied identity headers, inserts Tailscale-User-Login)
    -> http://127.0.0.1:8090
    -> Coldstore exact-login allowlist
    -> existing full owner web UI/API
```

The boundary is exact:

- Tailscale Serve, never Funnel;
- one explicitly shared server machine, not general membership in the owner's tailnet;
- only TCP 443 on that machine;
- only the Coldstore reverse proxy on Serve's HTTPS listener;
- Coldstore remains bound to `127.0.0.1:8090`;
- `auth_mode = "tailscale"` requires an exact allowlist of both co-owner logins;
- requests without a listed `Tailscale-User-Login` are rejected without a Basic Auth prompt;
- browser mutations with a foreign `Origin` or cross-site Fetch Metadata are rejected;
- accepted mutations are journalled with login, method, path and response status;
- no SSH, Syncthing UI/API, raw port 8090, filesystem share, shell or other server service is
  included; and
- each admitted co-owner receives the existing full UI/API, including mutating operations.
  There is no reduced remote role in v1.

Tailscale Serve identity headers are trustworthy only across the loopback proxy boundary.
Coldstore refuses Tailscale authentication on a network-visible listener because a direct
client could forge those headers. Other processes already running on the server remain
inside the loopback trust boundary.

## Never put these in the repository

Keep real values in the private production configuration or Tailscale admin console:

- either co-owner's Tailscale login;
- machine-share invitation URL;
- real `.ts.net` hostname, tailnet name, Tailscale IP or device identifier; and
- host-specific service or policy output.

The login allowlist is identity data, not a password, but it still does not belong in tracked
examples, issues or chat. Use placeholders in documentation.

## Current state

Owner-provided evidence on 2026-08-21 established:

- Tailscale 1.102.3 is installed on the server;
- initial `tailscale serve status` and `tailscale funnel status` showed no configuration;
- Coldstore was loopback-only and returned the expected Basic Auth `401` challenge;
- the owner reviewed the tailnet policy and restricted shared access to Coldstore HTTPS;
- Tailscale Serve HTTPS was enabled for the server and proxies the loopback listener; and
- the owner successfully reached the private URL and logged in with the existing Basic Auth
  credential.

The Serve transport is therefore operational. Basic Auth remains the production application
gate until the Tailscale identity implementation is merged, built, configured and restarted.
Do not remove the current credentials from a running old binary: it does not understand the
new identity mode.

## Roles

### Existing owner

- administers the server's Tailscale account and policy;
- keeps only the server machine shared;
- privately records the exact co-owner logins;
- deploys a build containing Tailscale identity authentication;
- updates the allowlist and restarts Coldstore through the normal deployment gate; and
- can revoke a machine share and remove one login independently.

### Second co-owner

- uses a separate Tailscale account;
- installs Tailscale on each approved personal device;
- accepts only the server-machine share;
- reaches Coldstore only through the private HTTPS Serve URL; and
- understands that their identity is authorized for the full mutating control plane.

## Phase 1 — verify the tailnet policy

Tailscale grants are additive. A narrow rule does not cancel a broader matching rule. In
**Access controls -> Policies -> General access rules**, preserve unrestricted access only
for the owner's identity and grant shared users TCP 443 to the server specifically.

The intended visual rule is:

```text
source       autogroup:shared
destination  the Coldstore server machine specifically
protocol     TCP
port         443
```

Do not put a real device identifier in this file. If JSON is used, choose the server's
Tailscale IP or a private host alias as `dst`; do not copy that value into the repository.

Review every other rule whose source is `*`, `autogroup:shared`, or a co-owner login. A broad
matching grant would add SSH, Syncthing or other port access back. Use the policy editor's
validation before saving.

## Phase 2 — verify the Serve transport

On the server, read the current state:

```bash
tailscale serve status
tailscale funnel status
curl --head --max-time 5 http://127.0.0.1:8090/
```

Before the new identity build is deployed, the expected loopback response is the existing
Basic Auth `401` with `WWW-Authenticate: Basic realm="coldstore"`. Serve status must show a
single private HTTPS mapping to `http://127.0.0.1:8090`; Funnel must show no configuration.

The reviewed Serve command is recorded for reconstruction, not repeated during a normal
authentication rollout:

```bash
tailscale serve --bg http://127.0.0.1:8090
```

Do not run `tailscale funnel`. Do not run `tailscale serve reset` when another Serve entry
could exist; reset clears the full Serve configuration.

## Phase 3 — configure Coldstore identity authentication

Only after installing a build containing this feature, update the private production
`[server]` configuration using exact login strings shown by Tailscale. It is safe to deploy
with only the existing owner first; add the co-owner only after their exact account login is
known:

```toml
[server]
listen_addr = "127.0.0.1:8090"
auth_mode = "tailscale"
tailscale_allowed_users = [
  "OWNER-TAILSCALE-LOGIN",
  # "COOWNER-TAILSCALE-LOGIN", # add after the account identity is confirmed
]
```

The match is exact. Do not guess, normalize case, substitute a display name, or use the
profile-picture/name headers. `Tailscale-User-Login` is the stable authorization input.

Remove both `COLDSTORE_USERNAME` and `COLDSTORE_PASSWORD` from the service environment as
part of the same reviewed change. Coldstore deliberately refuses to start when Tailscale
mode and Basic credentials coexist, including credentials inherited from the systemd
environment file. Keep the old password in the password manager temporarily for rollback;
do not print the service environment or credential file.

Before restarting, run the candidate binary's normal preflight against the production
configuration. Its authentication line must say:

```text
auth             Tailscale Serve identity allowlist (N users)
```

`N` is one for an owner-only rollout and two after co-owner onboarding. The report prints
only the count, not the private logins. Review every other preflight gate,
then perform the owner-approved service restart. A failed restart is a stop condition; do
not alter Serve or widen the listener to compensate.

## Phase 4 — verify the owner before sharing

From an already authorized owner device:

1. Connect Tailscale.
2. Open the private `.ts.net` HTTPS URL.
3. Confirm Coldstore loads without a Basic Auth prompt.
4. Exercise read-only listing, preview, download and Range-request paths.
5. Perform a mutation only if it is an ordinary intended action; do not archive or restore a
   file solely as an access test.
6. Inspect the Coldstore journal after an intended mutation and confirm the audit record
   contains `auth_provider=tailscale` and the correct `actor`, without credentials.

Then verify the backend itself fails closed:

```bash
curl --head --max-time 5 http://127.0.0.1:8090/
```

It must return `401 Unauthorized` without `WWW-Authenticate`; a direct loopback request has
not passed through Serve and has no authenticated identity. Do not manually supply or test a
forged identity header—the loopback host is the declared proxy trust boundary.

## Phase 5 — share only the server machine

In the Tailscale admin console:

1. Open the server in **Machines**.
2. Choose the machine-sharing action; do not invite the co-owner as a general tailnet member.
3. Create a share for the co-owner's separate Tailscale account.
4. Send the invitation through a private channel.
5. Revoke and recreate the invitation if its link reaches the wrong person or channel.
6. Wait for acceptance and verify the accepted account's exact login.
7. If that login was not already configured, add it to `tailscale_allowed_users`, run
   preflight and perform the owner-approved Coldstore restart before the co-owner tests the UI.

Machine sharing, the TCP 443 grant and Coldstore's allowlist are three cumulative gates. No
application password is sent to the co-owner.

## Phase 6 — co-owner device setup

On each approved iPhone, macOS, Linux or Windows device:

1. Install the current Tailscale client from the platform's normal trusted source.
2. Sign in with the co-owner's own Tailscale account.
3. Accept the server-machine share.
4. Confirm the shared server appears in Tailscale.
5. Open the exact private `.ts.net` HTTPS URL supplied by the owner.
6. Confirm Coldstore loads without an application login prompt.

Do not use `http://SERVER:8090`, a LAN address, a raw Tailscale IP with port 8090, or a
Funnel URL. The supported path is the private HTTPS Serve URL.

## Acceptance checks

- [ ] With Tailscale disconnected, the private Coldstore URL is unreachable.
- [ ] With Tailscale connected as each allowlisted co-owner, the URL loads without Basic Auth.
- [ ] Direct loopback access without Serve identity returns `401` and no Basic challenge.
- [ ] TCP 22, TCP 8090 and Syncthing remain unreachable to the shared user through Tailscale.
- [ ] The existing UI and read-only API work through HTTPS.
- [ ] Image/document preview, download and HTTP Range requests work through Serve.
- [ ] An intended mutation produces an audit record naming the correct Tailscale login.
- [ ] An unlisted shared identity receives the same generic `401` as a missing identity.
- [ ] The iPhone works on Wi-Fi and once over cellular when convenient.
- [ ] At least one desktop works away from the home LAN when convenient.
- [ ] A reboot or restart leaves the background Serve mapping unchanged.
- [ ] `tailscale funnel status` still shows no public configuration.

## Routine use

- Keep Tailscale connected when using Coldstore away from home.
- Use only the private HTTPS Serve URL.
- Treat a lost device, unexpected machine share or unexpected allowlist entry as a security
  event.
- Periodically review the machine share, exact-login allowlist, policy and Serve/Funnel status.

## Offboarding one co-owner

1. Revoke that co-owner's server-machine share immediately.
2. Confirm their devices can no longer reach the private URL.
3. Remove their exact login from `tailscale_allowed_users`.
4. Run preflight and perform the owner-approved Coldstore restart.
5. Confirm the remaining owner still loads Coldstore and that the reported allowlist count
   decreased.

No shared Coldstore password needs rotation. If Basic Auth was retained as rollback material,
it was never distributed for this identity-based onboarding.

## Disable or roll back

To remove only the Coldstore HTTPS listener while preserving other Serve state:

```bash
tailscale serve --https=443 off
tailscale serve status
```

Disabling Serve does not stop Coldstore or revoke a machine share. Revoke the share
separately. Do not use `tailscale serve reset` unless clearing every Serve entry is intended.

To roll the application back to Basic Auth, restore both Basic credentials through the
private service environment, remove `auth_mode = "tailscale"` and its allowlist, run
preflight, and restart through the normal deployment procedure. Never widen the listener or
enable Funnel as a rollback.

## References

- [Tailscale Serve and identity headers](https://tailscale.com/docs/features/tailscale-serve)
- [Tailscale Serve command](https://tailscale.com/docs/reference/tailscale-cli/serve)
- [Tailscale machine sharing](https://tailscale.com/docs/features/sharing)
- [Tailscale grants](https://tailscale.com/docs/features/access-control/grants)
- [Tailscale grant syntax](https://tailscale.com/docs/reference/syntax/grants)
